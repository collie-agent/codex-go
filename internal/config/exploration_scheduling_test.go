package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMultiAgentPolicySurvivesRoleSelectionAndCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := `name: multi-agent
multi-agent:
  entry: coordinator
  max_rounds: 10
  max_parallel: 2
base:
  llm:
    model: test
    api_key: test
  exploration_budget:
    initial_turns: 7
    extension_turns: 2
    max_extensions: 3
    decision_retries: 2
    modification_window_turns: 4
agents:
  coordinator:
    next_agents: [builder]
  builder:
    exploration_budget:
      initial_turns: 5
      max_extensions: 0
      decision_retries: 0
  react:
    exploration_budget:
      initial_turns: 99
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	builder := cfg.Copy().ForAgent("builder").Copy()
	expected := ExplorationBudget{InitialTurns: 5, ExtensionTurns: 2, MaxExtensions: 0, DecisionRetries: 0, ModificationWindowTurns: 4}
	if builder.ExplorationBudget != expected {
		t.Fatalf("builder=%+v", builder.ExplorationBudget)
	}
	builder.ExplorationBudget.InitialTurns = 100
	if got := cfg.Copy().ForAgent("builder").ExplorationBudget; got != expected {
		t.Fatalf("copy mutated original policy: %+v", got)
	}
	if got := cfg.ForAgent("coordinator").Copy().ExplorationBudget; got.InitialTurns != 7 || got.MaxExtensions != 3 || got.DecisionRetries != 2 {
		t.Fatalf("coordinator inherited wrong role: %+v", got)
	}
}
