package agent

import (
	"context"
	"fmt"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/provider"
)

type ObserveResult struct {
	ConfirmedFacts []string
	ReportedErrors []string
}

type ObserveProcess struct {
	ctx         context.Context
	agentConfig *config.AgentConfig
}

func NewObserveProcess(ctx context.Context, agentConfig *config.AgentConfig) *ObserveProcess {
	return &ObserveProcess{ctx: ctx, agentConfig: agentConfig}
}

// Run extracts new facts and errors from the tool results. The caller decides
// when to merge the returned observation into the ReAct context.
func (o *ObserveProcess) Run(observationContext *fileContext.ObservationContext) (ObserveResult, error) {
	var result ObserveResult
	if o == nil || o.agentConfig == nil {
		return result, fmt.Errorf("observation: agent config is required")
	}
	if o.ctx == nil {
		return result, fmt.Errorf("observation: context is required")
	}
	if observationContext == nil || observationContext.DurableState == nil {
		return result, fmt.Errorf("observation: observation context with durable state is required")
	}
	taskID := observationContext.DurableState.RunId
	client, err := provider.NewResponsesApiClient[ObserveResult](o.agentConfig.LLMConfig, filemcp.Config{})
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
