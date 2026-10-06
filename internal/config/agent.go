package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type AgentConfig struct {
	Name       string
	LLMConfig  *LLMConfig
	AllowTools map[McpToolName]struct{}
	SysPrompt  string
}

// LoadAgentConfig 加载并校验单个 YAML Agent；严格边界可避免拼写错误的配置
// 悄悄改变任务行为。
func LoadAgentConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var raw yamlAgentConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	// Decode 只消费一个文档，必须确认 EOF，避免尾随配置或语法错误被忽略。
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
		return nil, fmt.Errorf("config %q must contain exactly one YAML document", path)
	}
	if strings.TrimSpace(raw.Name) == "" {
		return nil, fmt.Errorf("config name is required")
	}
	if raw.Name != "react" {
		return nil, fmt.Errorf("unknown agent type %q", raw.Name)
	}
	if raw.LLM.Model == "" {
		return nil, fmt.Errorf("llm.model is required")
	}
	if raw.LLM.ApiKey != "" && raw.LLM.ApiKeyEnv != "" {
		return nil, fmt.Errorf("llm.api_key and llm.api_key_env are mutually exclusive")
	}
	apiKey := raw.LLM.ApiKey
	if raw.LLM.ApiKeyEnv != "" {
		apiKey = os.Getenv(raw.LLM.ApiKeyEnv)
	} else if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("LLM API key is empty")
	}
	outputSchema := raw.LLM.OutputSchema
	if outputSchema == "" {
		outputSchema = string(JsonLLMOutputSchema)
	}
	if outputSchema != string(JsonLLMOutputSchema) && outputSchema != string(LineProtocolLLMOutputSchema) {
		return nil, fmt.Errorf("unknown llm.output_schema %q", outputSchema)
	}
	allowTools := make(map[McpToolName]struct{}, len(raw.AllowTools))
	for _, name := range raw.AllowTools {
		tool := McpToolName(name)
		if !knownMCPTools[tool] {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		allowTools[tool] = struct{}{}
	}
	baseURL := raw.LLM.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &AgentConfig{
		Name:       raw.Name,
		LLMConfig:  &LLMConfig{BaseUrl: baseURL, ApiKey: apiKey, Model: raw.LLM.Model, OutputSchema: LLMOutputSchema(outputSchema)},
		AllowTools: allowTools,
		SysPrompt:  raw.SysPrompt,
	}, nil
}

type yamlAgentConfig struct {
	Name       string        `yaml:"name"`
	LLM        yamlLLMConfig `yaml:"llm"`
	AllowTools []string      `yaml:"allow_tools"`
	SysPrompt  string        `yaml:"sys_prompt"`
}

type yamlLLMConfig struct {
	BaseURL      string `yaml:"base_url"`
	ApiKey       string `yaml:"api_key"`
	ApiKeyEnv    string `yaml:"api_key_env"`
	Model        string `yaml:"model"`
	OutputSchema string `yaml:"output_schema"`
}

var knownMCPTools = map[McpToolName]bool{
	ReadFileMCPToolName: true, ListDirMCPToolName: true, GlobMCPToolName: true,
	GrepMCPToolName: true, ApplyPatchMCPToolName: true,
}
