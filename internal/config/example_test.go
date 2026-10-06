package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReactAgentExampleLoads(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "example-test-key")
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "examples", "react-agent.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("example config is unavailable: %v", err)
	}
	got, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatalf("example config should load: %v", err)
	}
	if got.Name != "react" || got.SysPrompt == "" {
		t.Fatalf("example does not describe a ReAct prompt: %+v", got)
	}
}
