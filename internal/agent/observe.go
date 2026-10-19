package agent

import (
	"context"
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
	input := observationContext.FormatWithSysPrompt(agentConfig.SysPrompt)
	result, toolCalls, _, err := client.Run(o.ctx, taskID, input, nil)
	if err != nil {
		return zero, nil, fmt.Errorf("task %s: observation: %w", taskID, err)
	}
	if len(toolCalls) > 0 {
		return zero, nil, fmt.Errorf("task %s: observation returned unexpected tool calls", taskID)
	}
	return result, []AgentProcessorType{EndAgentType}, nil
}
