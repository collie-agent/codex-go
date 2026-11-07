package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

func TestLoadLogDirectory(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{{"", "logs"}, {"log_dir: custom/logs\n", "custom/logs"}} {
		path := filepath.Join(t.TempDir(), "agent.yaml")
		if err := os.WriteFile(path, []byte(tc.yaml+"name: react\nbase:\n  llm:\n    model: test\n    api_key: test\n"), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadAgentConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.LogDir != tc.want || cfg.ForAgent("react").LogDir != tc.want {
			t.Fatalf("log directory=%q, want %q", cfg.LogDir, tc.want)
		}
	}
}
