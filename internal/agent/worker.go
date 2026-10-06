package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/provider"
	mcpSdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Worker[T LLMSchema] struct {
	ctx           context.Context
	agentConfig   *config.AgentConfig
	mcpClientSess *mcpSdk.ClientSession
	toolConfig    filemcp.Config
}

func NewWorker[T LLMSchema](ctx context.Context, agentConfig *config.AgentConfig, mcpClientSess *mcpSdk.ClientSession) *Worker[T] {
	return &Worker[T]{
		ctx:           ctx,
		agentConfig:   agentConfig,
		mcpClientSess: mcpClientSess,
	}
}

func (w *Worker[T]) Run(reactContext *fileContext.ReactContext) (T, string, []*mcpSdk.CallToolParams, error) {
	var result T
	client, err := provider.NewResponsesApiClient[T](w.agentConfig.LLMConfig, w.toolConfig)
	if err != nil {
		return result, "", nil, err
	}
	result, toolCalls, reasoning, err := client.Run(w.ctx, reactContext.DurableState.RunId, reactContext.Format(), w.mcpClientSess)
	if err != nil {
		return result, "", nil, err
	}
	var toolParams []*mcpSdk.CallToolParams
	if len(toolCalls) > 0 {
		for _, call := range toolCalls {
			if _, ok := w.agentConfig.AllowTools[config.McpToolName(call.Name)]; !ok {
				return result, "", nil, fmt.Errorf("task %s: tool %s is not allowed",
					reactContext.DurableState.RunId, call.Name)
			}

			var args map[string]any
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
				return result, "", nil, fmt.Errorf("task %s: decode tool %s arguments: %w",
					reactContext.DurableState.RunId, call.Name, err)
			}

			toolParams = append(toolParams, &mcpSdk.CallToolParams{
				Meta:      mcpSdk.Meta{toolCallIDMetaKey: call.CallID},
				Name:      call.Name,
				Arguments: args,
			})
		}
	}
	return result, reasoning, toolParams, nil
}
