package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/collie-agent/codex-go/internal/agent"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/mcp"
	"gorm.io/gorm"
)

// interruptedHistory separates reference data from confirmed tool messages.
// Historical calls provide evidence only; they never restore scheduler state.
func interruptedHistory(ctx context.Context, store *database.RunStore, run database.AgentRun) (state.RunOutcome, error) {
	outcome := state.RunOutcome{RunID: run.RunID}
	type execution struct {
		ID          string              `json:"execution_id"`
		Result      json.RawMessage     `json:"result,omitempty"`
		Tools       []*state.ToolResult `json:"tool_interactions,omitempty"`
		ToolCallIDs []string            `json:"tool_call_ids,omitempty"`
	}
	// 未知结果只能保留为历史说明，不能构造已完成的工具返回。
	makeExecution := func(id string, result json.RawMessage, calls []*state.ToolResult) *execution {
		item := &execution{ID: id, Result: result}
		for _, call := range calls {
			if call == nil {
				continue
			}
			copy := *call
			if copy.Status == mcp.ToolCallSuceessStatus || copy.Status == mcp.ToolCallFailedStatus {
				item.ToolCallIDs = append(item.ToolCallIDs, copy.ToolCallId)
				outcome.ToolResults = append(outcome.ToolResults, &copy)
			} else {
				copy.Status = "outcome unknown"
				item.Tools = append(item.Tools, &copy)
			}
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
		database.StepExecution
		Execution *execution `json:"execution,omitempty"`
	}
	history := struct {
		RunID              string          `json:"run_id"`
		Goal               string          `json:"goal"`
		CompletionCriteria []string        `json:"completion_criteria"`
		Result             json.RawMessage `json:"result,omitempty"`
		Execution          *execution      `json:"execution,omitempty"`
		Steps              []step          `json:"steps,omitempty"`
		Delegations        any             `json:"delegations,omitempty"`
	}{RunID: run.RunID, Goal: run.Goal, CompletionCriteria: run.CompletionCriteria, Result: run.Result}
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
			exec, err := load(item.ExecutionID)
			if err != nil {
				return state.RunOutcome{}, err
			}
			history.Steps = append(history.Steps, step{StepExecution: item, Execution: exec})
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
	outcome.History = string(encoded)
	return outcome, nil
}
