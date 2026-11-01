package database_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
)

func TestMigrationPreservesNewRuns(t *testing.T) {
	url := os.Getenv("CODEX_TEST_MYSQL_URL")
	if url == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := database.NewRunStore(db)
	run, err := store.Create(ctx, "", "inspect repository", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ConvID == "" || saved.RunID == "" || saved.Status != "pending" || saved.Goal != "inspect repository" || saved.Mode != "react" || saved.Workspace != run.Workspace || saved.CreateAt.IsZero() || saved.UpdateAt.IsZero() {
		t.Fatalf("new run lost its recovery data: %+v", saved)
	}
	conv, err := store.Conversation(ctx, run.ConvID)
	if err != nil || conv.CreateAt.IsZero() || conv.UpdateAt.IsZero() {
		t.Fatalf("conversation=%+v, error=%v", conv, err)
	}
}

func TestNewTasksHaveIndependentRunsInExistingConversation(t *testing.T) {
	store := testStore(t)
	first, err := store.Create(context.Background(), "", "first", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(context.Background(), first.ConvID, "second", first.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	if second.RunID == first.RunID || second.ConvID != first.ConvID {
		t.Fatalf("incorrect run identity: %+v, %+v", first, second)
	}
	if _, err := store.Create(context.Background(), "nonexistent-conversation", "third", first.Workspace, "react"); err == nil {
		t.Fatal("accepted nonexistent conversation")
	}
}

func testStore(t *testing.T) *database.RunStore {
	t.Helper()
	url := os.Getenv("CODEX_TEST_MYSQL_URL")
	if url == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	db, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return database.NewRunStore(db)
}

func TestRunRetainsCompleteResultAndRecoveryCriteria(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "inspect", t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "running"
	run.Mode = "react"
	run.CompletionCriteria = []string{"repository inspected"}
	if err := store.Save(ctx, &run); err != nil {
		t.Fatal(err)
	}
	running, err := store.Get(ctx, run.RunID)
	if err != nil || running.Status != "running" {
		t.Fatalf("running=%+v, err=%v", running, err)
	}
	run.Status = "completed"
	run.Result = []byte(`{"status":"completed","answer":"done","Observation":["found README"],"CompletionEvidence":["read README"]}`)
	if err := store.Save(ctx, &run); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Answer                          string
		Observation, CompletionEvidence []string
	}
	if err := json.Unmarshal(saved.Result, &result); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "completed" || saved.Mode != "react" || len(saved.CompletionCriteria) != 1 || saved.CompletionCriteria[0] != "repository inspected" || result.Answer != "done" || len(result.Observation) != 1 || result.Observation[0] != "found README" || len(result.CompletionEvidence) != 1 || result.CompletionEvidence[0] != "read README" || saved.UpdateAt.Before(saved.CreateAt) {
		t.Fatalf("saved=%+v, result=%+v", saved, result)
	}
}

func TestRunAllowsRepeatedUnchangedCheckpoints(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "continue original goal", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "running"
	// 同一毫秒内重复写相同业务状态仍是成功的 checkpoint。
	for range 20 {
		if err := store.Save(ctx, &run); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil || saved.Status != "running" {
		t.Fatalf("saved=%+v, err=%v", saved, err)
	}
}

func TestPlanCheckpointPersistsOrderedStepsAtomically(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "execute plan", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "running"
	steps := []database.StepExecution{
		{StepID: "zeta", Status: "pending", Intent: "second", CompletionCriteria: "second done", Revision: 1},
		{StepID: "alpha", Status: "pending", Intent: "first", CompletionCriteria: "first done", Revision: 1},
	}
	if err := store.SaveCheckpoint(ctx, &run, steps); err != nil {
		t.Fatal(err)
	}
	// Rewriting an identical checkpoint is still a successful save when MySQL
	// reports zero changed rows for the existing step records.
	for range 3 {
		if err := store.SaveCheckpoint(ctx, &run, steps); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := store.Steps(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 2 || saved[0].StepID != "zeta" || saved[0].Position != 0 || saved[1].StepID != "alpha" || saved[1].Position != 1 {
		t.Fatalf("steps=%+v", saved)
	}
	if saved[0].RunID != run.RunID || saved[1].RunID != run.RunID {
		t.Fatalf("steps lost run identity: %+v", saved)
	}
}

func TestPlanCheckpointRollsBackRunWhenStepWriteFails(t *testing.T) {
	url := os.Getenv("CODEX_TEST_MYSQL_URL")
	if url == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	db, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	store := database.NewRunStore(db)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "rollback failed plan", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	trigger := "reject_step_" + run.RunID
	if err := db.Exec("CREATE TRIGGER " + trigger + " BEFORE INSERT ON step_execution FOR EACH ROW BEGIN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'step checkpoint unavailable'; END").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Exec("DROP TRIGGER IF EXISTS " + trigger).Error })
	run.Status = "running"
	err = store.SaveCheckpoint(ctx, &run, []database.StepExecution{{
		RunID: run.RunID, StepID: "first", Status: "pending", Intent: "first step",
		CompletionCriteria: "first step done", Revision: 1,
	}})
	if err == nil || !strings.Contains(err.Error(), "step checkpoint unavailable") {
		t.Fatalf("SaveCheckpoint() error = %v, want step write failure", err)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := store.Steps(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "pending" || len(steps) != 0 {
		t.Fatalf("failed transaction partially committed: run=%+v steps=%+v", saved, steps)
	}
}

func TestFailedStepCheckpointPersistsRunErrorAtomically(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "record failed step", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "running"
	durable := &state.DurableState{
		RunId: run.RunID, Goal: run.Goal, Error: "tool execution failed",
		StepExecutions: []state.StepExecution{{
			StepId: "step-1", Status: state.FailedExecutionStatus, Intent: "run tool",
			CompletionCriteria: "tool succeeds", Revision: 1,
		}},
	}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := store.Steps(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Error != "tool execution failed" || len(steps) != 1 || steps[0].Status != "failed" {
		t.Fatalf("failure checkpoint did not persist run error and step together: run=%+v steps=%+v", saved, steps)
	}
}

func TestRunLockRejectsConcurrentOwnerAndReleases(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "locked", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AcquireRunLock(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release(ctx)
	otherRun, err := store.Create(ctx, run.ConvID, "independent lock", run.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	independent, err := store.AcquireRunLock(ctx, otherRun.RunID)
	if err != nil {
		t.Fatalf("different run was blocked by the first lock: %v", err)
	}
	if err := independent.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireRunLock(ctx, run.RunID); err == nil {
		t.Fatal("second owner acquired the run lock")
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := store.AcquireRunLock(ctx, run.RunID)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationUsesSpecifiedColumnsAndJSONTypes(t *testing.T) {
	url := os.Getenv("CODEX_TEST_MYSQL_URL")
	if url == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	db, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]map[string]string{
		"conversation":   {"conv_id": "varchar", "create_at": "datetime", "update_at": "datetime"},
		"agent_run":      {"multi_agent_checkpoint": "json", "failure_type": "varchar", "run_id": "varchar", "conv_id": "varchar", "goal": "longtext", "status": "varchar", "result": "json", "error": "longtext", "mode": "varchar", "completion_criteria": "json", "planning_context": "longtext", "workspace": "longtext", "create_at": "datetime", "update_at": "datetime"},
		"step_execution": {"execution_id": "varchar", "step_id": "varchar", "run_id": "varchar", "status": "varchar", "intent": "longtext", "completion_criteria": "longtext", "completion_evidence": "json", "observation": "json", "position": "bigint", "revision": "bigint", "create_at": "datetime", "update_at": "datetime"},
	} {
		// schema 是迁移命令的对外契约，独立列出预期列，防止 ORM 默认命名或软删除字段混入。
		columns, err := db.Migrator().ColumnTypes(table)
		if err != nil {
			t.Fatal(err)
		}
		if len(columns) != len(want) {
			t.Errorf("%s has %d columns, want %d", table, len(columns), len(want))
		}
		for _, column := range columns {
			if want[column.Name()] != column.DatabaseTypeName() {
				t.Errorf("unexpected column %s.%s: %s", table, column.Name(), column.DatabaseTypeName())
			}
		}
	}
}

func TestCheckpointUpgradesReactWithHandoff(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "implement feature", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, Goal: run.Goal}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "plan-execute"); err == nil {
		t.Fatal("upgrade without handoff accepted")
	}
	durable.PlanningContext = "Verified contract; implementation remains"
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Mode != "plan-execute" || saved.PlanningContext != durable.PlanningContext {
		t.Fatalf("lost upgrade: %+v", saved)
	}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err == nil {
		t.Fatal("downgrade accepted")
	}
}
