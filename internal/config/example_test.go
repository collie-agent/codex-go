package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReactAgentExampleLoads(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "example-test-key")
	t.Setenv("ANALYZER_API_KEY", "analyzer-test-key")
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
	if got.Name != "react" {
		t.Fatalf("example selects %q, want react", got.Name)
	}
	react := got.ForAgent("react")
	if len(react.AllowTools) == 0 {
		t.Fatal("example should keep the ReAct tools enabled for the CLI example")
	}
	analyzer := got.ForAgent("task_analyzer")
	if analyzer.LLMConfig.Model != "analyzer-model" || analyzer.LLMConfig.ApiKey != "analyzer-test-key" {
		t.Fatalf("example should demonstrate an independent analyzer LLM, got %+v", analyzer.LLMConfig)
	}
	if analyzer.SysPrompt != "分析器专用约束" {
		t.Fatalf("example should demonstrate an independent analyzer prompt, got %q", analyzer.SysPrompt)
	}
	if len(analyzer.AllowTools) != 0 {
		t.Fatalf("example should demonstrate an explicitly empty tool list, got %v", analyzer.AllowTools)
	}
	if got.ForAgent("observe").SysPrompt != "" {
		t.Fatal("example should demonstrate an explicitly empty prompt")
	}
}
