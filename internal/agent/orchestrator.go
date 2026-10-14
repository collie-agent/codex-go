package agent

import (
	"context"
	"errors"
	"fmt"
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
	toolConfig    filemcp.Config
	mcpCommand    func() *exec.Cmd
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
	return o.runBatches(agentConfig, durableState, o.runProcessor)
}

// processorResult 统一不同泛型处理器的结果，供批次结束后串行合并。
type processorResult struct {
	react    *ReactResult
	analysis *TaskAnalyzerResult
	next     []AgentProcessorType
}

type processorRunner func(AgentProcessorType, *config.AgentConfig, *fileContext.DurableState) (processorResult, error)

// processorBranch 保留分支关系，后继节点继承该路径上最近的 ReAct 结果。
// 使用树的叶节点收集结果，避免分支执行深度或结束时间改变返回顺序。
type processorBranch struct {
	agentType AgentProcessorType
	react     *ReactResult
	children  []*processorBranch
}

func (o *ProcessorOrchestrator) runBatches(agentConfig *config.AgentConfig, durableState *fileContext.DurableState, run processorRunner) ([]ReactResult, error) {
	root := &processorBranch{agentType: o.rootAgentType}
	batch := []*processorBranch{root}
	for len(batch) > 0 {
		if err := o.ctx.Err(); err != nil {
			return nil, err
		}
		// 先校验整批类型，避免同批中有不支持的节点时，其他节点已执行工具。
		for _, branch := range batch {
			agentType := branch.agentType
			switch agentType {
			case TaskAnalyzerAgentType, ReactAgentType, EndAgentType:
			default:
				return nil, fmt.Errorf("orchestrator: unsupported processor type %q", agentType)
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
			// 每个处理器读取独立快照；完成标准在整批结束后统一写回，避免并发读写。
			state := *durableState
			state.CompletionCriteria = slices.Clone(durableState.CompletionCriteria)
			wg.Go(func() {
				results[i], errs[i] = run(agentType, resolvedConfig, &state)
				if errs[i] != nil {
					errs[i] = fmt.Errorf("orchestrator: processor %q: %w", agentType, errs[i])
				}
			})
		}
		// 即使某个节点失败，也等待同批节点退出，确保返回后没有本批处理器仍在运行。
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		if err := o.ctx.Err(); err != nil {
			return nil, err
		}

		var next []*processorBranch
		// 按调度顺序合并，避免 goroutine 完成先后改变最终结果；重复类型保留为独立实例。
		for i, output := range results {
			branch := batch[i]
			if output.analysis != nil {
				durableState.CompletionCriteria = slices.Clone(output.analysis.CompletionCriteria)
			}
			if output.react != nil {
				branch.react = output.react
			}
			// 单后继延续当前分支，多后继分叉；end 和空后继都终止路径。
			for _, agentType := range output.next {
				child := &processorBranch{agentType: agentType, react: branch.react}
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
		analysis, next, err := process.Run(agentConfig, state)
		return processorResult{analysis: &analysis, next: next}, err
	case ReactAgentType:
		var process *ReactProcess[ReactResult]
		var err error
		if o.mcpCommand != nil {
			process, err = NewReactProcessWithCommand[ReactResult](o.ctx, o.mcpCommand())
		} else {
			process, err = NewReactProcess[ReactResult](o.ctx, o.toolConfig)
		}
		if err != nil {
			return processorResult{}, fmt.Errorf("create processor %q: %w", agentType, err)
		}
		result, next, err := process.Run(agentConfig, state)
		return processorResult{react: &result, next: next}, err
	default:
		return processorResult{}, fmt.Errorf("unsupported processor type %q", agentType)
	}
}
