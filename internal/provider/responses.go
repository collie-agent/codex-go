package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type responsesApiClient[T any] struct {
	client          *openai.Client
	agentConfig     *config.AgentConfig
	contextLogLabel string
}

// SetContextLogLabel 仅标记当前客户端的上下文日志，不修改任务 ID 或模型请求。
func (c *responsesApiClient[T]) SetContextLogLabel(label string) {
	c.contextLogLabel = label
}

// OutputParseError 区分可由模型纠正的输出格式错误和请求、配置等运行错误。
// Output 保留原始响应，便于下一次请求指出具体需要纠正的内容。
type OutputParseError struct {
	Output string
	Err    error
}

func (e *OutputParseError) Error() string { return "parse response output: " + e.Err.Error() }

func (e *OutputParseError) Unwrap() error { return e.Err }

// NewResponsesApiClient creates a client whose Run method returns T.
// allowTools 是 Agent 的工具权限；nil 或空 map 都不向模型提供工具。
func NewResponsesApiClient[T any](llmConfig *config.LLMConfig, allowTools map[config.McpToolName]struct{}) (*responsesApiClient[T], error) {
	if llmConfig == nil {
		return nil, fmt.Errorf("LLM config is required")
	}
	client := openai.NewClient(
		option.WithAPIKey(llmConfig.ApiKey),
		option.WithBaseURL(llmConfig.BaseUrl),
	)
	c := &responsesApiClient[T]{
		agentConfig: &config.AgentConfig{LLMConfig: llmConfig, AllowTools: allowTools},
		client:      &client,
	}
	return c, nil
}

// Run returns unparsed output text as reasoning when the response calls tools.
// Responses without tool calls are parsed into T.
// A nil MCP session sends a request without tools.
// 方法不能独立声明泛型参数，因此在创建客户端时指定结果类型
func (c *responsesApiClient[T]) Run(ctx context.Context, taskId, input string, mcpClientSess *sdk.ClientSession, trailingContext ...string) (T, []responses.ResponseFunctionToolCall, string, error) {
	return c.RunWithToolMessages(ctx, taskId, input, mcpClientSess, nil, trailingContext...)
}

// ToolMessage 保留 SDK 原生工具消息及调用信息，供无服务端历史的请求重放。
type ToolMessage struct {
	openai.ChatCompletionToolMessageParam
	Name      string
	Arguments string
}

