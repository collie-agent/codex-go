package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandAcceptsLogDirectory(t *testing.T) {
	for _, entry := range []string{".", "./codex-cli", "./codex-consumer"} {
		t.Run(entry, func(t *testing.T) {
			command := exec.Command("go", "run", entry, "--log-dir", t.TempDir(), "--help")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("help failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "-log-dir") {
				t.Fatalf("missing log directory option: %s", output)
			}
		})
	}
}

func TestCommandErrorsGoToLogFile(t *testing.T) {
	for _, tc := range []struct {
		entry string
		args  []string
		want  string
	}{
		{".", []string{"--config", "/missing/agent.yaml"}, "/missing/agent.yaml"},
		{"./codex-cli", []string{"--config", "/missing/agent.yaml"}, "/missing/agent.yaml"},
		{"./codex-consumer", []string{"--config", "/missing/agent.yaml"}, "/missing/agent.yaml"},
		{"./mcp", nil, "--workspace is required"},
		{"./migrate", nil, "CODEX_MYSQL_URL is required"},
		{"./codex-server", []string{"--redis-url", "invalid"}, "redis"},
		{".", []string{"--unknown-option"}, "invalid command arguments"},
	} {
		t.Run(tc.entry+"/"+tc.want, func(t *testing.T) {
			dir := t.TempDir()
			args := append([]string{"run", tc.entry, "--log-dir", dir}, tc.args...)
			command := exec.Command("go", args...)
			command.Env = append(os.Environ(), "CODEX_MYSQL_URL=")
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatal("expected failure")
			}
			data, readErr := os.ReadFile(filepath.Join(dir, "codex.log"))
			if readErr != nil {
				t.Fatalf("missing error log: %v; output=%s", readErr, output)
			}
			if !strings.Contains(string(data), "[ERROR]") || !strings.Contains(string(data), tc.want) {
				t.Fatalf("invalid error log: %s", data)
			}
			if strings.Contains(string(output), tc.want) {
				t.Fatalf("error leaked to console: %s", output)
			}
		})
	}
}
