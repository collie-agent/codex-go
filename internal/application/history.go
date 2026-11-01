package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/collie-agent/codex-go/internal/agent"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/mcp"
	"gorm.io/gorm"
)

// interruptedHistory renders prior execution evidence as Markdown reference data.
// Historical calls provide evidence only; they never restore scheduler state.
func interruptedHistory(ctx context.Context, store *database.RunStore, run database.AgentRun) (state.RunOutcome, error) {
	outcome := state.RunOutcome{RunID: run.RunID, Goal: run.Goal, Status: run.Status, Error: run.Error}
	type execution struct {
		ID     string              `json:"execution_id"`
		Result json.RawMessage     `json:"result,omitempty"`
		Tools  []*state.ToolResult `json:"tool_interactions,omitempty"`
	}
	// 未知结果只能保留为历史说明，不能构造已完成的工具返回。
	makeExecution := func(id string, result json.RawMessage, calls []*state.ToolResult) *execution {
		item := &execution{ID: id, Result: result}
		for _, call := range calls {
			if call == nil {
				continue
			}
			copy := *call
			if copy.Status != mcp.ToolCallSuceessStatus && copy.Status != mcp.ToolCallFailedStatus {
				copy.Status = "outcome unknown"
			}
			item.Tools = append(item.Tools, &copy)
		}
		return item
	}
	load := func(id string) (*execution, error) {
		if id == "" {
			return nil, nil
		}
		saved, err := store.ExecutionByID(ctx, run.RunID, id)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return makeExecution(id, saved.ExecutionResult, saved.ToolExecutions), nil
	}
	type step struct {
		StepID             string
		Intent             string
		Status             string
		CompletionCriteria string     `json:",omitempty"`
		Observation        []string   `json:",omitempty"`
		CompletionEvidence []string   `json:",omitempty"`
		Execution          *execution `json:"execution,omitempty"`
	}
	history := struct {
		CompletionCriteria []string        `json:"completion_criteria"`
		Answer             string          `json:"answer,omitempty"`
		CompletionEvidence []string        `json:"CompletionEvidence,omitempty"`
		Result             json.RawMessage `json:"result,omitempty"`
		Execution          *execution      `json:"execution,omitempty"`
		Steps              []step          `json:"steps,omitempty"`
		Delegations        any             `json:"delegations,omitempty"`
	}{CompletionCriteria: run.CompletionCriteria, Result: run.Result}
	if run.Mode == "multi-agent" && len(run.MultiAgentCheckpoint) > 0 {
		// Decode only run-owned graph data; inherited conversation history is excluded.
		var graph state.DelegationGraph[agent.MultiAgentResult, agent.DelegatedTask]
		if err := json.Unmarshal(run.MultiAgentCheckpoint, &graph); err != nil {
			return state.RunOutcome{}, err
		}
		type turnHistory struct {
			Goal               string                `json:"goal"`
			CompletionCriteria []string              `json:"completion_criteria"`
			Execution          *execution            `json:"execution,omitempty"`
			Children           []agent.DelegatedTask `json:"children,omitempty"`
		}
		type nodeHistory struct {
			Turns []turnHistory           `json:"turns"`
			Done  *agent.MultiAgentResult `json:"done,omitempty"`
		}
		nodes := make(map[string]nodeHistory, len(graph.Nodes))
		// Map iteration must not reorder historical delegation messages across requests.
		ids := make([]string, 0, len(graph.Nodes))
		for id := range graph.Nodes {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			node := graph.Nodes[id]
			historicalNode := nodeHistory{Done: node.Done}
			for _, turn := range node.Turns {
				historicalTurn := turnHistory{Children: turn.Children}
				if turn.Local != nil {
					historicalTurn.Goal = turn.Local.Goal
					historicalTurn.CompletionCriteria = turn.Local.CompletionCriteria
					historicalTurn.Execution = makeExecution(turn.Local.ExecutionID, turn.Local.ExecutionResult, turn.Local.ToolExecutions)
				}
				historicalNode.Turns = append(historicalNode.Turns, historicalTurn)
			}
			nodes[id] = historicalNode
		}
		history.Delegations = nodes
	} else if run.Mode == "plan-execute" {
		// 运行级只保留回答和完成证据，不再展开其中的观察和步骤历史。
		history.Result = nil
		if len(run.Result) > 0 {
			var result struct {
				Answer             string   `json:"answer"`
				CompletionEvidence []string `json:"CompletionEvidence"`
			}
			if err := json.Unmarshal(run.Result, &result); err != nil {
				return state.RunOutcome{}, fmt.Errorf("decode historical plan result: %w", err)
			}
			history.Answer = result.Answer
			history.CompletionEvidence = result.CompletionEvidence
		}
		// A ReAct run may have handed off to a plan; retain the original instance too.
		var err error
		history.Execution, err = load(run.RunID)
		if err != nil {
			return state.RunOutcome{}, err
		}
		steps, err := store.Steps(ctx, run.RunID)
		if err != nil {
			return state.RunOutcome{}, err
		}
		for _, item := range steps {
			// 未开始的计划只用于调度，不属于此前运行产生的执行历史。
			if item.Status == string(state.PendingExecutionStatus) && item.ExecutionID == "" {
				continue
			}
			historicalStep := step{StepID: item.StepID, Intent: item.Intent, Status: item.Status}
			if item.Status == string(state.CompletedExecutionStatus) {
				// 已完成步骤只保留观察和证据，避免加载实例结果重复展开摘要。
				historicalStep.Observation = item.Observation
				historicalStep.CompletionEvidence = item.CompletionEvidence
			} else {
				// 未完成实例保留验收标准及工具进度，供后续任务理解中断现场。
				exec, err := load(item.ExecutionID)
				if err != nil {
					return state.RunOutcome{}, err
				}
				historicalStep.CompletionCriteria = item.CompletionCriteria
				historicalStep.Execution = exec
				if exec != nil {
					exec.Result = nil
				}
			}
			history.Steps = append(history.Steps, historicalStep)
		}
	} else {
		var err error
		history.Execution, err = load(run.RunID)
		if err != nil {
			return state.RunOutcome{}, err
		}
	}
	encoded, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return state.RunOutcome{}, fmt.Errorf("encode history: %w", err)
	}
	// 将类型化历史转换为列表树；工具参数及结果仍保留原文，不解释为指令。
	var tree any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&tree); err != nil {
		return state.RunOutcome{}, err
	}
	var markdown strings.Builder
	writeHistoryMarkdown(&markdown, tree, 0)
	outcome.History = markdown.String()
	return outcome, nil
}

