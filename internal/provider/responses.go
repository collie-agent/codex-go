package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// ErrEmptyResponse 区分正常空文本与协议解析失败，由调用方决定是否继续。
var ErrEmptyResponse = errors.New("response output is empty")

// responsesApiClient delegates transport selection while sharing request and output behavior.
type responsesApiClient[T any] struct {
	*baseResponsesApiClient[T]
}

type baseResponsesApiClient[T any] struct {
	strategy        responseStrategy
	client          *openai.Client
	agentConfig     *config.AgentConfig
	contextLogLabel string
	systemPrompt    string
	corrections     []string
	logLevel        config.LogLevel
	output          Outputer
	inputTokens     int64
	additionalTools []*sdk.Tool
	stderr          Outputer
}

// SetOutputer 在运行前配置输出目标；nil 恢复默认 stderr 输出。
// 自定义目标接收所有正文 delta；默认 stderr 仅在 debug 级别展示 delta。
// 与其他配置方法一样，不应在请求执行期间调用。
func (c *baseResponsesApiClient[T]) SetOutputer(output Outputer) {
	if output == nil {
		c.stderr = NewStderrOutputer(nil)
	}
	c.output = output
}

// InputTokens 返回最近一次请求中模型报告的输入 token 数；未提供 usage 时为零。
func (c *baseResponsesApiClient[T]) InputTokens() int64 { return c.inputTokens }

// SetAdditionalTools 为执行实例内的本地工具提供定义；执行仍由调用方负责。
func (c *baseResponsesApiClient[T]) SetAdditionalTools(tools []*sdk.Tool) {
	c.additionalTools = slices.Clone(tools)
}

// emitOutput 记录输出错误；持久输出器会取消运行上下文，阻止后续执行。
func (c *baseResponsesApiClient[T]) emitOutput(ctx context.Context, taskID string, kind OutputKind, data any) {
	encoded, err := json.Marshal(data)
	if err == nil {
		output := c.output
		if output == nil {
			// 默认 stderr 只在 debug 级别展示正文增量，避免流式内容刷屏。
			if isDeltaOutput(kind) && c.logLevel != config.DebugLogLevel {
				return
			}
			output = c.stderr
		}
		err = output.Emit(ctx, OutputEvent{TaskID: taskID, RunID: taskID, Label: c.contextLogLabel, Kind: kind, Data: encoded})
	}
	if err != nil {
		log.Printf("task %s: emit output kind=%s: %v", taskID, kind, err)
	}
}

func (c *baseResponsesApiClient[T]) SetLogLevel(level config.LogLevel) {
	c.logLevel = level
}

// SetContextLogLabel 仅标记当前客户端的上下文日志，不修改任务 ID 或模型请求。
func (c *baseResponsesApiClient[T]) SetContextLogLabel(label string) {
	c.contextLogLabel = label
}

// SetSystemPrompt 将系统指令作为独立的 system 消息发送。
func (c *baseResponsesApiClient[T]) SetSystemPrompt(prompt string) {
	c.systemPrompt = prompt
}

// SetCorrectionMessages 将运行内的纠错提示作为正文之后的独立 user 消息发送。
// 复制切片，避免调用方后续追加时改变当前请求的消息快照。
func (c *baseResponsesApiClient[T]) SetCorrectionMessages(corrections []string) {
	c.corrections = slices.Clone(corrections)
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
	var strategy responseStrategy = nonStreamingResponseStrategy{}
	if llmConfig.Stream {
		strategy = streamingResponseStrategy{}
	}
	c := &responsesApiClient[T]{baseResponsesApiClient: &baseResponsesApiClient[T]{
		strategy:    strategy,
		agentConfig: &config.AgentConfig{LLMConfig: llmConfig, AllowTools: allowTools},
		client:      &client,
		stderr:      NewStderrOutputer(nil),
	}}
	return c, nil
}

// Run returns unparsed output text as reasoning when the response calls tools.
// Responses without tool calls are parsed into T.
// A nil MCP session omits remote tools; explicitly supplied local tools remain available.
// 方法不能独立声明泛型参数，因此在创建客户端时指定结果类型
func (c *baseResponsesApiClient[T]) Run(ctx context.Context, taskId, input string, mcpClientSess *sdk.ClientSession) (T, []responses.ResponseFunctionToolCall, string, error) {
	return c.RunWithToolMessages(ctx, taskId, input, mcpClientSess, nil)
}

// ToolMessage 保留 SDK 原生工具消息及调用信息，供无服务端历史的请求重放。
type ToolMessage struct {
	openai.ChatCompletionToolMessageParam
	Name      string
	Arguments string
}

