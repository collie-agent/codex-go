package context

type ReactStatus string

const (
	CompletedReactStatus ReactStatus = "completed"
	FailedReactStatus    ReactStatus = "failed"
)

type ReactResult struct {
	Status ReactStatus `json:"status" lineprotocol:"STATUS"`
	Answer string      `json:"answer" lineprotocol:"ANSWER"`
	// 计划步骤完成时必填；普通 ReAct 模式使用空列表。
	Observation        []string `json:"Observation" lineprotocol:"OBSERVATION"`
	CompletionEvidence []string `json:"CompletionEvidence" lineprotocol:"COMPLETION_EVIDENCE"`
}

type AgentID string

type AgentHandoff struct {
	Agent              AgentID  `json:"agent" lineprotocol:"AGENT"`
	Task               string   `json:"task" lineprotocol:"TASK"`
	CompletionCriteria []string `json:"completion_criteria" lineprotocol:"COMPLETION_CRITERIA"`
	Context            string   `json:"context" lineprotocol:"CONTEXT"`
}

type MultiAgentResult struct {
	// 仅入口缺少验收标准的首轮响应填写；后续响应不能改写已保存标准。
	EntryCompletionCriteria []string       `json:"entry_completion_criteria,omitempty" lineprotocol:"ENTRY_COMPLETION_CRITERIA"`
	Result                  ReactResult    `json:"result" lineprotocol:"RESULT"`
	Next                    []AgentHandoff `json:"next" lineprotocol:"NEXT"`
}

// DelegatedTask 是一次委派的独立执行上下文；同名 worker 的多次任务也不共享历史。
type DelegatedTask struct {
	TaskID             string            `json:"task_id"`
	Worker             AgentID           `json:"worker"`
	Goal               string            `json:"goal"`
	CompletionCriteria []string          `json:"completion_criteria"`
	Context            string            `json:"context"`
	Result             *MultiAgentResult `json:"result"`
	Failure            string            `json:"failure"`
}

// DelegationGraph is the shared persisted scheduler contract. Result and Task
// stay typed for the scheduler/history reader; storage can treat both as JSON.
type DelegationGraph[Result, Task any] struct {
	NextID int
	Nodes  map[string]*DelegationNode[Result, Task]
}

type DelegationNode[Result, Task any] struct {
	Turns []*DelegationTurn[Task]
	Done  *Result
}

type DelegationTurn[Task any] struct {
	Local    *DurableState
	Children []Task
}

// Executions returns the run-owned instances that must commit with this graph.
func (g *DelegationGraph[Result, Task]) Executions() []*DurableState {
	var executions []*DurableState
	for _, node := range g.Nodes {
		for _, turn := range node.Turns {
			if turn.Local != nil {
				executions = append(executions, turn.Local)
			}
		}
	}
	return executions
}
