package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
	mcpSdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
)

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

func (w *Worker[T]) Run(reactContext *fileContext.ReactContext) (T, string, []*mcpSdk.CallToolParams, error) {
	var result T
	client, err := provider.NewResponsesApiClient[T](w.agentConfig.LLMConfig, w.agentConfig.AllowTools)
	if err != nil {
		return result, "", nil, err
	}
	input, toolResults := reactContext.FormatForModel()
	var messages []provider.ToolMessage
	for _, tr := range toolResults {
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
	// 将目标、计划和当前步骤验收标准整体移到协议说明之后，保持相邻顺序。
	criteria := reactContext.CurrentStepContext()
	if criteria != "" {
		input = strings.TrimSpace(strings.TrimSuffix(input, criteria))
	}
	if _, ok := any(result).(ReactResult); ok {
		// Go 字段标签不会自动进入模型请求，显式提供业务结构和所选格式的示例。
		input += "\n\n# Required result schema\n\n" + reactResultInstructions
		switch w.agentConfig.LLMConfig.OutputSchema {
		case config.JsonLLMOutputSchema:
			input += "\nJSON example: {\"status\":\"completed\",\"answer\":\"Task completed.\"}"
		case config.LineProtocolLLMOutputSchema:
			input += "\nLine Protocol example:\nBEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"Task completed.\"\nEND RESULT"
		}
	}
	result, toolCalls, assistantOutput, err := client.RunWithToolMessages(w.ctx, reactContext.DurableState.RunId, input, w.mcpClientSess, messages, criteria)
	if err != nil {
		return result, "", nil, err
	}
	// This is the assistant's visible response text for the ReAct turn, not hidden reasoning.
	if _, ok := any(result).(ReactResult); ok {
		fmt.Printf("task %s: assistant output=%s\n", reactContext.DurableState.RunId, assistantOutput)
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
	return result, assistantOutput, toolParams, nil
}

const reactResultInstructions = `When no tools are called, return a result with exactly these required string fields:
- status: "completed" only when the task is complete; "failed" when blocked or unable to continue. No other status is valid.
- answer: a non-empty final answer for "completed", or a concrete failure reason for "failed".
For JSON use the field names status and answer. For Line Protocol use root RESULT and fields STATUS and ANSWER.
When calling tools, use the supplied function tools; do not emit a final result until the task completes or fails.`
