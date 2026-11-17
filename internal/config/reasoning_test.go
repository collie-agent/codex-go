package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

func TestLoadReasoningEffort(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test")
	path := filepath.Join(t.TempDir(), "agent.yaml")
	err := os.WriteFile(path, []byte(`name: react
base:
  llm:
    model: test
    reasoning_effort: high
agents:
  compact:
    llm:
      model: compact
      reasoning_effort: low
`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMConfig.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q", cfg.LLMConfig.ReasoningEffort)
	}
	if cfg.ForAgent("compact").LLMConfig.ReasoningEffort != "low" {
		t.Fatal("agent reasoning override not applied")
	}
}