// writeHistoryMarkdown 保持集合顺序和稳定的字段顺序，多行叶子以引用块隔离。
func writeHistoryMarkdown(b *strings.Builder, value any, depth int) {
	indent := strings.Repeat("  ", depth)
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			// 字段名也可能来自委派节点 ID，转义换行以保持列表边界。
			keyLabel := strings.NewReplacer("\n", " ", "\r", " ").Replace(key)
			fmt.Fprintf(b, "%s- %s:\n", indent, keyLabel)
			// 工具结果在持久层是 JSON 字符串，解析外壳后再展示文本换行。
			// 只处理 Result 字段，不反复解码工具正文中的字面转义。
			if raw, ok := v[key].(string); ok && key == "Result" {
				writeHistoryMarkdown(b, state.FormatHistoricalResult(raw), depth+1)
			} else {
				writeHistoryMarkdown(b, v[key], depth+1)
			}
		}
	case []any:
		for i, item := range v {
			fmt.Fprintf(b, "%s- Item %d:\n", indent, i+1)
			writeHistoryMarkdown(b, item, depth+1)
		}
	default:
		fmt.Fprintln(b)
		for _, line := range strings.Split(fmt.Sprint(v), "\n") {
			fmt.Fprintf(b, "%s> %s\n", indent, line)
		}
		fmt.Fprintln(b)
	}
}
