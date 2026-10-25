package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
)

// 状态定义位于 context，避免持久状态与 agent 包循环依赖。
type ExecutionStatus = fileContext.ExecutionStatus
type StepExecution = fileContext.StepExecution

const (
	PendingExecutionStatus   = fileContext.PendingExecutionStatus
	RunningExecutionStatus   = fileContext.RunningExecutionStatus
	CompletedExecutionStatus = fileContext.CompletedExecutionStatus
	FailedExecutionStatus    = fileContext.FailedExecutionStatus
)

type PlannerResult struct {
	StepExecutions []StepExecution
}

type PlannerProcess struct {
	ctx context.Context
}

func NewPlannerProcess(ctx context.Context) *PlannerProcess {
	return &PlannerProcess{ctx: ctx}
}

// Run 根据 Goal 生成有序计划，后续由已有的 ReAct 处理器执行。
func (p *PlannerProcess) Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (PlannerResult, []AgentProcessorType, error) {
	if p == nil || p.ctx == nil {
		return PlannerResult{}, nil, fmt.Errorf("planner: context is required")
	}
	if agentConfig == nil || agentConfig.LLMConfig == nil {
		return PlannerResult{}, nil, fmt.Errorf("planner: agent config with LLM config is required")
	}
	if durableState == nil || strings.TrimSpace(durableState.Goal) == "" {
		return PlannerResult{}, nil, fmt.Errorf("planner: durable state with goal is required")
	}
	// 已生成的计划保留执行记录；后续轮次只选择剩余步骤，不重新生成初始计划。
	if len(durableState.StepExecutions) > 0 {
		stepIndex := -1
		for i, step := range durableState.StepExecutions {
			switch step.Status {
			case PendingExecutionStatus:
				if stepIndex < 0 {
					stepIndex = i
				}
			case CompletedExecutionStatus:
			default:
				return PlannerResult{}, nil, fmt.Errorf("planner: step %q has unexpected status %q", step.StepId, step.Status)
			}
		}
		// 校验成功后再发布选择结果，也为没有索引的旧持久计划恢复当前步骤。
		durableState.CurrentStepIndex = stepIndex
		if stepIndex >= 0 {
			return PlannerResult{StepExecutions: durableState.Clone().StepExecutions}, []AgentProcessorType{ReactAgentType}, nil
		}
		return PlannerResult{StepExecutions: durableState.Clone().StepExecutions}, []AgentProcessorType{EndAgentType}, nil
	}
	taskID := durableState.RunId
	plannerContext := fileContext.NewPlannerContext(durableState)
	plannerContext.SetSysPrompt(agentConfig.SysPrompt)
	// 直接使用 PlannerResult 作为两种输出协议的对象根节点。
	client, err := provider.NewResponsesApiClient[PlannerResult](agentConfig.LLMConfig, nil)
	if err != nil {
		return PlannerResult{}, nil, fmt.Errorf("task %s: planner: %w", taskID, err)
	}
	client.SetLogLevel(agentConfig.LogLevel)
	request := plannerContext.ModelRequest()
	client.SetSystemPrompt(appendContextFewShot(request.SystemPrompt, plannerContext, agentConfig.LLMConfig.OutputSchema))
	result, toolCalls, _, err := client.Run(p.ctx, taskID, request.Input, nil)
	if err != nil {
		return PlannerResult{}, nil, fmt.Errorf("task %s: planner: %w", taskID, err)
	}
	if len(toolCalls) > 0 {
		return PlannerResult{}, nil, fmt.Errorf("task %s: planner returned unexpected tool calls", taskID)
	}
	if len(result.StepExecutions) == 0 {
		return PlannerResult{}, nil, fmt.Errorf("task %s: planner requires at least one step", taskID)
	}
	ids := make(map[string]struct{}, len(result.StepExecutions))
	for i, step := range result.StepExecutions {
		id := strings.TrimSpace(step.StepId)
		if id == "" || strings.TrimSpace(step.Intent) == "" || step.Revision != 1 || step.Status != PendingExecutionStatus || len(step.CompletionEvidence) != 0 || len(step.Observation) != 0 {
			return PlannerResult{}, nil, fmt.Errorf("task %s: planner step %d requires non-empty StepId and Intent, Revision 1, pending Status and empty Observation and CompletionEvidence", taskID, i+1)
		}
		if _, exists := ids[id]; exists {
			return PlannerResult{}, nil, fmt.Errorf("task %s: planner returned duplicate StepId %q", taskID, id)
		}
		if strings.TrimSpace(step.CompletionCriteria) == "" {
			return PlannerResult{}, nil, fmt.Errorf("task %s: planner step %d requires non-empty CompletionCriteria", taskID, i+1)
		}
		ids[id] = struct{}{}
	}
	durableState.CurrentStepIndex = 0
	return result, []AgentProcessorType{ReactAgentType}, nil
}
