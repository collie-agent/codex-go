package config

type AgentConfig struct {
	LLMConfig  *LLMConfig
	AllowTools map[McpToolName]struct{}
	SysPrompt  string
}
