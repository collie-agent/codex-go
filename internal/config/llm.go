package config

type LLMOutputSchema string

const (
	JsonLLMOutputSchema         LLMOutputSchema = "json_llm_output"
	LineProtocolLLMOutputSchema LLMOutputSchema = "line_protocol_llm_output"
)

type LLMConfig struct {
	BaseUrl      string
	ApiKey       string
	Model        string
	OutputSchema LLMOutputSchema
}
