package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

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

// DelegationReport 仅传递已完成任务的结果与来源，避免递归携带路由和输入上下文。
type DelegationReport struct {
	TaskID string      `json:"task_id"`
	Worker AgentID     `json:"worker"`
	Goal   string      `json:"goal"`
	Result ReactResult `json:"result"`
}

// MultiAgentProcessorOrchestrator 使用组合复用 ReAct 和 MCP，调度权由配置图约束。
type MultiAgentProcessorOrchestrator struct {
	ctx        context.Context
	toolConfig filemcp.Config
	mcpCommand func() *exec.Cmd
	skills     []Skill
	checkpoint func(*state.DurableState, string) error
}

func NewMultiAgentProcessorOrchestrator(ctx context.Context, tools filemcp.Config) *MultiAgentProcessorOrchestrator {
	return &MultiAgentProcessorOrchestrator{ctx: ctx, toolConfig: tools}
}

func NewMultiAgentProcessorOrchestratorWithCommand(ctx context.Context, command string, args ...string) *MultiAgentProcessorOrchestrator {
	o := NewMultiAgentProcessorOrchestrator(ctx, filemcp.Config{})
	commandArgs := slices.Clone(args)
	o.mcpCommand = func() *exec.Cmd {
		return exec.CommandContext(ctx, command, commandArgs...)
	}
	return o
}

func (o *MultiAgentProcessorOrchestrator) SetSkills(skills []Skill) { o.skills = slices.Clone(skills) }
func (o *MultiAgentProcessorOrchestrator) SetCheckpoint(save func(*state.DurableState, string) error) {
	o.checkpoint = save
}

// Run 返回入口 agent 的最终结果。业务失败保留在结果中，运行错误通过 error 返回。
func (o *MultiAgentProcessorOrchestrator) Run(cfg *config.AgentConfig, durable *state.DurableState) ([]MultiAgentResult, error) {
	if o == nil || o.ctx == nil || cfg == nil || cfg.MultiAgent == nil || durable == nil || strings.TrimSpace(durable.Goal) == "" {
		return nil, fmt.Errorf("multi-agent: context, graph config and goal are required")
	}
	graph := *cfg.MultiAgent
	if graph.Entry == "" || graph.MaxRounds <= 0 || graph.MaxParallel <= 0 {
		return nil, fmt.Errorf("multi-agent: invalid execution limits or entry")
	}
	ctx, cancel := context.WithCancel(o.ctx)
	defer cancel()
	run := multiAgentRun{o: o, ctx: ctx, cancel: cancel, cfg: cfg, durable: durable.Clone(), graph: graph, slots: make(chan struct{}, graph.MaxParallel)}
	if len(durable.MultiAgentCheckpoint) > 0 {
		if err := json.Unmarshal(durable.MultiAgentCheckpoint, &run.graphState); err != nil {
			return nil, fmt.Errorf("multi-agent: decode checkpoint: %w", err)
		}
	}
	if run.graphState.Nodes == nil {
		run.graphState.Nodes = make(map[string]*delegationNode)
	}
	// 首轮结果生成的标准也必须在运行失败时保留，恢复沿用同一验收目标。
	defer func() { durable.CompletionCriteria = slices.Clone(run.durable.CompletionCriteria) }()
	task := DelegatedTask{TaskID: fmt.Sprintf("%s/delegation-1", durable.RunId), Worker: AgentID(graph.Entry), Goal: durable.Goal, CompletionCriteria: slices.Clone(durable.CompletionCriteria)}
	result, err := run.execute(task, "", nil)
	if err != nil {
		return nil, err
	}
	durable.Result = []MultiAgentResult{result}
	return []MultiAgentResult{result}, nil
}

type multiAgentRun struct {
	o            *MultiAgentProcessorOrchestrator
	ctx          context.Context
	cancel       context.CancelFunc
	cfg          *config.AgentConfig
	durable      *state.DurableState
	graph        config.MultiAgentConfig
	slots        chan struct{}
	ids          atomic.Int64
	turns        atomic.Int64
	checkpointMu sync.Mutex
	graphState   delegationGraph
	pending      map[*delegationTurn]*state.DurableState
}

