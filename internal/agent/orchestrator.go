package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

type AgentProcessorType string

const (
	DirectAgentType       AgentProcessorType = "direct"
	ReactAgentType        AgentProcessorType = "react"
	PlanAgentType         AgentProcessorType = "plan"
	EndAgentType          AgentProcessorType = "end"
	TaskAnalyzerAgentType AgentProcessorType = "task_analyzer"
)

type ProcessorOrchestrator struct {
	ctx           context.Context
	rootAgentType AgentProcessorType
	executionMode AgentProcessorType
	toolConfig    filemcp.Config
	mcpCommand    func() *exec.Cmd
	checkpoint    func(*fileContext.DurableState, string) error
}

// NewProcessorOrchestrator 从任务分析器开始，后续节点由各处理器的返回值决定。
func NewProcessorOrchestrator(ctx context.Context, toolConfig filemcp.Config) *ProcessorOrchestrator {
	return &ProcessorOrchestrator{
		ctx:           ctx,
		toolConfig:    toolConfig,
		rootAgentType: TaskAnalyzerAgentType,
	}
}

// NewProcessorOrchestratorWithCommand 从任务分析器开始，ReAct 通过子进程连接 MCP。
func NewProcessorOrchestratorWithCommand(ctx context.Context, command string, args ...string) *ProcessorOrchestrator {
	o := NewProcessorOrchestrator(ctx, filemcp.Config{})
	commandArgs := slices.Clone(args)
	// 每个 ReAct 节点创建独立命令，避免并发节点重复启动同一个 exec.Cmd。
	o.mcpCommand = func() *exec.Cmd {
		return exec.CommandContext(ctx, command, commandArgs...)
	}
	return o
}

// SetMode 显式指定模式时跳过任务分析器；空值恢复默认的分析流程。
func (o *ProcessorOrchestrator) SetMode(mode string) error {
	switch mode {
	case "":
		o.rootAgentType = TaskAnalyzerAgentType
	case "react":
		o.rootAgentType = ReactAgentType
	case "plan-execute":
		o.rootAgentType = PlanAgentType
	default:
		return fmt.Errorf("unsupported mode %q: expected react or plan-execute", mode)
	}
	return nil
}

// SetCheckpoint 设置执行边界的持久化操作；失败时不再调度后续处理器。
func (o *ProcessorOrchestrator) SetCheckpoint(save func(*fileContext.DurableState, string) error) {
	o.checkpoint = save
}

func (o *ProcessorOrchestrator) saveCheckpoint(state *fileContext.DurableState) error {
	if o.checkpoint == nil {
		return nil
	}
	mode := "react"
	if o.executionMode == PlanAgentType {
		mode = "plan-execute"
	}
	return o.checkpoint(state.Clone(), mode)
}

// Run 按批次执行动态处理流程，按分支顺序返回每个终止分支的最后一个 ReAct 结果。
// 未执行 ReAct 的分支不产生结果；任何节点失败时返回 nil 和错误。
func (o *ProcessorOrchestrator) Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) ([]ReactResult, error) {
	var zero []ReactResult
	if o == nil || o.ctx == nil {
		return zero, fmt.Errorf("orchestrator: context is required")
	}
	if err := o.ctx.Err(); err != nil {
		return zero, err
	}
	if agentConfig == nil || agentConfig.LLMConfig == nil {
		return zero, fmt.Errorf("orchestrator: agent config with LLM config is required")
	}
	if durableState == nil || strings.TrimSpace(durableState.Goal) == "" {
		return zero, fmt.Errorf("orchestrator: durable state with goal is required")
	}
	// 每次运行使用独立的模式设置，避免复用 orchestrator 时串扰。
	runner := *o
	runner.executionMode = ReactAgentType
	// 显式 ReAct 模式优先于 YAML 的 name，不能误入计划执行流程。
	if o.rootAgentType == PlanAgentType || (o.rootAgentType == TaskAnalyzerAgentType && agentConfig.Name == "plan-execute") {
		runner.executionMode = PlanAgentType
	}
	return runner.runBatches(agentConfig, durableState, runner.runProcessor)
}

