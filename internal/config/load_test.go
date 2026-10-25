package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAgentConfigLogLevel(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test")
	for _, level := range []string{"", "info", "error", "debug"} {
		t.Run("level="+level, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.yaml")
			data := "name: react\nbase:\n  llm:\n    model: test\nagents:\n  observe:\n    sys_prompt: observer\n"
			if level != "" {
				data += "log_level: " + level + "\n"
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadAgentConfig(path)
			if level == "debug" {
				if err == nil || !strings.Contains(err.Error(), "unknown log_level") {
					t.Fatalf("invalid log level error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := InfoLogLevel
			if level != "" {
				want = LogLevel(level)
			}
			if cfg.LogLevel != want {
				t.Fatalf("log level = %q, want %q", cfg.LogLevel, want)
			}
			for _, name := range []string{"base", "react", "observe", "plan"} {
				if got := cfg.ForAgent(name).LogLevel; got != want {
					t.Errorf("%s log level = %q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestLoadAgentConfigAppliesDefaultsAndResolvesEnvironmentKey(t *testing.T) {
	t.Setenv("YAML_AGENT_TEST_KEY", "secret-value")
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(`name: react
base:
  llm:
    model: gpt-test
    api_key_env: YAML_AGENT_TEST_KEY
  allow_tools:
    - read_file
    - grep
    - exec
`), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "react" || got.LLMConfig.Model != "gpt-test" {
		t.Fatalf("unexpected identity: %+v", got)
	}
	if got.LLMConfig.BaseUrl != "https://api.openai.com/v1" || got.LLMConfig.OutputSchema != JsonLLMOutputSchema {
		t.Fatalf("defaults not applied: %+v", got.LLMConfig)
	}
	if got.LLMConfig.ApiKey != "secret-value" {
		t.Fatalf("environment key not resolved")
	}
	if _, ok := got.AllowTools[ReadFileMCPToolName]; !ok {
		t.Fatal("read_file was not enabled")
	}
	if _, ok := got.AllowTools[GrepMCPToolName]; !ok {
		t.Fatal("grep was not enabled")
	}
	if _, ok := got.AllowTools[ExecMCPToolName]; !ok {
		t.Fatal("exec was not enabled")
	}
}

func TestLoadAgentConfigResolvesBaseForReact(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "base-secret")
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(`name: react
base:
  llm:
    model: base-model
  allow_tools: [read_file]
  sys_prompt: shared prompt
`), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.ForAgent("react")
	if got.LLMConfig.Model != "base-model" || got.LLMConfig.ApiKey != "base-secret" {
		t.Fatalf("unexpected resolved LLM config: %+v", got.LLMConfig)
	}
	if _, ok := got.AllowTools[ReadFileMCPToolName]; !ok || got.SysPrompt != "shared prompt" {
		t.Fatalf("base settings were not applied: %+v", got)
	}
}

func TestLoadAgentConfigAppliesIndependentOverrides(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "base-secret")
	path := filepath.Join(t.TempDir(), "agents.yaml")
	if err := os.WriteFile(path, []byte(`name: react
base:
  llm:
    model: base-model
  allow_tools: [read_file]
  sys_prompt: shared prompt
agents:
  react:
    allow_tools: []
    sys_prompt: ""
`), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	react := loaded.ForAgent("react")
	if len(react.AllowTools) != 0 || react.SysPrompt != "" {
		t.Fatalf("explicit empty overrides were lost: %+v", react)
	}
}

func TestLoadAgentConfigValidatesUnusedAgentSettingsAndToolBoundary(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "base-secret")
	for _, tc := range []struct{ name, agent, want string }{
		{"removed analyzer", "task_analyzer:\n    llm:\n      model: analyzer\n", "unknown agent type"},
		{"internal tools", "observe:\n    allow_tools: [read_file]\n", "cannot enable tools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agents.yaml")
			body := "name: react\nbase:\n  llm:\n    model: base-model\nagents:\n  " + tc.agent
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAgentConfig(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadAgentConfigRejectsExplicitNullWithoutLeakingKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sensitive-value")
	path := filepath.Join(t.TempDir(), "agents.yaml")
	body := "name: react\nbase:\n  llm:\n    model: base-model\n  sys_prompt: null\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentConfig(path); err == nil || !strings.Contains(err.Error(), "null") || strings.Contains(err.Error(), "sensitive-value") {
		t.Fatalf("error=%v, want null rejection without secret", err)
	}
}

func TestLoadAgentConfigReturnsIndependentAgentCopies(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "base-secret")
	path := filepath.Join(t.TempDir(), "agents.yaml")
	body := "name: react\nbase:\n  llm:\n    model: base-model\n  allow_tools: [read_file]\n  sys_prompt: shared\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	first := loaded.ForAgent("react")
	first.SysPrompt = "changed"
	first.LLMConfig.Model = "changed-model"
	delete(first.AllowTools, ReadFileMCPToolName)
	second := first.ForAgent("observe")
	if second.SysPrompt != "shared" || second.LLMConfig.Model != "base-model" {
		t.Fatalf("configuration mutation leaked: %+v", second)
	}
	if _, ok := second.AllowTools[ReadFileMCPToolName]; ok {
		t.Fatal("base tools leaked into internal agent")
	}
	second.LLMConfig.Model = "changed-observer-model"
	second.SysPrompt = "changed-observer-prompt"
	second.AllowTools[ApplyPatchMCPToolName] = struct{}{}
	third := second.ForAgent("react")
	if third.LLMConfig.Model != "base-model" || third.SysPrompt != "shared" {
		t.Fatalf("configuration mutation leaked through chained lookup: %+v", third)
	}
	if len(third.AllowTools) != 1 {
		t.Fatalf("tool mutation leaked through chained lookup: %v", third.AllowTools)
	}
	if _, ok := third.AllowTools[ReadFileMCPToolName]; !ok {
		t.Fatal("original ReAct tools were lost")
	}
}

func TestLoadAgentConfigRejectsTrailingYAMLDocuments(t *testing.T) {
	const valid = "name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n"
	for _, tc := range []struct {
		name, suffix, want string
	}{
		{"second agent", "---\n" + valid, "exactly one YAML document"},
		{"unknown trailing field", "---\nextra: true\n", "exactly one YAML document"},
		{"empty trailing document", "---\n", "exactly one YAML document"},
		{"malformed trailing document", "---\nllm: [\n", "parse config"},
		{"trailing comments", "# trailing comment\n", ""},
		{"explicit document end", "...\n# trailing comment\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.yaml")
			if err := os.WriteFile(path, []byte(valid+tc.suffix), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadAgentConfig(path)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadAgentConfigRejectsInvalidDefinitions(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"unknown field", "name: react\nbase:\n  llm:\n    model: x\n    api_key: key\nextra: true\n", "field extra not found"},
		{"unknown agent", "name: planner\nbase:\n  llm:\n    model: x\n    api_key: key\n", "unknown agent type"},
		{"unknown tool", "name: react\nbase:\n  llm:\n    model: x\n    api_key: key\n  allow_tools: [shell]\n", "unknown tool"},
		{"conflicting keys", "name: react\nbase:\n  llm:\n    model: x\n    api_key: key\n    api_key_env: KEY\n", "mutually exclusive"},
		{"missing key", "name: react\nbase:\n  llm:\n    model: x\n", "API key is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAgentConfig(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}
