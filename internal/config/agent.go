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
	LogLevel   LogLevel
	LLMConfig  *LLMConfig
	AllowTools map[McpToolName]struct{}
	SysPrompt  string

	// configs stores independent resolved values so one processor cannot mutate another's settings.
	configs map[string]*AgentConfig
}

// ForAgent returns an independent copy of the resolved configuration for an agent type.
func (c *AgentConfig) ForAgent(name string) *AgentConfig {
	if c == nil {
		return nil
	}
	// Direct configs used by callers remain usable; loaded multi-agent configs
	// select an isolated resolved value below.
	if c.configs == nil {
		return cloneAgentConfig(c)
	}
	resolved := c.configs[name]
	if resolved == nil {
		resolved = c.configs["base"]
	}
	clone := cloneAgentConfig(resolved)
	// 保留只读配置集合，供 ReAct 副本解析内部观察器配置；每次返回的
	// 模型和工具设置仍独立复制，调用方修改不会影响集合中的原始配置。
	clone.configs = c.configs
	return clone
}

func cloneAgentConfig(c *AgentConfig) *AgentConfig {
	if c == nil {
		return nil
	}
	clone := &AgentConfig{Name: c.Name, LogLevel: c.LogLevel, SysPrompt: c.SysPrompt, LLMConfig: nil, AllowTools: make(map[McpToolName]struct{}, len(c.AllowTools))}
	if c.LLMConfig != nil {
		llm := *c.LLMConfig
		clone.LLMConfig = &llm
	}
	for tool := range c.AllowTools {
		clone.AllowTools[tool] = struct{}{}
	}
	return clone
}

// LoadAgentConfig strictly validates the complete multi-agent YAML before resolving any processor settings.
func LoadAgentConfig(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("parse config %q: %w", path, err)
		}
		return nil, fmt.Errorf("config %q must contain exactly one YAML document", path)
	}
	if err := rejectNull(&node); err != nil {
		return nil, err
	}
	var raw yamlAgentConfig
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if strings.TrimSpace(raw.Name) == "" {
		return nil, fmt.Errorf("config name is required")
	}
	if raw.LogLevel == "" {
		raw.LogLevel = InfoLogLevel
	}
	if raw.LogLevel != InfoLogLevel && raw.LogLevel != ErrorLogLevel {
		return nil, fmt.Errorf("unknown log_level %q: must be info or error", raw.LogLevel)
	}
	if raw.Name != "react" && raw.Name != "plan-execute" {
		return nil, fmt.Errorf("unknown agent type %q", raw.Name)
	}
	if raw.Base == nil {
		return nil, fmt.Errorf("base is required")
	}
	base, err := resolveConfig("base", *raw.Base, nil)
	if err != nil {
		return nil, err
	}
	resolved := map[string]*AgentConfig{"base": base}
	for name, partial := range raw.Agents {
		if name != "react" && name != "observe" && name != "plan" {
			return nil, fmt.Errorf("unknown agent type %q", name)
		}
		if partial == nil {
			return nil, fmt.Errorf("agents.%s must be a mapping", name)
		}
		cfg, err := resolveConfig(name, *partial, base)
		if err != nil {
			return nil, err
		}
		if name != "react" && partial.AllowTools != nil && len(*partial.AllowTools) != 0 {
			return nil, fmt.Errorf("agents.%s cannot enable tools", name)
		}
		resolved[name] = cfg
	}
	for _, name := range []string{"react", "observe", "plan"} {
		if _, ok := resolved[name]; !ok {
			resolved[name] = cloneAgentConfig(base)
			resolved[name].Name = name
		}
		// 内部代理不继承 base 的工具权限，确保其最终能力始终为空。
		if name != "react" {
			resolved[name].AllowTools = map[McpToolName]struct{}{}
		}
	}
	// 日志级别属于整个运行，所有独立代理及配置副本使用相同级别。
	for _, cfg := range resolved {
		cfg.LogLevel = raw.LogLevel
	}
	entry := raw.Name
	if entry == "plan-execute" {
		entry = "plan"
	}
	out := cloneAgentConfig(resolved[entry])
	out.Name = raw.Name
	out.configs = resolved
	return out, nil
}

func resolveConfig(name string, raw yamlAgentSettings, base *AgentConfig) (*AgentConfig, error) {
	var llm *LLMConfig
	var err error
	// LLM 是整块替换；工具与提示词分别按字段是否省略来回退。
	if raw.LLM != nil {
		llm, err = resolveLLM(*raw.LLM)
		if err != nil {
			return nil, err
		}
	} else if base != nil {
		llm = base.LLMConfig
	} else {
		return nil, fmt.Errorf("base.llm is required")
	}
	tools := map[McpToolName]struct{}{}
	if base != nil {
		for tool := range base.AllowTools {
			tools[tool] = struct{}{}
		}
	}
	if raw.AllowTools != nil {
		tools = make(map[McpToolName]struct{}, len(*raw.AllowTools))
		for _, name := range *raw.AllowTools {
			tool := McpToolName(name)
			if !knownMCPTools[tool] {
				return nil, fmt.Errorf("unknown tool %q", name)
			}
			tools[tool] = struct{}{}
		}
	}
	prompt := ""
	if base != nil {
		prompt = base.SysPrompt
	}
	if raw.SysPrompt != nil {
		prompt = *raw.SysPrompt
	}
	return &AgentConfig{Name: name, LLMConfig: llm, AllowTools: tools, SysPrompt: prompt}, nil
}

func resolveLLM(raw yamlLLMConfig) (*LLMConfig, error) {
	if raw.Model == "" {
		return nil, fmt.Errorf("llm.model is required")
	}
	if raw.ApiKey != "" && raw.ApiKeyEnv != "" {
		return nil, fmt.Errorf("llm.api_key and llm.api_key_env are mutually exclusive")
	}
	key := raw.ApiKey
	if raw.ApiKeyEnv != "" {
		key = os.Getenv(raw.ApiKeyEnv)
	} else if key == "" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if key == "" {
		return nil, fmt.Errorf("LLM API key is empty")
	}
	output := raw.OutputSchema
	if output == "" {
		output = string(JsonLLMOutputSchema)
	}
	if output != string(JsonLLMOutputSchema) && output != string(LineProtocolLLMOutputSchema) {
		return nil, fmt.Errorf("unknown llm.output_schema %q", output)
	}
	baseURL := raw.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &LLMConfig{BaseUrl: baseURL, ApiKey: key, Model: raw.Model, OutputSchema: LLMOutputSchema(output)}, nil
}

func rejectNull(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return fmt.Errorf("explicit null is not allowed")
	}
	for _, child := range node.Content {
		if err := rejectNull(child); err != nil {
			return err
		}
	}
	return nil
}

type yamlAgentConfig struct {
	Name     string                        `yaml:"name"`
	LogLevel LogLevel                      `yaml:"log_level"`
	Base     *yamlAgentSettings            `yaml:"base"`
	Agents   map[string]*yamlAgentSettings `yaml:"agents"`
}
type yamlAgentSettings struct {
	LLM        *yamlLLMConfig `yaml:"llm"`
	AllowTools *[]string      `yaml:"allow_tools"`
	SysPrompt  *string        `yaml:"sys_prompt"`
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
	GrepMCPToolName: true, ApplyPatchMCPToolName: true, ExecMCPToolName: true,
}
