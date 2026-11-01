package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
)

func TestResumeReactExecutionBoundaries(t *testing.T) {
	for _, stage := range []string{"registered", "partial", "confirmed", "final", "business-failure"} {
		t.Run(stage, func(t *testing.T) {
			_, store := runStore(t)
			ctx := context.Background()
			run, err := store.Create(ctx, "", "continue original goal", t.TempDir(), "react")
			if err != nil {
				t.Fatal(err)
			}
			durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, CompletionCriteria: []string{"done"}, ToolExecutions: []*state.ToolResult{
				{ToolCallId: "first", ToolName: "read_file", Arguments: `{"path":"first"}`, Batch: 1},
				{ToolCallId: "second", ToolName: "read_file", Arguments: `{"path":"second"}`, Batch: 1, Position: 1},
			}}
			if stage != "registered" {
				durable.ToolExecutions[0].Status = "success"
				durable.ToolExecutions[0].Result = `[{"type":"text","text":"first result"}]`
			}
			if stage == "confirmed" || stage == "final" || stage == "business-failure" {
				durable.ToolExecutions[1].Status = "success"
				durable.ToolExecutions[1].Result = `[{"type":"text","text":"second result"}]`
			}
			if stage == "final" {
				durable.ExecutionResult = []byte(`{"status":"completed","answer":"saved answer"}`)
			}
			if stage == "business-failure" {
				durable.ExecutionResult = []byte(`{"status":"failed","answer":"cannot complete"}`)
			}
			if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				body, _ := io.ReadAll(r.Body)
				input := string(body)
				if !strings.Contains(input, "first result") || !strings.Contains(input, "second result") || strings.Index(input, "first result") > strings.Index(input, "second result") {
					t.Errorf("confirmed history missing or reordered: %s", input)
				}
				writeResult(w, `{"status":"completed","answer":"saved answer"}`)
			}))
			defer server.Close()
			opts := application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard}
			results, err := application.Resume(ctx, opts)
			switch stage {
			case "registered", "partial":
				if err == nil || !strings.Contains(err.Error(), "unknown tool outcome") || requests != 0 {
					t.Fatalf("results=%v error=%v requests=%d", results, err, requests)
				}
			case "business-failure":
				if err == nil || !strings.Contains(err.Error(), "cannot complete") || requests != 0 {
					t.Fatalf("error=%v requests=%d", err, requests)
				}
			default:
				want := 0
				if stage == "confirmed" {
					want = 1
				}
				if err != nil || len(results) != 1 || results[0].Answer != "saved answer" || requests != want {
					t.Fatalf("results=%v error=%v requests=%d", results, err, requests)
				}
			}
		})
	}
}

func TestReactCheckpointFailureDoesNotRepeatSideEffects(t *testing.T) {
	for _, stage := range []string{"registration", "result"} {
		t.Run(stage, func(t *testing.T) {
			db, store := runStore(t)
			ctx := context.Background()
			workspace := t.TempDir()
			run, err := store.Create(ctx, "", "create marker", workspace, "react")
			if err != nil {
				t.Fatal(err)
			}
			run.CompletionCriteria = []string{"marker created"}
			if err := store.Save(ctx, &run); err != nil {
				t.Fatal(err)
			}
			trigger := "fail_tool_" + run.RunID
			event := "INSERT"
			if stage == "result" {
				event = "UPDATE"
			}
			if err := db.Exec("CREATE TRIGGER " + trigger + " BEFORE " + event + " ON tool_execution FOR EACH ROW BEGIN IF NEW.run_id = '" + run.RunID + "' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'tool checkpoint unavailable'; END IF; END").Error; err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Exec("DROP TRIGGER IF EXISTS " + trigger) })
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"id": "tools", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "fc_write", "call_id": "write", "name": "apply_patch", "arguments": `{"patch":"--- /dev/null\n+++ marker\n@@ -0,0 +1 @@\n+created\n"}`}}})
			}))
			defer server.Close()
			path := agentConfig(t, server.URL)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("  react:\n    allow_tools: [apply_patch]\n")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			opts := application.Options{RunID: run.RunID, ConfigPath: path, Timeout: time.Second, Stderr: io.Discard}
			_, err = application.Resume(ctx, opts)
			if err == nil || !strings.Contains(err.Error(), "tool checkpoint unavailable") || requests != 1 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
			_, statErr := os.Stat(filepath.Join(workspace, "marker"))
			if stage == "registration" && !os.IsNotExist(statErr) {
				t.Fatalf("unregistered tool executed: %v", statErr)
			}
			if stage == "result" {
				if statErr != nil {
					t.Fatal(statErr)
				}
				db.Exec("DROP TRIGGER " + trigger)
				_, err = application.Resume(ctx, opts)
				if err == nil || !strings.Contains(err.Error(), "unknown tool outcome") || requests != 1 {
					t.Fatalf("unknown side effect retried: error=%v requests=%d", err, requests)
				}
			}
		})
	}
}

func TestTerminalReactBusinessFailureReturnsSavedResult(t *testing.T) {
	for _, stage := range []string{"model", "saved-instance", "plan-step"} {
		t.Run(stage, func(t *testing.T) {
			_, store := runStore(t)
			ctx := context.Background()
			mode := "react"
			if stage == "plan-step" {
				mode = "plan-execute"
			}
			run, err := store.Create(ctx, "", "finish", t.TempDir(), mode)
			if err != nil {
				t.Fatal(err)
			}
			failure := `{"status":"failed","answer":"cannot complete","Observation":["permission denied"],"CompletionEvidence":[]}`
			durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, CompletionCriteria: []string{"done"}}
			if stage == "saved-instance" {
				durable.ExecutionResult = []byte(failure)
			}
			if stage == "plan-step" {
				durable.ExecutionID = ""
				durable.StepExecutions = []state.StepExecution{
					{StepId: "first", Status: state.PendingExecutionStatus, Intent: "first", CompletionCriteria: "done"},
					{StepId: "second", Status: state.PendingExecutionStatus, Intent: "second", CompletionCriteria: "done"},
				}
			}
			if err := store.SaveDurableCheckpoint(ctx, &run, durable, mode); err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if stage == "plan-step" && requests == 1 {
					writeResult(w, `{"status":"completed","answer":"previous step","Observation":["first done"],"CompletionEvidence":["verified"]}`)
					return
				}
				writeResult(w, failure)
			}))
			defer server.Close()
			opts := application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard}
			for attempt := 0; attempt < 2; attempt++ {
				results, err := application.Resume(ctx, opts)
				if err == nil || !strings.Contains(err.Error(), "cannot complete") || len(results) != 1 || results[0].Status != agent.FailedReactStatus || results[0].Answer != "cannot complete" || len(results[0].Observation) != 1 {
					t.Fatalf("attempt=%d results=%+v err=%v", attempt, results, err)
				}
				saved, err := store.Get(ctx, run.RunID)
				if err != nil || saved.Status != "failed" || saved.FailureType != "business" || !strings.Contains(string(saved.Result), "permission denied") {
					t.Fatalf("saved=%+v error=%v", saved, err)
				}
			}
			wantRequests := 1
			if stage == "plan-step" {
				wantRequests = 2
			}
			if stage == "saved-instance" {
				wantRequests = 0
			}
			if requests != wantRequests {
				t.Fatalf("requests=%d want=%d", requests, wantRequests)
			}
		})
	}
}
