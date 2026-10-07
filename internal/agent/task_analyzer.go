package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
)

type TaskAnalyzerResult struct {
	NeedsTools         bool     //是否需要工具
	ExpectedSteps      int      //预计的有效步骤数
	ReasoningSummary   string   //简短说明任务特征，不要求暴露思维链
	CompletionCriteria []string //仅对需要工具、步骤数大于 1 的任务提供；每项应简洁且可独立验证
}

type TaskAnalyzerProcess[T LLMSchema] struct {
	ctx context.Context
}

func NewTaskAnalyzerProcess[T LLMSchema](ctx context.Context) *TaskAnalyzerProcess[T] {
	return &TaskAnalyzerProcess[T]{ctx: ctx}
}

// Run 分析任务并返回执行特征；由调用方决定是否将完成标准写入持久状态。
func (p *TaskAnalyzerProcess[T]) Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (T, []AgentProcessorType, error) {
	// 失败时返回 T 的零值，避免暴露未完成的解析结果。
	var zero T
	if p == nil || p.ctx == nil {
		return zero, nil, fmt.Errorf("task analyzer: context is required")
	}
	if agentConfig == nil || agentConfig.LLMConfig == nil {
		return zero, nil, fmt.Errorf("task analyzer: agent config with LLM config is required")
	}
	if durableState == nil || strings.TrimSpace(durableState.Goal) == "" {
		return zero, nil, fmt.Errorf("task analyzer: durable state with goal is required")
	}

	taskID := durableState.RunId
	analyzerContext := fileContext.NewTaskAnalyzerContext(durableState)
	analyzerContext.SetSysPrompt(agentConfig.SysPrompt)
	// 分析器只评估提供的目标，不暴露或执行工具。
	client, err := provider.NewResponsesApiClient[T](agentConfig.LLMConfig, nil)
	if err != nil {
		return zero, nil, fmt.Errorf("task %s: task analyzer: %w", taskID, err)
	}
	result, toolCalls, _, err := client.Run(p.ctx, taskID, analyzerContext.Format(), nil)
	if err != nil {
		return zero, nil, fmt.Errorf("task %s: task analyzer: %w", taskID, err)
	}
	if len(toolCalls) > 0 {
		return zero, nil, fmt.Errorf("task %s: task analyzer returned unexpected tool calls", taskID)
	}
	// 仅对任务分析结果检查业务约束，其他输出类型沿用各自的协议解析。
	if analyzerResult, ok := any(result).(TaskAnalyzerResult); ok {
		// 协议解析只检查格式，这里检查会影响后续执行流程的业务约束。
		if analyzerResult.ExpectedSteps < 1 || strings.TrimSpace(analyzerResult.ReasoningSummary) == "" {
			return zero, nil, fmt.Errorf("task %s: task analyzer requires positive ExpectedSteps and non-empty ReasoningSummary", taskID)
		}
		if analyzerResult.NeedsTools && analyzerResult.ExpectedSteps > 1 {
			if len(analyzerResult.CompletionCriteria) == 0 {
				return zero, nil, fmt.Errorf("task %s: task analyzer requires CompletionCriteria for multi-step tool tasks", taskID)
			}
			for _, criterion := range analyzerResult.CompletionCriteria {
				if strings.TrimSpace(criterion) == "" {
					return zero, nil, fmt.Errorf("task %s: task analyzer returned an empty completion criterion", taskID)
				}
			}
		} else if len(analyzerResult.CompletionCriteria) > 0 {
			return zero, nil, fmt.Errorf("task %s: task analyzer returned CompletionCriteria for a task that does not require them", taskID)
		}
	}
	// DirectAgentType, ReactAgentType, PlanAgentType可选
	return result, []AgentProcessorType{ReactAgentType}, nil
}
