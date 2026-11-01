package context

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
