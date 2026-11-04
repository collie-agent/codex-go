package entity

import "strings"

type TaskMessage struct {
	RunId  string
	Input  string
	TaskId string
	ConvId string
}

// ValidInput keeps HTTP admission and queue consumption on the same task contract.
// Whitespace-only values cannot select a mode or accompany the other mode.
func (m TaskMessage) ValidInput() bool {
	return (strings.TrimSpace(m.Input) != "" && m.RunId == "") ||
		(strings.TrimSpace(m.RunId) != "" && m.Input == "")
}

type LLMResponse struct {
	RunID                 string `json:"run_id"`
	AgentInstanceID       string `json:"agent_instance_id"`
	ParentAgentInstanceID string `json:"parent_agent_instance_id"`
	AgentName             string `json:"agent_name"`

	Type  string
	Delta string
	Err   error
}

// StreamEvent is one event read from a task response stream.
type StreamEvent struct {
	RunID                 string `json:"run_id"`
	AgentInstanceID       string `json:"agent_instance_id"`
	ParentAgentInstanceID string `json:"parent_agent_instance_id"`
	AgentName             string `json:"agent_name"`

	ID    string
	Type  string
	Delta string
}
