package application_test

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestResumeReactPreservesIncrementalCompactionAndOriginalEvidence(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "original compacted goal", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	run.CompletionCriteria = []string{"original criterion"}
	summary := agent.ExecutionCompaction{ExecutionID: run.RunID, ThroughBatch: 3, Summary: agent.ExecutionSummary{Facts: []agent.Evidence{{Text: "earlier evidence", SourceCallIDs: []string{"call_1"}}}}}
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, CompletionCriteria: run.CompletionCriteria, ExecutionCompaction: encoded}
	for batch := 1; batch <= 14; batch++ {
		durable.ToolExecutions = append(durable.ToolExecutions, &state.ToolResult{Batch: batch, ToolCallId: fmt.Sprintf("call_%d", batch), ToolName: "read_file", Arguments: `{"path":"evidence"}`, Status: "success", Result: fmt.Sprintf("original-batch-%d:", batch) + strings.Repeat("payload ", 1800)})
	}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err != nil {
		t.Fatal(err)
	}
	compactions, reasoning := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var request struct {
			Input []struct {
				Content string `json:"content"`
				Type    string `json:"type"`
				CallID  string `json:"call_id"`
				Output  string `json:"output"`
			} `json:"input"`
			Text struct {
				Format struct {
					Schema struct {
						Properties map[string]any `json:"properties"`
					} `json:"schema"`
				} `json:"format"`
			} `json:"text"`
		}
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if _, compact := request.Text.Format.Schema.Properties["Facts"]; compact {
			compactions++
			input := ""
			for _, item := range request.Input {
				if item.Type != "function_call_output" {
					input += item.Content
				}
			}
			if compactions == 1 {
				for _, want := range []string{"earlier evidence", "original-batch-4:", "original-batch-6:", "original compacted goal", "original criterion"} {
					if !strings.Contains(input, want) {
						t.Errorf("incremental input missing %q", want)
					}
				}
				for _, excluded := range []string{"original-batch-1:", "original-batch-3:", "original-batch-7:"} {
					if strings.Contains(input, excluded) {
						t.Errorf("incremental input repeats %q", excluded)
					}
				}
			}
			writeResult(w, `{"Facts":[{"Text":"earlier evidence","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)
			return
		}
		reasoning++
		if reasoning == 1 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"read","status":"completed","output":[{"type":"function_call","id":"fc_original","call_id":"read_original","name":"read_tool_result","arguments":"{\"call_id\":\"call_1\",\"offset\":0,\"limit\":17}"}]}`)
			return
		}
		found := false
		for _, item := range request.Input {
			if item.Type == "function_call_output" && item.CallID == "read_original" && strings.Contains(item.Output, "original-batch-1:") {
				found = true
			}
		}
		if !found {
			t.Error("resume cannot read original compacted evidence")
		}
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	result, err := application.Resume(ctx, application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err != nil || len(result) != 1 || result[0].Answer != "done" || reasoning != 2 || compactions < 1 {
		t.Fatalf("result=%+v compact=%d reasoning=%d err=%v", result, compactions, reasoning, err)
	}
	saved, err := store.Execution(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var savedSummary agent.ExecutionCompaction
	if err := json.Unmarshal(saved.ExecutionCompaction, &savedSummary); err != nil {
		t.Fatal(err)
	}
	if savedSummary.ThroughBatch < 6 || len(saved.ToolExecutions) != 15 || saved.ToolExecutions[0].Result != durable.ToolExecutions[0].Result {
		t.Fatalf("incomplete restored history: summary=%+v calls=%d", savedSummary, len(saved.ToolExecutions))
	}
}