// RunWithToolMessages 原样发送调用方组装的 input；格式约束和末尾上下文由调用方负责。
func (c *baseResponsesApiClient[T]) RunWithToolMessages(ctx context.Context, taskId, input string, mcpClientSess *sdk.ClientSession, messages []ToolMessage) (_ T, _ []responses.ResponseFunctionToolCall, _ string, runErr error) {
	c.inputTokens = 0
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
			c.emitOutput(ctx, taskId, OutputSchema, schema)
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
	candidates := slices.Clone(c.additionalTools)
	if mcpClientSess != nil {
		for tool, err := range mcpClientSess.Tools(ctx, nil) {
			if err != nil {
				return result, nil, "", fmt.Errorf("task %s: list MCP tools: %w", taskId, err)
			}
			candidates = append(candidates, tool)
		}
	}
	for _, tool := range candidates {
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
	if len(messages) > 0 || c.systemPrompt != "" || len(c.corrections) > 0 {
		items := make(responses.ResponseInputParam, 0, 2*len(messages)+2+len(c.corrections))
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
		// 工具历史之后提供最新状态；纠错提示单独置于状态之后。
		items = append(items, responses.ResponseInputItemParamOfMessage(input, responses.EasyInputMessageRoleUser))
		for _, correction := range c.corrections {
			items = append(items, responses.ResponseInputItemParamOfMessage(correction, responses.EasyInputMessageRoleUser))
		}
		params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	}
	// 从最终请求生成可读日志；单次日志调用避免并发请求交错。
	if c.logLevel.InfoEnabled() {
		encodedContext, err := formatLLMContext(params)
		if err != nil {
			return result, nil, "", fmt.Errorf("task %s: encode LLM context: %w", taskId, err)
		}
		c.emitOutput(ctx, taskId, OutputContext, ContextOutput{Context: string(encodedContext)})
	}
	// Middleware 位于 SDK 重试循环内；正文只随 SDK 的 Read 被动收集，不能抢先读取并阻塞重试。
	var attemptBodies []*usageBody
	// 自定义目标接收全部流式片段，stderr 的展示级别由输出适配器处理。
	answer := &answerStream{lineProtocol: c.agentConfig.LLMConfig.OutputSchema == config.LineProtocolLLMOutputSchema}
	// 校验前的流式正文是临时内容；失败时只撤回本次请求已发出的字节。
	// 使用字节数与 Go/Redis 的 UTF-8 编码一致，避免误删此前轮次的答案。
	var provisional OutputResetOutput
	defer func() {
		if runErr != nil && (provisional.TextBytes > 0 || provisional.AnswerBytes > 0) {
			c.emitOutput(context.WithoutCancel(ctx), taskId, OutputReset, provisional)
		}
	}()
	var streamedText strings.Builder
	onTextDelta := func(delta string) error {
		streamedText.WriteString(delta)
		text := answer.feed(delta)
		// 确定非法后不再发布当前片段；立即交由传输层取消，再撤回此前已发布内容。
		if answer.err != nil {
			return &OutputParseError{Output: streamedText.String(), Err: answer.err}
		}
		provisional.TextBytes += len(delta)
		if text != "" {
			provisional.AnswerBytes += len(text)
			c.emitOutput(ctx, taskId, OutputAnswerDelta, TextOutput{Text: text})
		}
		c.emitOutput(ctx, taskId, OutputTextDelta, TextOutput{Text: delta})
		return nil
	}
	response, err := c.strategy.create(ctx, c.client, params, onTextDelta, option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		resp, requestErr := next(req)
		status := "unknown"
		if requestErr != nil {
			status = attemptErrorStatus(requestErr)
		} else if resp != nil && resp.StatusCode >= 400 {
			status = fmt.Sprintf("http_error_%d", resp.StatusCode)
		}
		record := func(body []byte, readErr error) {
			if c.logLevel == config.DebugLogLevel && len(body) > 0 {
				c.emitOutput(ctx, taskId, OutputRawResponse, RawResponseOutput{Body: string(body)})
			}
			usageResponse := &responses.Response{}
			// SSE 的终态事件本身已完整；[DONE]、取消或断连不保证 HTTP EOF。
			// 仅解析 SDK 已读取的数据，绝不主动读取被丢弃的重试正文。
			if resp != nil && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				decoder := ssestream.NewDecoder(&http.Response{Header: resp.Header, Body: io.NopCloser(bytes.NewReader(body))})
				defer decoder.Close()
				for decoder.Next() {
					var event responses.ResponseStreamEventUnion
					if json.Unmarshal(decoder.Event().Data, &event) == nil {
						switch event.Type {
						case "response.completed", "response.failed", "response.incomplete":
							*usageResponse = event.Response
						}
					}
				}
			} else if readErr == io.EOF {
				// JSON 正文仍须读到 EOF，避免把不完整响应当作有效统计。
				if json.Unmarshal(body, usageResponse) != nil {
					usageResponse = &responses.Response{}
				}
			}
			if readErr != nil && readErr != io.EOF {
				status = attemptErrorStatus(readErr)
			} else if readErr != io.EOF && req.Context().Err() != nil {
				status = attemptErrorStatus(req.Context().Err())
			}
			if usageResponse.Status == "" {
				usageResponse.Status = responses.ResponseStatus(status)
			}
			c.logResponseUsage(ctx, taskId, params.Model, usageResponse)
		}
		if resp != nil && resp.Body != nil {
			body := &usageBody{ReadCloser: resp.Body, record: record}
			attemptBodies = append(attemptBodies, body)
			resp.Body = body
		} else {
			record(nil, nil)
		}
		return resp, requestErr
	}))
	// SDK 在响应头返回后发现 context 已取消时会提前退出；兜底关闭，并通过 Once 避免重复关闭和统计。
	for _, body := range attemptBodies {
		_ = body.Close()
	}

	if response != nil {
		c.inputTokens = response.Usage.InputTokens
	}
	if err != nil {
		var parseErr *OutputParseError
		if errors.As(err, &parseErr) {
			log.Printf("task %s: [%s] LLM output format error: %v; output=%q", taskId, c.contextLogLabel, parseErr.Err, parseErr.Output)
			return result, nil, "", fmt.Errorf("task %s: %w", taskId, parseErr)
		}
		// 请求错误可能被上层处理，仍需在 error 级别保留诊断。
		defer func() { log.Printf("task %s: [%s] LLM request failed: %v", taskId, c.contextLogLabel, runErr) }()
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
	// 统计已由 HTTP 尝试层记录；此处状态检查和输出解析不得重复计数。
	if response.Status != responses.ResponseStatusCompleted {
		err := fmt.Errorf("task %s: response status %q (error: %s, incomplete reason: %s)",
			taskId, response.Status, response.Error.Message, response.IncompleteDetails.Reason)
		log.Printf("task %s: [%s] LLM response failed: %v", taskId, c.contextLogLabel, err)
		return result, nil, "", err
	}
	var toolCalls []responses.ResponseFunctionToolCall
	for _, item := range response.Output {
		if item.Type == "function_call" {
			call := item.AsFunctionCall()
			// 两种日志级别均保留完整调用参数，便于追踪实际执行的操作。
			c.emitOutput(ctx, taskId, OutputToolCall, ToolCallOutput{Name: call.Name, CallID: call.CallID, Arguments: call.Arguments})
			toolCalls = append(toolCalls, call)
		}
	}

	text := response.OutputText()
	// 完整文本用于诊断；自定义输出仍保持原有的 answer 和增量事件语义。
	if strings.TrimSpace(text) != "" && c.logLevel.InfoEnabled() {
		logger := c.stderr
		if configured, ok := c.output.(*configuredOutputer); ok {
			logger = configured.stderr
		}
		if c.output == nil || logger != c.stderr {
			data, _ := json.Marshal(TextOutput{Text: text})
			if err := logger.Emit(ctx, OutputEvent{TaskID: taskId, RunID: taskId, Label: c.contextLogLabel, Kind: OutputText, Data: data}); err != nil {
				log.Printf("task %s: emit response text: %v", taskId, err)
			}
		}
	}

	emitNonStreamingText := func() {
		if !c.agentConfig.LLMConfig.Stream && strings.TrimSpace(text) != "" {
			visibleText := answer.feed(text)
			if c.output == nil {
				visibleText = text
			}
			if c.output != nil || !c.logLevel.InfoEnabled() {
				c.emitOutput(ctx, taskId, OutputText, TextOutput{Text: visibleText})
			}
		}
	}

	if len(toolCalls) > 0 {
		// 普通说明可以伴随工具调用；可解析的最终结果与工具调用互斥。
		// 在返回任何调用前整批拒绝，避免执行副作用后再发现完成声明冲突。
		document := text
		if c.agentConfig.LLMConfig.OutputSchema == config.LineProtocolLLMOutputSchema {
			document = lineProtocolResponseDocument(text)
		}
		var finalResult T
		if strings.TrimSpace(document) != "" && outputParser.Parse(document, &finalResult) == nil {
			rejection := errors.New("response contains both a final result and tool calls; the rejected tool batch was not executed. Return only the final result if the task is complete, or only tool calls with optional explanatory text if more work is needed")
			for _, call := range toolCalls {
				c.emitOutput(ctx, taskId, OutputToolError, ToolResultOutput{Name: call.Name, CallID: call.CallID, Error: rejection.Error()})
			}
			return result, nil, "", fmt.Errorf("task %s: %w", taskId, &OutputParseError{
				Output: text,
				Err:    rejection,
			})
		}
		emitNonStreamingText()
		return result, toolCalls, text, nil
	}
	if strings.TrimSpace(text) == "" {
		return result, nil, "", fmt.Errorf("task %s: %w", taskId, ErrEmptyResponse)
	}
	if c.agentConfig.LLMConfig.OutputSchema == config.LineProtocolLLMOutputSchema {
		text = lineProtocolResponseDocument(text)
	}
	if err := outputParser.Parse(text, &result); err != nil {
		// 格式错误可能被上层纠错重试消化，必须在此记录，error 级别也保留诊断。
		log.Printf("task %s: [%s] LLM output format error: %v; output=%q", taskId, c.contextLogLabel, err, response.OutputText())
		var zero T
		return zero, nil, "", fmt.Errorf("task %s: %w", taskId, &OutputParseError{Output: response.OutputText(), Err: err})
	}
	emitNonStreamingText()
	return result, toolCalls, "", nil
}

