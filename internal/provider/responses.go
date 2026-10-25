package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

type responsesApiClient[T any] struct {
	client          *openai.Client
	agentConfig     *config.AgentConfig
	contextLogLabel string
	systemPrompt    string
	logLevel        config.LogLevel
}

func (c *responsesApiClient[T]) SetLogLevel(level config.LogLevel) {
	c.logLevel = level
}

// SetContextLogLabel 仅标记当前客户端的上下文日志，不修改任务 ID 或模型请求。
func (c *responsesApiClient[T]) SetContextLogLabel(label string) {
	c.contextLogLabel = label
}

// SetSystemPrompt 将系统指令作为独立的 system 消息发送。
func (c *responsesApiClient[T]) SetSystemPrompt(prompt string) {
	c.systemPrompt = prompt
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
func (c *responsesApiClient[T]) Run(ctx context.Context, taskId, input string, mcpClientSess *sdk.ClientSession) (T, []responses.ResponseFunctionToolCall, string, error) {
	return c.RunWithToolMessages(ctx, taskId, input, mcpClientSess, nil)
}

// ToolMessage 保留 SDK 原生工具消息及调用信息，供无服务端历史的请求重放。
type ToolMessage struct {
	openai.ChatCompletionToolMessageParam
	Name      string
	Arguments string
}

// RunWithToolMessages 原样发送调用方组装的 input；格式约束和末尾上下文由调用方负责。
func (c *responsesApiClient[T]) RunWithToolMessages(ctx context.Context, taskId, input string, mcpClientSess *sdk.ClientSession, messages []ToolMessage) (T, []responses.ResponseFunctionToolCall, string, error) {
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
		if c.logLevel.InfoEnabled() {
			if encodedSchema, err := json.MarshalIndent(schema, "", "  "); err == nil {
				fmt.Fprintf(os.Stderr, "task %s: JSON schema=\n%s\n", taskId, encodedSchema)
			}
		}
		// Responses API 通过 text.format 约束最终文本，工具调用仍按原有流程处理。
		textConfig.Format.OfJSONSchema = &responses.ResponseFormatTextJSONSchemaConfigParam{
			Name: "llm_output", Schema: schema, Strict: openai.Bool(true),
		}
		outputParser = parser
	case config.LineProtocolLLMOutputSchema:
		outputParser = &LineProtocolOutput[*T]{}
	default:
		return result, nil, "", fmt.Errorf("task %s: unsupported output schema %q", taskId, c.agentConfig.LLMConfig.OutputSchema)
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
		// 当前请求不重放 reasoning 项，显式关闭思考模式以支持后续工具结果回传。
		Reasoning: shared.ReasoningParam{Effort: shared.ReasoningEffortNone},
	}
	if len(messages) > 0 || c.systemPrompt != "" {
		items := make(responses.ResponseInputParam, 0, 2*len(messages)+1)
		// 系统指令独立于任务数据，工具重放时也保持相同角色。
		if c.systemPrompt != "" {
			items = append(items, responses.ResponseInputItemParamOfMessage(c.systemPrompt, responses.EasyInputMessageRoleSystem))
		}
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
		// 先重放原生工具调用及结果，再提供本轮最新状态，保持状态区块位于 context 末尾。
		items = append(items, responses.ResponseInputItemParamOfMessage(input, responses.EasyInputMessageRoleUser))
		params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	}
	// 从最终请求生成可读日志；单次日志调用避免并发请求交错。
	if c.logLevel.InfoEnabled() {
		encodedContext, err := formatLLMContext(params)
		if err != nil {
			return result, nil, "", fmt.Errorf("task %s: encode LLM context: %w", taskId, err)
		}
		if c.contextLogLabel != "" {
			log.Printf("task %s: [%s] LLM context=\n%s", taskId, c.contextLogLabel, encodedContext)
		} else {
			log.Printf("task %s: LLM context=\n%s", taskId, encodedContext)
		}
	}
	response, err := c.client.Responses.New(ctx, params)
	if err != nil {
		// SDK 的 Error() 只含 HTTP 状态；补充服务端诊断，保留原始错误链。
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			detail := apiErr.RawJSON()
			if key := c.agentConfig.LLMConfig.ApiKey; key != "" {
				detail = strings.ReplaceAll(detail, key, "<REDACTED>")
				// JSON 正文中的凭据也可能包含转义字符。
				encodedKey, _ := json.Marshal(key)
				detail = strings.ReplaceAll(detail, string(encodedKey[1:len(encodedKey)-1]), "<REDACTED>")
			}
			if detail != "" {
				return result, nil, "", fmt.Errorf("task %s: create response: %w; provider error: %s", taskId, err, detail)
			}
		}
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
			// 两种日志级别均保留完整调用参数，便于追踪实际执行的操作。
			fmt.Fprintf(os.Stderr, "task %s: tool calling name=%s call_id=%s arguments=%s\n",
				taskId, call.Name, call.CallID, call.Arguments)
			toolCalls = append(toolCalls, call)
		}
	}

	text := response.OutputText()
	// 响应文本是用户可见输出；error 级别及携带工具调用的轮次也保留。
	if strings.TrimSpace(text) != "" {
		fmt.Fprintf(os.Stderr, "task %s: LLM response text=%s\n", taskId, text)
	}
	if len(toolCalls) > 0 {
		return result, toolCalls, text, nil
	}
	if strings.TrimSpace(text) == "" {
		return result, nil, "", fmt.Errorf("task %s: response output is empty", taskId)
	}
	if c.agentConfig.LLMConfig.OutputSchema == config.LineProtocolLLMOutputSchema {
		text = lineProtocolResponseDocument(text)
	}
	if err := outputParser.Parse(text, &result); err != nil {
		var zero T
		return zero, nil, "", fmt.Errorf("task %s: %w", taskId, &OutputParseError{Output: response.OutputText(), Err: err})
	}
	return result, toolCalls, "", nil
}
