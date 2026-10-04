package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type responsesApiClient[T any] struct {
	client *openai.Client
	config *config.LLMConfig
}

// NewResponsesApiClient creates a client whose Run method returns T.
func NewResponsesApiClient[T any](config *config.LLMConfig) *responsesApiClient[T] {
	client := openai.NewClient(
		option.WithAPIKey(config.ApiKey),
		option.WithBaseURL(config.BaseUrl),
	)
	return &responsesApiClient[T]{
		config: config,
		client: &client,
	}
}

// 方法不能独立声明泛型参数，因此在创建客户端时指定结果类型
func (c *responsesApiClient[T]) Run(ctx context.Context, taskId, input string) (T, error) {
	var result T
	var outputParser LLMOutputParser[*T]
	switch c.config.OutputSchema {
	case config.JsonLLMOutputSchema:
		outputParser = &JsonOutput[*T]{}
		input += "\n\n" + jsonFewShotPrompt
	case config.LineProtocolLLMOutputSchema:
		outputParser = &LineProtocolOutput[*T]{}
		input += "\n\n" + lineProtocolFewShotPrompt
	default:
		return result, fmt.Errorf("task %s: unsupported output schema %q", taskId, c.config.OutputSchema)
	}
	response, err := c.client.Responses.New(ctx, responses.ResponseNewParams{
		Model: c.config.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(input),
		},
	})
	if err != nil {
		return result, fmt.Errorf("task %s: create response: %w", taskId, err)
	}
	if response.Status != responses.ResponseStatusCompleted {
		return result, fmt.Errorf("task %s: response status %q (error: %s, incomplete reason: %s)",
			taskId, response.Status, response.Error.Message, response.IncompleteDetails.Reason)
	}
	text := response.OutputText()
	if strings.TrimSpace(text) == "" {
		return result, fmt.Errorf("task %s: response output is empty", taskId)
	}
	if err := outputParser.Parse(text, &result); err != nil {
		var zero T
		return zero, fmt.Errorf("task %s: parse response output: %w", taskId, err)
	}
	return result, nil
}
