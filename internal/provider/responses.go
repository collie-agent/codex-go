package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type responsesApiClient[T any] struct {
	client      *openai.Client
	agentConfig *config.AgentConfig
}

// NewResponsesApiClient creates a client whose Run method returns T and connects
// its MCP tools. Call Close when the client is no longer needed.
func NewResponsesApiClient[T any](llmConfig *config.LLMConfig, toolConfig filemcp.Config) (*responsesApiClient[T], error) {
	if llmConfig == nil {
		return nil, fmt.Errorf("LLM config is required")
	}
	client := openai.NewClient(
		option.WithAPIKey(llmConfig.ApiKey),
		option.WithBaseURL(llmConfig.BaseUrl),
	)
	c := &responsesApiClient[T]{
		agentConfig: &config.AgentConfig{LLMConfig: llmConfig},
		client:      &client,
	}
	return c, nil
}

// Run returns unparsed output text as reasoning when the response calls tools.
// Responses without tool calls are parsed into T.
// A nil MCP session sends a request without tools.
// 方法不能独立声明泛型参数，因此在创建客户端时指定结果类型
func (c *responsesApiClient[T]) Run(ctx context.Context, taskId, input string, mcpClientSess *sdk.ClientSession) (T, []responses.ResponseFunctionToolCall, string, error) {
	var result T
	var outputParser LLMOutputParser[*T]
	switch c.agentConfig.LLMConfig.OutputSchema {
	case config.JsonLLMOutputSchema:
		outputParser = &JsonOutput[*T]{}
		input += "\n\n" + jsonFewShotPrompt
	case config.LineProtocolLLMOutputSchema:
		outputParser = &LineProtocolOutput[*T]{}
		input += "\n\n" + lineProtocolFewShotPrompt
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
			tools = append(tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
				Name: tool.Name, Description: openai.String(tool.Description),
				Parameters: parameters, Strict: openai.Bool(false),
			}})
		}
	}
	params := responses.ResponseNewParams{
		Model: c.agentConfig.LLMConfig.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(input),
		},
		Tools: tools,
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
	if err := outputParser.Parse(text, &result); err != nil {
		var zero T
		return zero, nil, "", fmt.Errorf("task %s: parse response output: %w", taskId, err)
	}
	return result, toolCalls, "", nil
}
