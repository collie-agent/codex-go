package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExplorationExtensionDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.ForAgent("react").ExplorationBudget
	if policy.ExtensionTurns != 4 || policy.MaxExtensions != 2 || policy.ModificationWindowTurns != 3 {
		t.Fatalf("unexpected defaults: %+v", policy)
	}
}

func TestLoadExplorationExtensionOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	text := "name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n  exploration_budget:\n    extension_turns: 6\n    max_extensions: 3\n    modification_window_turns: 5\nagents:\n  react:\n    exploration_budget:\n      max_extensions: 0\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.ForAgent("react").ExplorationBudget
	if policy.ExtensionTurns != 6 || policy.MaxExtensions != 0 || policy.ModificationWindowTurns != 5 {
		t.Fatalf("unexpected overrides: %+v", policy)
	}
}

func TestLoadExplorationExtensionRejectsInvalidLimits(t *testing.T) {
	for _, setting := range []string{"extension_turns: 0", "extension_turns: -1", "max_extensions: -1", "modification_window_turns: 0", "modification_window_turns: -1"} {
		t.Run(setting, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			text := "name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n  exploration_budget:\n    " + setting + "\n"
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadAgentConfig(path); err == nil {
				t.Fatal("expected invalid policy to be rejected")
			}
		})
	}
}

func TestExplorationBudgetUsesRequestedAgentRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	text := "name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n  exploration_budget:\n    extension_turns: 6\n    max_extensions: 3\nagents:\n  react:\n    exploration_budget:\n      max_extensions: 0\n  observe:\n    exploration_budget:\n      extension_turns: 2\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	react, observe := cfg.ForAgent("react").ExplorationBudget, cfg.ForAgent("observe").ExplorationBudget
	if react.MaxExtensions != 0 || react.ExtensionTurns != 6 || observe.MaxExtensions != 3 || observe.ExtensionTurns != 2 {
		t.Fatalf("react=%+v observe=%+v", react, observe)
	}
}