// processorResult 统一不同泛型处理器的结果，供批次结束后串行合并。
type processorResult struct {
	react    *ReactResult
	analysis *TaskAnalyzerResult
	plan     *PlannerResult
	state    *fileContext.DurableState
	next     []AgentProcessorType
}

type processorRunner func(AgentProcessorType, *config.AgentConfig, *fileContext.DurableState) (processorResult, error)

// processorBranch 保留分支关系，后继节点继承该路径上最近的 ReAct 结果。
// 使用树的叶节点收集结果，避免分支执行深度或结束时间改变返回顺序。
type processorBranch struct {
	agentType AgentProcessorType
	react     *ReactResult
	children  []*processorBranch
	state     *fileContext.DurableState
}

func (o *ProcessorOrchestrator) runBatches(agentConfig *config.AgentConfig, durableState *fileContext.DurableState, run processorRunner) ([]ReactResult, error) {
	root := &processorBranch{agentType: o.rootAgentType, state: durableState.Clone()}
	batch := []*processorBranch{root}
	for len(batch) > 0 {
		if err := o.ctx.Err(); err != nil {
			return nil, err
		}
		// 先校验整批类型，避免同批中有不支持的节点时，其他节点已执行工具。
		for _, branch := range batch {
			agentType := branch.agentType
			switch agentType {
			case TaskAnalyzerAgentType, ReactAgentType, PlanAgentType, EndAgentType:
			default:
				return nil, fmt.Errorf("orchestrator: unsupported processor type %q", agentType)
			}
		}

		// 整批 checkpoint 全部成功后才启动处理器，写入失败不能放行其他分支。
		for _, branch := range batch {
			if branch.agentType != EndAgentType {
				if err := o.saveCheckpoint(branch.state); err != nil {
					return nil, err
				}
			}
		}

		results := make([]processorResult, len(batch))
		errs := make([]error, len(batch))
		var wg sync.WaitGroup
		for i, branch := range batch {
			agentType := branch.agentType
			// end 只终止当前分支，不影响同一批次的其他分支。
			if agentType == EndAgentType {
				continue
			}
			resolvedConfig := agentConfig.ForAgent(string(agentType))
			// 分支继承自己的执行记录，每个节点读取深拷贝，批次结束后再写回。
			state := branch.state.Clone()
			wg.Go(func() {
				results[i], errs[i] = run(agentType, resolvedConfig, state)
				if errs[i] != nil {
					errs[i] = fmt.Errorf("orchestrator: processor %q: %w", agentType, errs[i])
				}
			})
		}
		// 即使某个节点失败，也等待同批节点退出，确保返回后没有本批处理器仍在运行。
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			// executor 失败仍保留其步骤状态，不能丢失已发生的执行记录。
			for _, output := range results {
				if output.state != nil {
					*durableState = *output.state.Clone()
				}
			}
			return nil, err
		}
		if err := o.ctx.Err(); err != nil {
			return nil, err
		}

		var next []*processorBranch
		// 按调度顺序合并，避免 goroutine 完成先后改变最终结果；重复类型保留为独立实例。
		for i, output := range results {
			branch := batch[i]
			if output.state != nil {
				branch.state = output.state.Clone()
			}
			if output.analysis != nil {
				branch.state.CompletionCriteria = slices.Clone(output.analysis.CompletionCriteria)
			}
			if output.plan != nil {
				branch.state.StepExecutions = output.plan.StepExecutions
				branch.state = branch.state.Clone()
			}
			if branch.agentType != EndAgentType {
				*durableState = *branch.state.Clone()
			}
			if output.react != nil {
				branch.react = output.react
			}
			// 单后继延续当前分支，多后继分叉；end 和空后继都终止路径。
			for _, agentType := range output.next {
				child := &processorBranch{agentType: agentType, react: branch.react, state: branch.state.Clone()}
				branch.children = append(branch.children, child)
				next = append(next, child)
			}
		}
		batch = next
	}
	var finalResults []ReactResult
	// 按后继声明顺序遍历叶节点；祖先的中间结果不会单独返回。
	pending := []*processorBranch{root}
	for len(pending) > 0 {
		branch := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if len(branch.children) == 0 {
			if branch.react != nil {
				finalResults = append(finalResults, *branch.react)
			}
			continue
		}
		for i := len(branch.children) - 1; i >= 0; i-- {
			pending = append(pending, branch.children[i])
		}
	}
	return finalResults, nil
}

