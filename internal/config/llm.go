package config

type LLMOutputSchema string

const (
	JsonLLMOutputSchema         LLMOutputSchema = "json_llm_output"
	LineProtocolLLMOutputSchema LLMOutputSchema = "line_protocol_llm_output"
)

type LLMConfig struct {
	// MaxOutputTokens 为零时使用服务端默认值，正数显式限制单次响应预算。
	MaxOutputTokens int64
	// Stream enables SSE transport; omitted configurations keep non-streaming behavior.
	Stream       bool
	BaseUrl      string
	ApiKey       string
	Model        string
	OutputSchema LLMOutputSchema
}
