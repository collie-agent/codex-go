package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExplorationBudget(t *testing.T) {
	for _, tc := range []struct {
		name, settings   string
		initial, retries int
		invalid          bool
	}{
		{"defaults", "", 12, 1, false},
		{"override", "base_extra", 7, 0, false},
		{"invalid", "invalid", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n"
			if tc.settings == "base_extra" {
				text += "  exploration_budget:\n    initial_turns: 7\n    decision_retries: 3\nagents:\n  react:\n    exploration_budget:\n      decision_retries: 0\n"
			}
			if tc.invalid {
				text += "  exploration_budget:\n    initial_turns: 0\n"
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadAgentConfig(path)
			if tc.invalid {
				if err == nil {
					t.Fatal("expected invalid budget")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			policy := cfg.ForAgent("react").ExplorationBudget
			if policy.InitialTurns != tc.initial || policy.DecisionRetries != tc.retries {
				t.Fatalf("unexpected policy: %+v", policy)
			}
		})
	}
}