func (c *responsesApiClient[T]) RunWithToolMessages(ctx context.Context, taskId, input string, mcpClientSess *sdk.ClientSession, messages []ToolMessage, trailingContext ...string) (T, []responses.ResponseFunctionToolCall, string, error) {
	var result T
	var outputParser LLMOutputParser[*T]
	var textConfig responses.ResponseTextConfigParam
	switch c.agentConfig.LLMConfig.OutputSchema {
	case config.JsonLLMOutputSchema:
		parser := &JsonOutput[*T]{}
		schema, err := parser.schema()
		if err != nil {
			return result, nil, "", fmt.Errorf("task %s: generate JSON output schema: %w", taskId, err)
		}
		// Print the exact schema attached to text.format for JSON output runs.
		if encodedSchema, err := json.MarshalIndent(schema, "", "  "); err == nil {
			fmt.Printf("task %s: JSON schema=\n%s\n", taskId, encodedSchema)
		}
		// Responses API 通过 text.format 约束最终文本，工具调用仍按原有流程处理。
		textConfig.Format.OfJSONSchema = &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name: "llm_output", Schema: schema, Strict: openai.Bool(true),
		}
		outputParser = parser
		input += "\n\n" + jsonFewShotPrompt
	case config.LineProtocolLLMOutputSchema:
		outputParser = &LineProtocolOutput[*T]{}
		input += "\n\n" + lineProtocolFewShotPrompt
	default:
		return result, nil, "", fmt.Errorf("task %s: unsupported output schema %q", taskId, c.agentConfig.LLMConfig.OutputSchema)
	}

	// 当前步骤的验收标准必须位于整个文本输入末尾，包括输出协议指令之后。
	for _, suffix := range trailingContext {
		if strings.TrimSpace(suffix) != "" {
			input += "\n\n" + suffix
		}
	}

	var tools []responses.ToolUnionParam
	if mcpClientSess != nil {
		for tool, err := range mcpClientSess.Tools(ctx, nil) {
			if err != nil {
				return result, nil, "", fmt.Errorf("task %s: list MCP tools: %w", taskId, err)
			}
			if _, ok := c.agentConfig.AllowTools[config.McpToolName(tool.Name)]; !ok {
				continue
			}
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil {
				return result, nil, "", fmt.Errorf("task %s: encode tool %s schema: %w", taskId, tool.Name, err)
			}
			var parameters map[string]any
			if err := json.Unmarshal(schema, &parameters); err != nil {
				return result, nil, "", fmt.Errorf("task %s: decode tool %s schema: %w", taskId, tool.Name, err)
			}
			// strict 工具要求所有字段必填，由模型按描述显式提供默认值。
			if err := strictJSONSchema(parameters, "$"); err != nil {
				return result, nil, "", fmt.Errorf("task %s: generate strict tool %s schema: %w", taskId, tool.Name, err)
			}
			tools = append(tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
				Name: tool.Name, Description: openai.String(tool.Description),
				Parameters: parameters, Strict: openai.Bool(true),
			}})
		}
	}
	// 工具定义仅通过 tools 字段提供，避免在 input 中重复注入。
	params := responses.ResponseNewParams{
		Model: c.agentConfig.LLMConfig.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(input),
		},
		Tools: tools,
		Text:  textConfig,
	}
	if len(messages) > 0 {
		items := make(responses.ResponseInputParam, 0, 2*len(messages)+1)
		// 先提供 ReAct 上下文，再附上独立的工具调用及结果。
		items = append(items, responses.ResponseInputItemParamOfMessage(input, responses.EasyInputMessageRoleUser))
		for _, message := range messages {
			if strings.TrimSpace(message.ToolCallID) == "" || strings.TrimSpace(message.Name) == "" {
				return result, nil, "", fmt.Errorf("task %s: tool message requires call ID and tool name", taskId)
			}
			arguments := message.Arguments
			if arguments == "" {
				arguments = "{}"
			}
			// 无 previous_response_id 时必须一起发送调用，避免结果找不到对应 call_id。
			items = append(items, responses.ResponseInputItemParamOfFunctionCall(arguments, message.ToolCallID, message.Name))
			content := message.Content.OfString.Value
			for _, part := range message.Content.OfArrayOfContentParts {
				content += part.Text
			}
			output := responses.ResponseInputItemParamOfFunctionCallOutput(content)
			output.OfFunctionCallOutput.CallID = openai.String(message.ToolCallID)
			items = append(items, output)
		}
		params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	}
	// 从最终请求生成可读日志；单次日志调用避免并发请求交错。
	encodedContext, err := formatLLMContext(params)
	if err != nil {
		return result, nil, "", fmt.Errorf("task %s: encode LLM context: %w", taskId, err)
	}
	if c.contextLogLabel != "" {
		log.Printf("task %s: [%s] LLM context=\n%s", taskId, c.contextLogLabel, encodedContext)
	} else {
		log.Printf("task %s: LLM context=\n%s", taskId, encodedContext)
	}
	response, err := c.client.Responses.New(ctx, params)
	if err != nil {
		return result, nil, "", fmt.Errorf("task %s: create response: %w", taskId, err)
	}
	if response.Status != responses.ResponseStatusCompleted {
		return result, nil, "", fmt.Errorf("task %s: response status %q (error: %s, incomplete reason: %s)",
			taskId, response.Status, response.Error.Message, response.IncompleteDetails.Reason)
	}
	var toolCalls []responses.ResponseFunctionToolCall
	for _, item := range response.Output {
		if item.Type == "function_call" {
			call := item.AsFunctionCall()
			fmt.Printf("task %s: tool calling name=%s call_id=%s arguments=%s\n",
				taskId, call.Name, call.CallID, call.Arguments)
			toolCalls = append(toolCalls, call)
		}
	}

	text := response.OutputText()
	if len(toolCalls) > 0 {
		return result, toolCalls, text, nil
	}
	if strings.TrimSpace(text) == "" {
		return result, nil, "", fmt.Errorf("task %s: response output is empty", taskId)
	}
	fmt.Printf("task %s: LLM response text=%s\n", taskId, text)
	if c.agentConfig.LLMConfig.OutputSchema == config.LineProtocolLLMOutputSchema {
		text = lineProtocolResponseDocument(text)
	}
	if err := outputParser.Parse(text, &result); err != nil {
		var zero T
		return zero, nil, "", fmt.Errorf("task %s: %w", taskId, &OutputParseError{Output: response.OutputText(), Err: err})
	}
	return result, toolCalls, "", nil
}