// 图与每次独立调用的工具历史作为一个快照提交，确保调度不会越过已确认边界。
type delegationGraph = state.DelegationGraph[MultiAgentResult, DelegatedTask]
type delegationNode = state.DelegationNode[MultiAgentResult, DelegatedTask]
type delegationTurn = state.DelegationTurn[DelegatedTask]

func (r *multiAgentRun) saveLocked() error {
	// Persist only each turn's own state, not the conversation inherited at runtime.
	snapshot := delegationGraph{NextID: r.graphState.NextID, Nodes: make(map[string]*delegationNode, len(r.graphState.Nodes))}
	for id, node := range r.graphState.Nodes {
		copyNode := &delegationNode{Done: node.Done}
		for _, turn := range node.Turns {
			copyTurn := *turn
			copyTurn.Local = turn.Local.Clone()
			if copyTurn.Local != nil {
				copyTurn.Local.PreviousRunOutcomes = nil
			}
			copyNode.Turns = append(copyNode.Turns, &copyTurn)
		}
		snapshot.Nodes[id] = copyNode
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	r.durable.MultiAgentCheckpoint = raw
	if r.o.checkpoint != nil {
		return r.o.checkpoint(r.durable.Clone(), "multi-agent")
	}
	return nil
}
func (r *multiAgentRun) execute(task DelegatedTask, parent AgentID, upstream []DelegationReport) (MultiAgentResult, error) {
	r.checkpointMu.Lock()
	node := r.graphState.Nodes[task.TaskID]
	if node == nil {
		node = &delegationNode{}
		r.graphState.Nodes[task.TaskID] = node
		if r.graphState.NextID == 0 {
			r.graphState.NextID = 1
		}
	}
	if node.Done != nil {
		result := *node.Done
		r.checkpointMu.Unlock()
		return result, nil
	}
	r.checkpointMu.Unlock()
	var reports []DelegatedTask
	inherited := slices.Clone(upstream)
	for turnIndex := 0; ; turnIndex++ {
		r.checkpointMu.Lock()
		// 先保存 checkpoint，再执行这一轮
		// 恢复时不会进入if
		if turnIndex == len(node.Turns) {
			local := &state.DurableState{RunId: r.durable.RunId, Goal: task.Goal, CompletionCriteria: slices.Clone(task.CompletionCriteria), ExecutionID: fmt.Sprintf("%s/turn-%d", task.TaskID, turnIndex)}
			if parent == "" {
				local.PreviousRunOutcomes = slices.Clone(r.durable.PreviousRunOutcomes)
			}
			if parent == "" && turnIndex == 0 {
				local.ExecutionID = r.durable.ExecutionID
				if local.ExecutionID == "" {
					local.ExecutionID = r.durable.RunId
				}
				local.ExecutionResult = slices.Clone(r.durable.ExecutionResult)
				local.ToolExecutions = r.durable.Clone().ToolExecutions
			}
			node.Turns = append(node.Turns, &delegationTurn{Local: local})
			if err := r.saveLocked(); err != nil {
				r.checkpointMu.Unlock()
				return MultiAgentResult{}, err
			}
		}
		turn := node.Turns[turnIndex]
		r.checkpointMu.Unlock()
		result, err := r.invoke(task, reports, parent, inherited, turn)
		if err != nil {
			return MultiAgentResult{}, err
		}
		if parent == "" && !hasNonEmptyItems(task.CompletionCriteria) {
			if !hasNonEmptyItems(result.EntryCompletionCriteria) {
				return MultiAgentResult{}, fmt.Errorf("multi-agent: entry requires entry_completion_criteria on first response")
			}
			for _, criterion := range result.EntryCompletionCriteria {
				if strings.TrimSpace(criterion) == "" {
					return MultiAgentResult{}, fmt.Errorf("multi-agent: entry_completion_criteria contains blank criterion")
				}
			}
			task.CompletionCriteria = slices.Clone(result.EntryCompletionCriteria)
			r.checkpointMu.Lock()
			r.durable.CompletionCriteria = slices.Clone(task.CompletionCriteria)
			err = r.saveLocked()
			r.checkpointMu.Unlock()
			if err != nil {
				return MultiAgentResult{}, err
			}
		}
		if result.Result.Status != FailedReactStatus {
			cfg := r.cfg.ForAgent(string(task.Worker))
			for _, next := range result.Next {
				if !slices.Contains(cfg.NextAgents, string(next.Agent)) {
					return MultiAgentResult{}, fmt.Errorf("multi-agent: agent %q cannot delegate to %q", task.Worker, next.Agent)
				}
				if next.Agent != parent && (strings.TrimSpace(next.Task) == "" || !hasNonEmptyItems(next.CompletionCriteria)) {
					return MultiAgentResult{}, fmt.Errorf("multi-agent: delegation requires task and completion_criteria")
				}
			}
		}
		if result.Result.Status == FailedReactStatus || len(result.Next) == 0 || (parent != "" && len(result.Next) == 1 && result.Next[0].Agent == parent) {
			r.checkpointMu.Lock()
			r.publishTurnLocked(turn, parent, turnIndex)
			node.Done = &result
			err = r.saveLocked()
			r.checkpointMu.Unlock()
			if err == nil && parent != "" {
				log.Printf("task %s: [multi-agent] %s -> %s: report %s (status=%s)", r.durable.RunId, task.Worker, parent, task.TaskID, result.Result.Status)
			}
			return result, err
		}
		for _, next := range result.Next {
			if next.Agent == parent {
				return MultiAgentResult{}, fmt.Errorf("multi-agent: return to caller must be the only handoff")
			}
		}
		r.checkpointMu.Lock()
		if len(turn.Children) == 0 {
			r.publishTurnLocked(turn, parent, turnIndex)
			for _, handoff := range result.Next {
				r.graphState.NextID++
				turn.Children = append(turn.Children, DelegatedTask{TaskID: fmt.Sprintf("%s/delegation-%d", r.durable.RunId, r.graphState.NextID), Worker: handoff.Agent, Goal: handoff.Task, CompletionCriteria: slices.Clone(handoff.CompletionCriteria), Context: handoff.Context})
			}
			err = r.saveLocked()
		}
		tasks := slices.Clone(turn.Children)
		r.checkpointMu.Unlock()
		if err != nil {
			return MultiAgentResult{}, err
		}
		errs := make([]error, len(tasks))
		var wg sync.WaitGroup
		for i := range tasks {//对子agent并发
			wg.Go(func() {
				log.Printf("task %s: [multi-agent] %s -> %s: delegate %s", r.durable.RunId, task.Worker, tasks[i].Worker, tasks[i].TaskID)
				output, err := r.execute(tasks[i], task.Worker, slices.Clone(upstream))
				if err != nil {
					errs[i] = err
					r.cancel()
					return
				}
				tasks[i].Result = &output
				if output.Result.Status == FailedReactStatus {
					tasks[i].Failure = output.Result.Answer
				}
			})
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return MultiAgentResult{}, err
		}
		reports = append(reports, tasks...)
		for _, completed := range tasks {
			if !slices.ContainsFunc(upstream, func(report DelegationReport) bool { return report.TaskID == completed.TaskID }) {
				upstream = append(upstream, DelegationReport{TaskID: completed.TaskID, Worker: completed.Worker, Goal: completed.Goal, Result: completed.Result.Result})
			}
		}
		task.Result = &result
	}
}

// 输出和由它生成的委派必须一起发布，任何子任务启动前已完成提交。
func (r *multiAgentRun) publishTurnLocked(turn *delegationTurn, parent AgentID, index int) {
	local := r.pending[turn]
	if local == nil {
		return
	}
	turn.Local = local
	delete(r.pending, turn)
	if parent == "" && index == 0 {
		r.durable.ExecutionID = turn.Local.ExecutionID
		r.durable.ExecutionResult = slices.Clone(turn.Local.ExecutionResult)
		r.durable.ToolExecutions = turn.Local.Clone().ToolExecutions
	}
}

func (r *multiAgentRun) invoke(task DelegatedTask, reports []DelegatedTask, parent AgentID, upstream []DelegationReport, turn *delegationTurn) (MultiAgentResult, error) {
	select {
	case r.slots <- struct{}{}:
	case <-r.ctx.Done():
		return MultiAgentResult{}, r.ctx.Err()
	}
	defer func() { <-r.slots }()
	if err := r.ctx.Err(); err != nil {
		return MultiAgentResult{}, err
	}
	// max_rounds 限制整个执行链的 agent 调用总数，防止循环图无限调度。
	if r.turns.Add(1) > int64(r.graph.MaxRounds) {
		return MultiAgentResult{}, fmt.Errorf("multi-agent: max_rounds (%d) exhausted", r.graph.MaxRounds)
	}
	r.checkpointMu.Lock()
	local := turn.Local.Clone()
	// Conversation history is reconstructed on each run, never persisted in a turn.
	if parent == "" {
		local.PreviousRunOutcomes = slices.Clone(r.durable.PreviousRunOutcomes)
	}
	r.checkpointMu.Unlock()
	save := func() error {
		r.checkpointMu.Lock()
		defer r.checkpointMu.Unlock()
		// 最终输出留待调度决策与子委派一起提交。
		if len(local.ExecutionResult) > 0 && len(turn.Local.ExecutionResult) == 0 {
			return nil
		}
		turn.Local = local.Clone()
		if parent == "" && len(reports) == 0 {
			r.durable.ExecutionID = local.ExecutionID
			r.durable.ExecutionResult = slices.Clone(local.ExecutionResult)
			r.durable.ToolExecutions = local.Clone().ToolExecutions
		}
		return r.saveLocked()
	}
	if err := save(); err != nil {
		return MultiAgentResult{}, err
	}
	var process *MultiAgentReactProcess
	var err error
	if r.o.mcpCommand != nil {
		process, err = NewMultiAgentReactProcessWithCommand(r.ctx, r.o.mcpCommand())
	} else {
		process, err = NewMultiAgentReactProcess(r.ctx, r.o.toolConfig)
	}
	if err != nil {
		return MultiAgentResult{}, err
	}
	process.SetSkills(r.o.skills)
	process.SetCheckpoint(save)
	encoded, err := json.Marshal(struct {
		UpstreamReports []DelegationReport        `json:"upstream_reports"`
		Task            DelegatedTask             `json:"delegated_task"`
		Reports         []DelegatedTask           `json:"reports"`
		NextAgents      []string                  `json:"next_agents"`
		Caller          AgentID                   `json:"caller"`
		Agents          []config.AgentInstruction `json:"agents"`
	}{upstream, task, reports, r.cfg.ForAgent(string(task.Worker)).NextAgents, parent, r.cfg.AgentInstructions()})
	if err != nil {
		return MultiAgentResult{}, err
	}
	process.SetDelegationContext(string(encoded), parent == "" && !hasNonEmptyItems(task.CompletionCriteria))
	output, _, err := process.Run(r.cfg.ForAgent(string(task.Worker)), local)
	if err != nil {
		return MultiAgentResult{}, fmt.Errorf("multi-agent %s (%s): %w", task.Worker, task.TaskID, err)
	}
	r.checkpointMu.Lock()
	if r.pending == nil {
		r.pending = make(map[*delegationTurn]*state.DurableState)
	}
	r.pending[turn] = local.Clone()
	r.checkpointMu.Unlock()
	return output, nil
}
