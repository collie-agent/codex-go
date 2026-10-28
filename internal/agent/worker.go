package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

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

// toolCallError 表示模型生成的调用不合法，可反馈纠正；请求和传输错误不属于此类。
type toolCallError struct{ err error }

func (e *toolCallError) Error() string { return e.err.Error() }
func (e *toolCallError) Unwrap() error { return e.err }

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
	client.SetLogLevel(w.agentConfig.LogLevel)
	// 输出协议和示例属于系统指令，状态正文始终位于工具结果之后。
	client.SetSystemPrompt(appendContextFewShot(request.SystemPrompt, modelContext, w.agentConfig.LLMConfig.OutputSchema))
	input := request.Input
	// 调用方提供的末尾上下文紧随正文。
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
	if w.agentConfig.LogLevel.InfoEnabled() {
		fmt.Fprintf(os.Stderr, "task %s: assistant output=%s\n", request.RunID, assistantOutput)
	}
	var toolParams []*mcpSdk.CallToolParams
	if len(toolCalls) > 0 {
		for _, call := range toolCalls {
			if _, ok := allowTools[config.McpToolName(call.Name)]; !ok {
				names := make([]string, 0, len(allowTools))
				for name := range allowTools {
					names = append(names, string(name))
				}
				sort.Strings(names)
				return result, "", nil, &toolCallError{err: fmt.Errorf("task %s: tool %s is not allowed (call_id=%q); allowed tools: [%s]; raw arguments: %s", request.RunID, call.Name, call.CallID, strings.Join(names, ", "), call.Arguments)}
			}

			var args map[string]any
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
				return result, "", nil, &toolCallError{err: fmt.Errorf("task %s: decode tool %s arguments (call_id=%q): %w; raw arguments: %s; supply a JSON object matching the tool schema",
					request.RunID, call.Name, call.CallID, err, call.Arguments)}
			}

			if args == nil {
				return result, "", nil, &toolCallError{err: fmt.Errorf("task %s: tool %s arguments (call_id=%q) must be a JSON object, not null; raw arguments: %s", request.RunID, call.Name, call.CallID, call.Arguments)}
			}
			toolParams = append(toolParams, &mcpSdk.CallToolParams{
				Meta:      mcpSdk.Meta{toolCallIDMetaKey: call.CallID},
				Name:      call.Name,
				Arguments: args,
			})
		}
	}
	// 状态更新与模式切换是控制流操作，不能与其他工具在同一批执行。
	for _, call := range toolParams {
		if (call.Name == string(config.SwitchToPlanExecuteMCPToolName) || call.Name == string(config.UpdateCompletionCriteriaMCPToolName)) && len(toolParams) != 1 {
			return result, "", nil, &toolCallError{err: fmt.Errorf("%s must be called alone", call.Name)}
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
