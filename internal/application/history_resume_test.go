package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
)

func TestResumeReplaysInterruptedHistoryAfterSystemMessage(t *testing.T) {
	db, store := runStore(t)
	ctx := context.Background()
	prior, err := store.Create(ctx, "", "historical goal", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&prior).Update("create_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	prior.Status = "interrupted"
	if err := store.SaveDurableCheckpoint(ctx, &prior, &state.DurableState{
		RunId: prior.RunID, ExecutionID: prior.RunID,
		ToolExecutions: []*state.ToolResult{
			{ToolCallId: "history-first", ToolName: "read_file", Arguments: `{"path":"a"}`, Status: "success", Result: "first historical result", Batch: 1},
			{ToolCallId: "history-second", ToolName: "read_file", Arguments: `{"path":"b"}`, Status: "failed", Error: "permission denied", Batch: 1, Position: 1},
		},
	}, "react"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(ctx, prior.ConvID, "continue current goal", prior.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDurableCheckpoint(ctx, &current, &state.DurableState{
		RunId: current.RunID, ExecutionID: current.RunID,
		ToolExecutions: []*state.ToolResult{{ToolCallId: "current-call", ToolName: "read_file", Arguments: `{}`, Status: "success", Result: "current result"}},
	}, "react"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Input []struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				CallID  string          `json:"call_id"`
				Output  string          `json:"output"`
				Content json.RawMessage `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		items := request.Input
		if len(items) != 8 {
			t.Errorf("want system, three call/output pairs, current state; got %+v", items)
		} else {
			if items[0].Role != "system" || items[7].Role != "user" {
				t.Errorf("wrong message order: %+v", items)
			}
			for i, id := range []string{"history-first", "history-second", "current-call"} {
				call, output := items[1+2*i], items[2+2*i]
				if call.Type != "function_call" || call.CallID != id || output.Type != "function_call_output" || output.CallID != id {
					t.Errorf("missing ordered call/output pair for %s: %+v", id, items)
				}
			}
			if items[2].Output != "first historical result" || !strings.Contains(items[4].Output, "permission denied") || items[6].Output != "current result" {
				t.Errorf("tool results lost: %+v", items)
			}
		}
		if len(items) > 0 && (strings.Contains(string(items[0].Content), "tool_interactions") || strings.Contains(string(items[0].Content), "first historical result")) {
			t.Error("confirmed tool interactions duplicated in system history")
		}
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	_, err = application.Resume(ctx, application.Options{RunID: current.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("want one model request, got %d", requests)
	}
}

func TestResumeKeepsHistoricalToolOwnershipAndUnknownOutcome(t *testing.T) {
	db, store := runStore(t)
	ctx := context.Background()
	prior, err := store.Create(ctx, "", "historical goal", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&prior).Update("create_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	prior.Status = "interrupted"
	if err := store.SaveDurableCheckpoint(ctx, &prior, &state.DurableState{
		RunId: prior.RunID, ExecutionID: prior.RunID,
		ToolExecutions: []*state.ToolResult{
			{ToolCallId: "confirmed-call", ToolName: "read_file", Arguments: `{}`, Status: "success", Result: "saved evidence"},
			{ToolCallId: "unknown-call", ToolName: "write_file", Arguments: `{"path":"a"}`},
		},
	}, "react"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(ctx, prior.ConvID, "continue", prior.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Input []struct {
				Role    string `json:"role"`
				CallID  string `json:"call_id"`
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Input) != 4 {
			t.Errorf("want system, confirmed call/output, user; got %d messages", len(request.Input))
		} else {
			system := request.Input[0].Content
			// The execution retains call IDs so detached native outputs remain attributable.
			for _, want := range []string{prior.RunID, `"tool_call_ids": [`, `"confirmed-call"`, `"unknown-call"`, "outcome unknown"} {
				if !strings.Contains(system, want) {
					t.Errorf("system history missing ownership/uncertainty %q", want)
				}
			}
			if strings.Contains(system, "saved evidence") || request.Input[1].CallID != "confirmed-call" || request.Input[2].CallID != "confirmed-call" {
				t.Error("confirmed evidence must occur only in its native call/output pair")
			}
		}
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	_, err = application.Resume(ctx, application.Options{RunID: current.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err != nil || requests != 1 {
		t.Fatalf("resume error=%v requests=%d", err, requests)
	}
}
