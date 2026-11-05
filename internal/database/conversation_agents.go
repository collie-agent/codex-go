package database

import (
	"encoding/json"
	"fmt"

	state "github.com/collie-agent/codex-go/internal/context"
)

// ConversationAgent 是客户端展示的参与实例，不暴露调度配置及恢复上下文。
type ConversationAgent struct {
	AgentInstanceID       string                  `json:"agent_instance_id"`
	ParentAgentInstanceID string                  `json:"parent_agent_instance_id"`
	AgentName             string                  `json:"agent_name"`
	Goal                  string                  `json:"goal"`
	CompletionCriteria    []string                `json:"completion_criteria"`
	Status                string                  `json:"status"`
	Error                 string                  `json:"error"`
	Result                *state.ReactResult      `json:"result"`
	ToolCalls             []ConversationToolCall  `json:"tool_calls"`
	Turns                 []ConversationAgentTurn `json:"turns"`
}

type ConversationAgentTurn struct {
	ExecutionID string                 `json:"execution_id"`
	Error       string                 `json:"error"`
	Result      *state.ReactResult     `json:"result"`
	ToolCalls   []ConversationToolCall `json:"tool_calls"`
}

func conversationAgents(run AgentRun) ([]ConversationAgent, error) {
	agents := make([]ConversationAgent, 0)
	if run.Mode != "multi-agent" || len(run.MultiAgentCheckpoint) == 0 {
		return agents, nil
	}
	var graph state.DelegationGraph[state.MultiAgentResult, state.DelegatedTask]
	if err := json.Unmarshal(run.MultiAgentCheckpoint, &graph); err != nil {
		return nil, fmt.Errorf("decode conversation agent history: %w", err)
	}
	// 沿委派顺序遍历，主代理在前；同名委派按参与身份分别保留。
	var visit func(state.DelegatedTask, string) error
	visit = func(task state.DelegatedTask, parent string) error {
		item := ConversationAgent{AgentInstanceID: task.TaskID, ParentAgentInstanceID: parent, AgentName: string(task.Worker), Goal: task.Goal, CompletionCriteria: task.CompletionCriteria, Status: "unknown", ToolCalls: []ConversationToolCall{}, Turns: []ConversationAgentTurn{}}
		node := graph.Nodes[task.TaskID]
		if node != nil {
			if node.AgentName != "" {
				item.AgentName = node.AgentName
			}
			if node.Done != nil {
				item.Result = &node.Done.Result
				item.Status = string(node.Done.Result.Status)
			}
			for _, turn := range node.Turns {
				if turn == nil || turn.Local == nil {
					continue
				}
				local := turn.Local
				item.Error = local.Error
				if len(item.CompletionCriteria) == 0 {
					item.CompletionCriteria = local.CompletionCriteria
				}
				history := ConversationAgentTurn{ExecutionID: local.ExecutionID, Error: local.Error, ToolCalls: []ConversationToolCall{}}
				if len(local.ExecutionResult) > 0 {
					var result state.MultiAgentResult
					if err := json.Unmarshal(local.ExecutionResult, &result); err != nil {
						return fmt.Errorf("decode conversation agent turn: %w", err)
					}
					history.Result = &result.Result
				}
				item.Turns = append(item.Turns, history)
			}
		}
		agents = append(agents, item)
		if node != nil {
			for _, turn := range node.Turns {
				if turn == nil {
					continue
				}
				for _, child := range turn.Children {
					if err := visit(child, task.TaskID); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	root := state.DelegatedTask{TaskID: run.RunID + "/delegation-1", Goal: run.Goal, CompletionCriteria: run.CompletionCriteria}
	if err := visit(root, ""); err != nil {
		return nil, err
	}
	return agents, nil
}
