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

func TestFailedCommandDoesNotConsumeModificationWindow(t *testing.T) {
	for _, args := range []map[string]any{
		{"command": "sed", "args": []string{"-i", "s/old/new/", "missing.go"}},
		{"command": "git", "args": []string{"status", "--short"}},
		{"command": "go", "args": []string{"test", "missing.go"}},
	} {
		t.Run(args["command"].(string), func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, "main.go")
			if err := os.WriteFile(path, []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = 1
			policy.ModificationWindowTurns = 1
			durable, err := runCommandExploration(t, workspace, [][]any{
				modificationTool("read", "read_file", map[string]any{"path": "main.go"}),
				modificationHandoff(),
				modificationTool("failed-command", "exec", args),
				modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
				{observationMessage(`{"status":"completed","answer":"fixed"}`)},
			}, policy)
			if err != nil {
				t.Fatalf("failed command consumed modification window: %v", err)
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != "package main\n" || len(durable.ToolExecutions) != 3 {
				t.Fatalf("content=%q err=%v tools=%+v", content, err, durable.ToolExecutions)
			}
			var result struct {
				ExitCode *int `json:"exit_code"`
			}
			if !commandResultPayload(durable.ToolExecutions[1], &result) || result.ExitCode == nil || *result.ExitCode == 0 {
				t.Fatalf("command did not fail: %+v", durable.ToolExecutions[1])
			}
			var progress struct {
				UsedTurns   int
				Exploration struct{ Used int }
			}
			if err := json.Unmarshal(durable.ExecutionProgress, &progress); err != nil {
				t.Fatal(err)
			}
			if progress.UsedTurns != 5 || progress.Exploration.Used != 1 {
				t.Fatalf("unrelated budgets changed: %s", durable.ExecutionProgress)
			}
		})
	}
}

func TestFailedCommandWindowRefundSurvivesResume(t *testing.T) {
	for _, name := range []string{"exec", "exec_start", "exec_poll", "exec_stop"} {
		for _, tc := range []struct {
			name, payload string
			want          int
		}{
			{"failed exit", `{"state":"exited","exit_code":2}`, 1},
			{"successful exit", `{"state":"exited","exit_code":0}`, 0},
			{"running", `{"state":"running","exit_code":2}`, 0},
			{"missing exit", `{"state":"exited"}`, 0},
			{"malformed", `invalid`, 0},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				payload, _ := json.Marshal([]map[string]string{{"type": "text", "text": tc.payload}})
				e := explorationState{Phase: "implement", Accounting: []explorationAccounting{{Batch: 1, ModificationCharged: true, Calls: map[string]string{"call": "modification"}, Outcome: "known"}}}
				calls := []*state.ToolResult{{Batch: 1, ToolCallId: "call", ToolName: name, Status: filemcp.ToolCallSuceessStatus, Result: string(payload)}}
				e.settleFailedModification(&e.Accounting[0], calls)
				raw, err := json.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				var restored explorationState
				if err := json.Unmarshal(raw, &restored); err != nil {
					t.Fatal(err)
				}
				restored.reconcile(calls)
				restored.reconcile(calls)
				if restored.ModificationLeft != tc.want {
					t.Fatalf("remaining=%d want=%d", restored.ModificationLeft, tc.want)
				}
			})
		}
	}
}

func TestFailedModificationWindowSettlement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		want     int
	}{
		{"all failed", []string{filemcp.ToolCallFailedStatus, filemcp.ToolCallFailedStatus}, 1},
		{"partial success", []string{filemcp.ToolCallFailedStatus, filemcp.ToolCallSuceessStatus}, 0},
		{"incomplete", []string{filemcp.ToolCallFailedStatus}, 0},
		{"unknown", []string{filemcp.ToolCallFailedStatus, ""}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := explorationState{Phase: "implement", Accounting: []explorationAccounting{{Batch: 1, ModificationCharged: true, Outcome: "pending", Calls: map[string]string{"a": "modification", "b": "modification"}}}}
			var calls []*state.ToolResult
			for i, status := range tc.statuses {
				calls = append(calls, &state.ToolResult{Batch: 1, ToolCallId: []string{"a", "b"}[i], Status: filemcp.ToolCallStatus(status)})
			}
			e.settleFailedInvestigation(&e.Accounting[0], calls)
			e.reconcile(calls)
			e.reconcile(calls)
			if e.ModificationLeft != tc.want {
				t.Fatalf("remaining=%d, want %d", e.ModificationLeft, tc.want)
			}
		})
	}
}
