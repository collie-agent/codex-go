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
	// 首轮结果生成的标准也必须在运行失败时保留，恢复沿用同一验收目标。
	defer func() { durable.CompletionCriteria = slices.Clone(run.durable.CompletionCriteria) }()
	task := DelegatedTask{TaskID: run.nextID(), Worker: AgentID(graph.Entry), Goal: durable.Goal, CompletionCriteria: slices.Clone(durable.CompletionCriteria)}
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
}

func (r *multiAgentRun) nextID() string {
	return fmt.Sprintf("%s/delegation-%d", r.durable.RunId, r.ids.Add(1))
}

func (r *multiAgentRun) execute(task DelegatedTask, parent AgentID, upstream []DelegationReport) (MultiAgentResult, error) {
	var reports []DelegatedTask
	inherited := slices.Clone(upstream)
	for {
		result, err := r.invoke(task, reports, parent, inherited)
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
			// 在任何子任务启动前保存，后续 entry 调用只读取这份标准。
			task.CompletionCriteria = slices.Clone(result.EntryCompletionCriteria)
			r.durable.CompletionCriteria = slices.Clone(task.CompletionCriteria)
			if r.o.checkpoint != nil {
				if err := r.o.checkpoint(r.durable.Clone(), "multi-agent"); err != nil {
					return MultiAgentResult{}, err
				}
			}
		}
		// 业务失败终止当前委派，后续重试或改派由调用方决策。
		// 保留原始输出供 supervisor 检查，但不执行失败结果中的 Next。
		if result.Result.Status == FailedReactStatus {
			return result, nil
		}
		cfg := r.cfg.ForAgent(string(task.Worker))
		// 在启动整批任务前校验所有路由，防止部分无效委派仍产生副作用。
		for _, next := range result.Next {
			if !slices.Contains(cfg.NextAgents, string(next.Agent)) {
				return MultiAgentResult{}, fmt.Errorf("multi-agent: agent %q cannot delegate to %q", task.Worker, next.Agent)
			}
			if next.Agent != parent && (strings.TrimSpace(next.Task) == "" || !hasNonEmptyItems(next.CompletionCriteria)) {
				return MultiAgentResult{}, fmt.Errorf("multi-agent: delegation requires task and completion_criteria")
			}
		}
		if parent != "" && len(result.Next) == 1 && result.Next[0].Agent == parent {
			log.Printf("task %s: [multi-agent] switching from %s back to %s", task.TaskID, task.Worker, parent)
			return result, nil
		}
		if len(result.Next) == 0 {
			return result, nil
		}
		for _, next := range result.Next {
			if next.Agent == parent {
				return MultiAgentResult{}, fmt.Errorf("multi-agent: return to caller must be the only handoff")
			}
		}
		tasks := make([]DelegatedTask, len(result.Next))
		errs := make([]error, len(tasks))
		var wg sync.WaitGroup
		for i, handoff := range result.Next {
			tasks[i] = DelegatedTask{TaskID: r.nextID(), Worker: handoff.Agent, Goal: handoff.Task, CompletionCriteria: slices.Clone(handoff.CompletionCriteria), Context: handoff.Context}
			log.Printf("task %s: [multi-agent] switching from %s to %s (delegation %s)", task.TaskID, task.Worker, handoff.Agent, tasks[i].TaskID)
			wg.Go(func() {
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
		// 等待所有委派退出后再返回，结果顺序始终与委派声明顺序相同。
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return MultiAgentResult{}, err
		}
		reports = append(reports, tasks...)
		// 同一批并行任务只在全部完成后发布结果，后续委派继承扁平报告。
		for _, completed := range tasks {
			if !slices.ContainsFunc(upstream, func(report DelegationReport) bool { return report.TaskID == completed.TaskID }) {
				upstream = append(upstream, DelegationReport{TaskID: completed.TaskID, Worker: completed.Worker, Goal: completed.Goal, Result: completed.Result.Result})
			}
		}
		task.Result = &result
	}
}

func (r *multiAgentRun) invoke(task DelegatedTask, reports []DelegatedTask, parent AgentID, upstream []DelegationReport) (MultiAgentResult, error) {
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
	local := &state.DurableState{RunId: task.TaskID, Goal: task.Goal, CompletionCriteria: slices.Clone(task.CompletionCriteria)}
	// 只有入口读取会话历史；子任务另行继承已完成委派的结果报告。
	if parent == "" {
		local.PreviousRunOutcomes = slices.Clone(r.durable.PreviousRunOutcomes)
	}
	save := func() error {
		if r.o.checkpoint == nil {
			return nil
		}
		r.checkpointMu.Lock()
		defer r.checkpointMu.Unlock()
		// 根目标和运行身份不被子任务覆盖；恢复按原目标重新执行 multi-agent。
		return r.o.checkpoint(r.durable.Clone(), "multi-agent")
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
	return output, nil
}
