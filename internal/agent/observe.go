package agent

import (
	"context"
	"fmt"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
)

type ObserveResult struct {
	ConfirmedFacts []string
	ReportedErrors []string
}

type ObserveProcess struct {
	ctx                context.Context
	observationContext *fileContext.ObservationContext
}

func NewObserveProcess(ctx context.Context, observationContext *fileContext.ObservationContext) *ObserveProcess {
	return &ObserveProcess{ctx: ctx, observationContext: observationContext}
}

// Run extracts new facts and errors from the tool results. The caller decides
// when to merge the returned observation into the ReAct context.
func (o *ObserveProcess) Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (ObserveResult, error) {
	var result ObserveResult
	if o == nil || agentConfig == nil {
		return result, fmt.Errorf("observation: agent config is required")
	}
	if o.ctx == nil {
		return result, fmt.Errorf("observation: context is required")
	}
	if o.observationContext == nil || durableState == nil {
		return result, fmt.Errorf("observation: observation context with durable state is required")
	}
	// 用本次运行的状态替换快照中的状态，不修改调用方提供的观察上下文。
	observationContext := *o.observationContext
	observationContext.DurableState = durableState
	taskID := durableState.RunId
	client, err := provider.NewResponsesApiClient[ObserveResult](agentConfig.LLMConfig, nil)
	if err != nil {
		return result, fmt.Errorf("task %s: observation: %w", taskID, err)
	}
	// Observation only analyzes supplied data, so no MCP tools are exposed.
	result, toolCalls, _, err := client.Run(o.ctx, taskID, observationContext.Format(), nil)
	if err != nil {
		return ObserveResult{}, fmt.Errorf("task %s: observation: %w", taskID, err)
	}
	if len(toolCalls) > 0 {
		return ObserveResult{}, fmt.Errorf("task %s: observation returned unexpected tool calls", taskID)
	}
	return result, nil
}
