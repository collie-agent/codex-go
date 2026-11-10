package database_test

import (
	"context"
	state "github.com/collie-agent/codex-go/internal/context"
	"testing"
)

func TestReactExecutionCheckpointRoundTrip(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "continue", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, ToolExecutions: []*state.ToolResult{{ToolCallId: "first", ToolName: "read_file", Arguments: `{"path":"a"}`, Result: `[]`, Status: "success"}, {ToolCallId: "second", ToolName: "exec", Arguments: `{"command":"go"}`}}}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Execution(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.ExecutionID != durable.ExecutionID || len(restored.ToolExecutions) != 2 || restored.ToolExecutions[0].ToolCallId != "first" || restored.ToolExecutions[1].Status != "" {
		t.Fatalf("restored=%+v", restored)
	}
}

func TestToolResultScopedLookup(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	run, err := store.Create(ctx, "", "lookup", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		durable := &state.DurableState{RunId: run.RunID, ExecutionID: id, ToolExecutions: []*state.ToolResult{{ToolCallId: "same", ToolName: "glob", Status: "success", Result: id}}}
		if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.ToolResult(ctx, run.RunID, "second", "same")
	if err != nil || result.Result != "second" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, scope := range [][3]string{{"wrong", "second", "same"}, {run.RunID, "wrong", "same"}, {run.RunID, "second", "missing"}} {
		if _, err := store.ToolResult(ctx, scope[0], scope[1], scope[2]); err == nil {
			t.Fatalf("unexpected match for %v", scope)
		}
	}
}
