package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAgentConfigAppliesDefaultsAndResolvesEnvironmentKey(t *testing.T) {
	t.Setenv("YAML_AGENT_TEST_KEY", "secret-value")
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(`name: react
llm:
  model: gpt-test
  api_key_env: YAML_AGENT_TEST_KEY
allow_tools:
  - read_file
  - grep
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
}

func TestLoadAgentConfigRejectsTrailingYAMLDocuments(t *testing.T) {
	const valid = "name: react\nllm:\n  model: test\n  api_key: test\n"
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
		{"unknown field", "name: react\nllm:\n  model: x\n  api_key: key\nextra: true\n", "field extra not found"},
		{"unknown agent", "name: planner\nllm:\n  model: x\n  api_key: key\n", "unknown agent type"},
		{"unknown tool", "name: react\nllm:\n  model: x\n  api_key: key\nallow_tools: [shell]\n", "unknown tool"},
		{"conflicting keys", "name: react\nllm:\n  model: x\n  api_key: key\n  api_key_env: KEY\n", "mutually exclusive"},
		{"missing key", "name: react\nllm:\n  model: x\n", "API key is empty"},
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
