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