func (o *ProcessorOrchestrator) runProcessor(agentType AgentProcessorType, agentConfig *config.AgentConfig, state *fileContext.DurableState) (processorResult, error) {
	switch agentType {
	case TaskAnalyzerAgentType:
		process := NewTaskAnalyzerProcess[TaskAnalyzerResult](o.ctx)
		process.Workspace = o.toolConfig.Workspace
		analysis, next, err := process.Run(agentConfig, state)
		if err == nil && o.executionMode == PlanAgentType {
			next = []AgentProcessorType{PlanAgentType}
		}
		if err == nil {
			mode := "react"
			if o.executionMode == PlanAgentType {
				mode = "plan-execute"
			}
			// 在模式覆盖完成后记录实际执行模式；error 级别也保留此调度信息。
			log.Printf("task %s: [task_analyzer] mode=%s", state.RunId, mode)
		}
		return processorResult{analysis: &analysis, next: next}, err
	case PlanAgentType:
		process := NewPlannerProcess(o.ctx)
		plan, next, err := process.Run(agentConfig, state)
		if err != nil {
			return processorResult{}, err
		}
		return processorResult{plan: &plan, state: state, next: next}, nil
	case ReactAgentType:
		planExecute := o.executionMode == PlanAgentType || o.rootAgentType == PlanAgentType
		var step *fileContext.StepExecution
		if planExecute {
			step = state.CurrentStep()
			if step == nil || step.Status != PendingExecutionStatus {
				return processorResult{}, fmt.Errorf("plan executor: no pending step at index %d", state.CurrentStepIndex)
			}
			step.Status = RunningExecutionStatus
			state.Error = ""
			// Persist the running boundary before constructing the executor or
			// making a model request, so recovery always has an explicit state.
			if err := o.saveCheckpoint(state); err != nil {
				return processorResult{state: state}, err
			}
		}
		var process *ReactProcess[ReactResult]
		var err error
		if o.mcpCommand != nil {
			process, err = NewReactProcessWithCommand[ReactResult](o.ctx, o.mcpCommand())
		} else {
			process, err = NewReactProcess[ReactResult](o.ctx, o.toolConfig)
		}
		if err != nil {
			if planExecute {
				step.Status = FailedExecutionStatus
				state.Error = err.Error()
				if checkpointErr := o.saveCheckpoint(state); checkpointErr != nil {
					return processorResult{state: state}, errors.Join(err, checkpointErr)
				}
				return processorResult{state: state}, fmt.Errorf("create processor %q: %w", agentType, err)
			}
			return processorResult{}, fmt.Errorf("create processor %q: %w", agentType, err)
		}
		process.SetCheckpoint(func() error { return o.saveCheckpoint(state) })
		result, _, err := process.Run(agentConfig, state)
		// ReAct 不负责选择运行模式；orchestrator 决定结束或回到 planner。
		next := []AgentProcessorType{EndAgentType}
		if planExecute {
			if err != nil {
				step.Status = FailedExecutionStatus
				state.Error = err.Error()
				if checkpointErr := o.saveCheckpoint(state); checkpointErr != nil {
					return processorResult{state: state}, errors.Join(err, checkpointErr)
				}
				return processorResult{state: state}, err
			}
			step.Status = CompletedExecutionStatus
			state.Error = ""
			// 直接保留 executor 的结构化结果，供后续步骤读取。
			step.Observation = slices.Clone(result.Observation)
			step.CompletionEvidence = slices.Clone(result.CompletionEvidence)
			// Persist the complete structured result together with the completed
			// step, allowing finalization to replay without another model call.
			state.Result = result
			if err := o.saveCheckpoint(state); err != nil {
				return processorResult{state: state}, err
			}
			next = []AgentProcessorType{PlanAgentType}
			return processorResult{react: &result, state: state, next: next}, nil
		}
		return processorResult{react: &result, next: next}, err
	default:
		return processorResult{}, fmt.Errorf("unsupported processor type %q", agentType)
	}
}
