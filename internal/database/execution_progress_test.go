package database_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	state "github.com/collie-agent/codex-go/internal/context"
)

func TestExecutionCheckpointRetainsDecisionBudget(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "continue", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	progress := []byte(`{"ExecutionID":"instance","UsedTurns":7,"Stale":3,"Recoveries":1,"RecoveryActive":true,"RecoveryLeft":2}`)
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: "instance", ExecutionProgress: progress}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err != nil {
		t.Fatal(err)
	}
	saved, err := store.ExecutionByID(ctx, run.RunID, "instance")
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(saved.ExecutionProgress, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(progress, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lost decision budget: %s", saved.ExecutionProgress)
	}
}