func (c *baseResponsesApiClient[T]) logResponseUsage(ctx context.Context, taskID, model string, response *responses.Response) {
	input, output, cached, uncached := "unknown", "unknown", "unknown", "unknown"
	u := response.Usage
	// SDK 数值零值不能区分缺失/null 与显式 0，必须检查字段元数据。
	if u.JSON.InputTokens.Valid() {
		input = strconv.FormatInt(u.InputTokens, 10)
	}
	if u.JSON.OutputTokens.Valid() {
		output = strconv.FormatInt(u.OutputTokens, 10)
	}
	if u.InputTokensDetails.JSON.CachedTokens.Valid() {
		cached = strconv.FormatInt(u.InputTokensDetails.CachedTokens, 10)
		if u.JSON.InputTokens.Valid() {
			// 未命中包含缓存写入消耗，只减去服务端报告的缓存命中。
			uncached = strconv.FormatInt(u.InputTokens-u.InputTokensDetails.CachedTokens, 10)
		}
	}
	// 不受日志级别过滤，保留每次 HTTP 尝试的计数和未知值语义。
	c.emitOutput(ctx, taskID, OutputUsage, UsageOutput{
		Model: model, ResponseID: response.ID, Status: string(response.Status),
		InputTokens: input, OutputTokens: output, CachedInputTokens: cached, UncachedInputTokens: uncached,
	})
}

