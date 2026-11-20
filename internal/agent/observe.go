package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
)

type ObserveResult struct {
	// 无决策相关的新信息时不对外输出空字段；LLM 的严格 JSON schema 仍要求返回空数组。
	ConfirmedFacts []string `json:"ConfirmedFacts,omitempty"`
	ReportedErrors []string `json:"ReportedErrors,omitempty"`
}

type ObserveProcess[T LLMSchema] struct {
	ctx                context.Context
	observationContext *fileContext.ObservationContext
}

func NewObserveProcess[T LLMSchema](ctx context.Context, observationContext *fileContext.ObservationContext) *ObserveProcess[T] {
	return &ObserveProcess[T]{ctx: ctx, observationContext: observationContext}
}

// Run extracts new facts and errors from the tool results. The caller decides
// when to merge the returned observation into the ReAct context.
func (o *ObserveProcess[T]) Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (T, []AgentProcessorType, error) {
	// 失败时返回 T 的零值，避免暴露未完成的解析结果。
	var zero T
	if o == nil || agentConfig == nil {
		return zero, nil, fmt.Errorf("observation: agent config is required")
	}
	if o.ctx == nil {
		return zero, nil, fmt.Errorf("observation: context is required")
	}
	if o.observationContext == nil || durableState == nil {
		return zero, nil, fmt.Errorf("observation: observation context with durable state is required")
	}
	// 用本次运行的状态替换快照中的状态，不修改调用方提供的观察上下文。
	observationContext := *o.observationContext
	observationContext.DurableState = durableState
	taskID := durableState.RunId
	client, err := provider.NewResponsesApiClient[T](agentConfig.LLMConfig, nil)
	if err != nil {
		return zero, nil, fmt.Errorf("task %s: observation: %w", taskID, err)
	}
	// Observation only analyzes supplied data, so no MCP tools are exposed.
	client.SetContextLogLabel("observe")
	client.SetLogLevel(agentConfig.LogLevel)
	// 观察器的输出只用于本地诊断，不写入任务的 Redis Stream。
	request := observationContext.ModelRequestWithSysPrompt(agentConfig.SysPrompt)
	systemPrompt := appendContextFewShot(request.SystemPrompt, &observationContext, agentConfig.LLMConfig.OutputSchema)
	client.SetSystemPrompt(systemPrompt)
	input := request.Input
	var corrections []string
	for formatErrors := 0; ; {
		if err := o.ctx.Err(); err != nil {
			return zero, nil, fmt.Errorf("task %s: observation: %w", taskID, err)
		}
		result, toolCalls, _, err := client.Run(o.ctx, taskID, input, nil)
		if err != nil {
			var parseErr *provider.OutputParseError
			if !errors.As(err, &parseErr) {
				return zero, nil, fmt.Errorf("task %s: observation: %w", taskID, err)
			}
			formatErrors++
			if formatErrors > maxOutputFormatErrors {
				return zero, nil, fmt.Errorf("task %s: observation: output format errors exceeded limit (%d): %w", taskID, maxOutputFormatErrors, err)
			}
			// 纠错独立于系统指令和观察快照，后续重试保留此前的纠错消息。
			corrections = append(corrections, outputFormatCorrection(taskID, "observe", formatErrors, err, parseErr))
			client.SetCorrectionMessages(corrections)
			continue
		}
		if len(toolCalls) > 0 {
			return zero, nil, fmt.Errorf("task %s: observation returned unexpected tool calls", taskID)
		}
		return result, []AgentProcessorType{EndAgentType}, nil
	}
}

func (r *BaseReactProcess[T]) observeExec(toolResult *fileContext.ToolResult, rc *fileContext.ReactContext, agentConfig *config.AgentConfig) {
	if toolResult == nil || toolResult.ToolName != string(config.ExecMCPToolName) ||
		agentConfig == nil || agentConfig.Global == nil || !agentConfig.Global.Observe || r.ctx.Err() != nil {
		return
	}
	// 同批工具可能仍在更新持久状态，先在执行锁内复制快照。
	r.executionMu.Lock()
	observationContext := rc.SnapshotObservationContext(toolResult)
	r.executionMu.Unlock()
	observerConfig := agentConfig.ForAgent("observe")
	go func() {
		observer := NewObserveProcess[ObserveResult](r.ctx, observationContext)
		result, _, err := observer.Run(observerConfig, observationContext.DurableState)
		if r.ctx.Err() != nil {
			return
		}
		if err != nil {
			tr := observationContext.ToolResult
			rc.AppendReportedErrors(fmt.Sprintf("tool %q (call_id=%q): observation failed: %v", tr.ToolName, tr.ToolCallId, err))
			return
		}
		for _, fact := range result.ConfirmedFacts {
			rc.AppendConfirmedFacts(fact)
		}
		for _, reportedError := range result.ReportedErrors {
			rc.AppendReportedErrors(reportedError)
		}
	}()
}
