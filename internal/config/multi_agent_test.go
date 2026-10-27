package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMultiAgentExample(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test")
	cfg, err := LoadAgentConfig("../../examples/supervisor-worker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "multi-agent" || cfg.ForAgent("worker1").SysPrompt == "" || cfg.ForAgent("worker1").SysPrompt == cfg.SysPrompt {
		t.Fatalf("unexpected configuration: %+v", cfg)
	}
}

func TestMultiAgentConfigCopyIsIndependent(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test")
	cfg, err := LoadAgentConfig("../../examples/supervisor-worker.yaml")
	if err != nil {
		t.Fatal(err)
	}
	copy := cfg.Copy()
	copy.MultiAgent.Entry = "worker1"
	copy.MultiAgent.MaxParallel = 99
	worker := cfg.ForAgent("worker1")
	worker.NextAgents[0] = "worker2"
	worker.LLMConfig.Model = "changed"
	delete(worker.AllowTools, ReadFileMCPToolName)
	if cfg.MultiAgent.Entry != "supervisor" || cfg.MultiAgent.MaxParallel != 2 {
		t.Fatalf("copy changed original graph: %+v", cfg.MultiAgent)
	}
	original := cfg.ForAgent("worker1")
	if original.NextAgents[0] != "supervisor" || original.LLMConfig.Model == "changed" {
		t.Fatalf("worker config leaked: %+v", original)
	}
	if _, ok := original.AllowTools[ReadFileMCPToolName]; !ok {
		t.Fatal("worker tool mutation leaked")
	}
}

func TestMultiAgentNamedWorkerKeepsConfiguredTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 5\n  max_parallel: 1\nbase:\n  llm:\n    api_key: test\n    model: test\n  allow_tools: [read_file]\nagents:\n  supervisor:\n    next_agents: [plan]\n  plan:\n    instruction: inspect files\n    next_agents: [supervisor]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.ForAgent("plan").AllowTools[ReadFileMCPToolName]; !ok {
		t.Fatal("named worker lost inherited tool permissions")
	}
}

func TestAgentInstructionsIncludesAllConfiguredRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 5\n  max_parallel: 1\nbase:\n  llm:\n    api_key: test\n    model: test\n  sys_prompt: inherited role\nagents:\n  supervisor:\n    instruction: delegate\n    next_agents: [worker1]\n  worker1:\n    instruction: implement\\nverify\n  worker2:\n    sys_prompt: review\n  worker3: {}\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []AgentInstruction{{"supervisor", "delegate"}, {"worker1", `implement\nverify`}, {"worker2", "review"}, {"worker3", "inherited role"}}
	for _, current := range []*AgentConfig{cfg, cfg.Copy(), cfg.ForAgent("worker1")} {
		got := current.AgentInstructions()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("instructions = %#v, want %#v", got, want)
		}
		got[0].Instruction = "changed"
		if !reflect.DeepEqual(current.AgentInstructions(), want) {
			t.Fatal("caller mutated stored instructions")
		}
	}
}
