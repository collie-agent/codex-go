package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestFailedToolCallingDoesNotConsumeExploration(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 10
	durable, _ := runCommandExploration(t, workspace, [][]any{
		modificationTool("missing", "read_file", map[string]any{"path": "missing.go"}),
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}),
		{observationMessage(`{"status":"failed","answer":"stop"}`)},
	}, policy)
	var saved struct{ Exploration struct{ Used int } }
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if len(durable.ToolExecutions) != 2 || durable.ToolExecutions[0].Status != "failed" || durable.ToolExecutions[1].Status != "success" {
		t.Fatalf("unexpected tool results: %+v", durable.ToolExecutions)
	}
	if saved.Exploration.Used != 1 {
		t.Fatalf("failed tool calling consumed exploration: used=%d, want 1", saved.Exploration.Used)
	}
}

func TestFailedInvestigationSettlement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		want   int
	}{{"all failed", "failed", 0}, {"mixed success", "success", 1}, {"unknown", "", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			e := explorationState{Used: 1, Accounting: []explorationAccounting{{Batch: 1, Charged: true, Outcome: "pending", Calls: map[string]string{"a": "investigation", "b": "investigation"}}}}
			completed := []*state.ToolResult{{Batch: 1, ToolCallId: "a", Status: "failed"}, {Batch: 1, ToolCallId: "b", Status: filemcp.ToolCallStatus(tc.status)}}
			e.reconcile(completed)
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			var restored explorationState
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			restored.reconcile(completed)
			if restored.Used != tc.want {
				t.Fatalf("used=%d, want %d", restored.Used, tc.want)
			}
		})
	}
}
