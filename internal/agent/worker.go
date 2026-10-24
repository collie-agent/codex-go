package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
	mcpSdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
)

// call responses api + tool calling handling
type Worker[T LLMSchema] struct {
	ctx           context.Context
	agentConfig   *config.AgentConfig
	mcpClientSess *mcpSdk.ClientSession
}

func NewWorker[T LLMSchema](ctx context.Context, agentConfig *config.AgentConfig, mcpClientSess *mcpSdk.ClientSession) *Worker[T] {
	return &Worker[T]{
		ctx:           ctx,
		agentConfig:   agentConfig,
		mcpClientSess: mcpClientSess,
	}
}

func (w *Worker[T]) Run(modelContext fileContext.Contexter) (T, string, []*mcpSdk.CallToolParams, error) {
	var result T
	request := modelContext.ModelRequest()
	// 上下文禁用工具时，即使 worker 带有 MCP 会话和权限，也不向模型暴露工具。
	var allowTools map[config.McpToolName]struct{}
	var session *mcpSdk.ClientSession
	if request.AllowTools {
		allowTools = w.agentConfig.AllowTools
		session = w.mcpClientSess
	}
	client, err := provider.NewResponsesApiClient[T](w.agentConfig.LLMConfig, allowTools)
	if err != nil {
		return result, "", nil, err
	}
	client.SetContextLogLabel(request.LogLabel)
	client.SetSystemPrompt(request.SystemPrompt)
	input := appendContextFewShot(request.Input, modelContext, w.agentConfig.LLMConfig.OutputSchema)
	// 当前步骤验收标准必须位于格式约束之后。
	if request.TrailingContext != "" {
		input += "\n\n" + request.TrailingContext
	}
	var messages []provider.ToolMessage
	for _, tr := range request.ToolResults {
		content := tr.Result
		if tr.Error != "" {
			// 传输错误也必须回填给对应调用，不能被空结果掩盖。
			content += "\n\nError: " + tr.Error
		}
		messages = append(messages, provider.ToolMessage{
			ChatCompletionToolMessageParam: *openai.ToolMessage(content, tr.ToolCallId).OfTool,
			Name:                           tr.ToolName, Arguments: tr.Arguments,
		})
	}
	result, toolCalls, assistantOutput, err := client.RunWithToolMessages(w.ctx, request.RunID, input, session, messages)
	if err != nil {
		return result, "", nil, err
	}
	if !request.AllowTools && len(toolCalls) > 0 {
		var zero T
		return zero, "", nil, fmt.Errorf("task %s: %s returned unexpected tool calls", request.RunID, request.LogLabel)
	}
	// This is the assistant's visible response text for the ReAct turn, not hidden reasoning.
	fmt.Fprintf(os.Stderr, "task %s: assistant output=%s\n", request.RunID, assistantOutput)
	var toolParams []*mcpSdk.CallToolParams
	if len(toolCalls) > 0 {
		for _, call := range toolCalls {
			if _, ok := allowTools[config.McpToolName(call.Name)]; !ok {
				return result, "", nil, fmt.Errorf("task %s: tool %s is not allowed",
					request.RunID, call.Name)
			}

			var args map[string]any
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
				return result, "", nil, fmt.Errorf("task %s: decode tool %s arguments: %w",
					request.RunID, call.Name, err)
			}

			toolParams = append(toolParams, &mcpSdk.CallToolParams{
				Meta:      mcpSdk.Meta{toolCallIDMetaKey: call.CallID},
				Name:      call.Name,
				Arguments: args,
			})
		}
	}
	return result, assistantOutput, toolParams, nil
}

// appendContextFewShot 只通过接口选择格式约束，具体业务示例由各 Context 持有。
func appendContextFewShot(input string, modelContext fileContext.Contexter, schema config.LLMOutputSchema) string {
	switch schema {
	case config.JsonLLMOutputSchema:
		return input + "\n\n" + modelContext.JsonFewShot()
	case config.LineProtocolLLMOutputSchema:
		return input + "\n\n" + modelContext.LineProtocolFewShot()
	default:
		return input
	}
}