// usageBody 不改变 SDK 的读取结果；Close 在重试、正常读取及取消兜底路径均只执行一次。
type usageBody struct {
	io.ReadCloser
	body      bytes.Buffer
	readErr   error
	record    func([]byte, error)
	closeOnce sync.Once
	closeErr  error
}

func (b *usageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	_, _ = b.body.Write(p[:n])
	if err != nil {
		b.readErr = err
	}
	return n, err
}

func (b *usageBody) Close() error {
	b.closeOnce.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.record(b.body.Bytes(), b.readErr)
	})
	return b.closeErr
}

func attemptErrorStatus(err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "network_error"
}

// responseStrategy isolates transport; both strategies return the same complete response.
type responseStrategy interface {
	create(context.Context, *openai.Client, responses.ResponseNewParams, func(string) error, ...option.RequestOption) (*responses.Response, error)
}

type nonStreamingResponseStrategy struct{}

func (nonStreamingResponseStrategy) create(ctx context.Context, client *openai.Client, params responses.ResponseNewParams, _ func(string) error, opts ...option.RequestOption) (*responses.Response, error) {
	return client.Responses.New(ctx, params, opts...)
}

type streamingResponseStrategy struct{}

func (streamingResponseStrategy) create(ctx context.Context, client *openai.Client, params responses.ResponseNewParams, onTextDelta func(string) error, opts ...option.RequestOption) (*responses.Response, error) {
	// 格式错误只取消当前 HTTP 请求，父运行上下文必须保持可用以便纠错重试。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := client.Responses.NewStreaming(ctx, params, opts...)
	defer stream.Close()
	var response *responses.Response
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "response.output_text.delta":
			// 仅转发正文片段；工具参数、推理片段和最终完整文本由各自路径处理。
			if onTextDelta != nil && event.Delta != "" {
				if err := onTextDelta(event.Delta); err != nil {
					cancel()
					return nil, err
				}
			}
		case "response.completed", "response.failed", "response.incomplete":
			// Terminal events carry the authoritative full text, tool calls and usage.
			final := event.Response
			response = &final
		case "error":
			return nil, fmt.Errorf("stream error: %s", event.RawJSON())
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("stream ended without a terminal response")
	}
	return response, nil
}
