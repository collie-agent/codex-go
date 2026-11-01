package application_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
)

func TestPlanResumeUsesUnfinishedStepHistory(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "finish both steps", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID + "-step-instance", StepExecutions: []state.StepExecution{
		{StepId: "first", Revision: 1, Status: state.CompletedExecutionStatus, Intent: "first task", CompletionCriteria: "first done", CompletionEvidence: []string{"first evidence"}},
		{StepId: "second", Revision: 1, Status: state.RunningExecutionStatus, Intent: "second task", CompletionCriteria: "second done", ExecutionID: run.RunID + "-step-instance"},
	}, CurrentStepIndex: 1, ToolExecutions: []*state.ToolResult{{ToolCallId: "read", ToolName: "read_file", Arguments: `{"path":"second"}`, Status: "success", Result: `[{"type":"text","text":"saved second history"}]`}}}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "saved second history") {
			t.Errorf("step history missing: %s", body)
		}
		writeResult(w, `{"status":"completed","answer":"second complete","Observation":["second done"],"CompletionEvidence":["second evidence"]}`)
	}))
	defer server.Close()
	results, err := application.Resume(ctx, application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err != nil || len(results) != 1 || results[0].Answer != "second complete" || requests != 1 {
		t.Fatalf("results=%v err=%v requests=%d", results, err, requests)
	}
	steps, err := store.Steps(ctx, run.RunID)
	if err != nil || len(steps) != 2 || steps[0].CompletionEvidence[0] != "first evidence" || steps[1].Status != "completed" {
		t.Fatalf("steps=%v err=%v", steps, err)
	}
}

func TestPlanResumeStopsOnUnknownStepToolOutcome(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "finish task", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID + "-unknown-step", StepExecutions: []state.StepExecution{{StepId: "step", ExecutionID: run.RunID + "-unknown-step", Revision: 1, Status: state.RunningExecutionStatus, Intent: "finish", CompletionCriteria: "done"}}, ToolExecutions: []*state.ToolResult{{ToolCallId: "write", ToolName: "apply_patch", Arguments: `{}`}}}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	_, err = application.Resume(ctx, application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "unknown tool outcome") || requests != 0 {
		t.Fatalf("err=%v requests=%d", err, requests)
	}
}

func TestPlanResumeFinishesSavedStepResultWithoutModel(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "finish task", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID + "-finished-step", ExecutionResult: []byte(`{"status":"completed","answer":"saved result","Observation":["saved observation"],"CompletionEvidence":["saved evidence"]}`), StepExecutions: []state.StepExecution{{StepId: "step", ExecutionID: run.RunID + "-finished-step", Revision: 1, Status: state.RunningExecutionStatus, Intent: "finish", CompletionCriteria: "done"}}}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeResult(w, `{"status":"completed","answer":"unexpected"}`)
	}))
	defer server.Close()
	opts := application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard}
	results, err := application.Resume(ctx, opts)
	if err != nil || len(results) != 1 || results[0].Answer != "saved result" || requests != 0 {
		t.Fatalf("results=%v err=%v requests=%d", results, err, requests)
	}
	steps, err := store.Steps(ctx, run.RunID)
	if err != nil || len(steps) != 1 || steps[0].Status != "completed" || len(steps[0].CompletionEvidence) != 1 || steps[0].CompletionEvidence[0] != "saved evidence" {
		t.Fatalf("steps=%v err=%v", steps, err)
	}
}

func TestPlanNextStepDoesNotReceivePreviousStepToolHistory(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "finish both steps", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID + "-first-step", StepExecutions: []state.StepExecution{
		{StepId: "first", ExecutionID: run.RunID + "-first-step", Revision: 1, Status: state.CompletedExecutionStatus, Intent: "first", CompletionCriteria: "done", CompletionEvidence: []string{"checked"}},
		{StepId: "second", Revision: 1, Status: state.PendingExecutionStatus, Intent: "second", CompletionCriteria: "done"},
	}, ToolExecutions: []*state.ToolResult{{ToolCallId: "read", ToolName: "read_file", Arguments: `{}`, Status: "success", Result: `[{"type":"text","text":"private first tool history"}]`}}}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "private first tool history") {
			t.Error("previous step tool history leaked")
		}
		writeResult(w, `{"status":"completed","answer":"done","Observation":["done"],"CompletionEvidence":["checked"]}`)
	}))
	defer server.Close()
	_, err = application.Resume(ctx, application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err != nil || requests != 1 {
		t.Fatalf("err=%v requests=%d", err, requests)
	}
	steps, err := store.Steps(ctx, run.RunID)
	if err != nil || len(steps) != 2 || steps[1].ExecutionID == "" || steps[1].ExecutionID == steps[0].ExecutionID {
		t.Fatalf("steps=%v err=%v", steps, err)
	}
}

func TestCriteriaAndControlResultCommitTogether(t *testing.T) {
	db, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "finish task", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	trigger := "control_fail_" + run.RunID
	sql := "CREATE TRIGGER " + trigger + " BEFORE UPDATE ON tool_execution FOR EACH ROW BEGIN IF NEW.run_id = '" + run.RunID + "' AND NEW.name = 'update_completion_criteria' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'control result unavailable'; END IF; END"
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER " + trigger)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"control","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"criteria","name":"update_completion_criteria","arguments":"{\"completion_criteria\":[\"task done\"]}"}]}`)
	}))
	defer server.Close()
	_, err = application.Resume(ctx, application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "control result unavailable") {
		t.Fatalf("err=%v", err)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil || len(saved.CompletionCriteria) != 0 {
		t.Fatalf("criteria=%v err=%v", saved.CompletionCriteria, err)
	}
}

func TestPlanUpgradeResumesAfterPlannerFailure(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "implement and verify", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	run.CompletionCriteria = []string{"task verified"}
	if err := store.Save(ctx, &run); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"switch","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"switch","name":"switch_to_plan_execute","arguments":"{\"reason\":\"dependent work\",\"summary\":\"API verified; implementation remains\"}"}]}`)
		case 2:
			http.Error(w, "planner unavailable", http.StatusBadRequest)
		case 3:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "API verified") {
				t.Error("planner missing handoff")
			}
			writeResult(w, `{"StepExecutions":[{"Revision":1,"StepId":"implement","Status":"pending","Intent":"implement","CompletionCriteria":"verified","Observation":[],"CompletionEvidence":[]}]}`)
		default:
			writeResult(w, `{"status":"completed","answer":"finished","Observation":["implemented"],"CompletionEvidence":["verified"]}`)
		}
	}))
	defer server.Close()
	opts := application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard}
	_, err = application.Resume(ctx, opts)
	if err == nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil || saved.Mode != "plan-execute" || !strings.Contains(saved.PlanningContext, "API verified") {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	results, err := application.Resume(ctx, opts)
	if err != nil || len(results) != 1 || results[0].Answer != "finished" || calls != 4 {
		t.Fatalf("results=%v err=%v calls=%d", results, err, calls)
	}
}

func TestPlanUpgradeHandoffAndControlResultCommitTogether(t *testing.T) {
	db, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "implement", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	run.CompletionCriteria = []string{"verified"}
	if err := store.Save(ctx, &run); err != nil {
		t.Fatal(err)
	}
	trigger := "switch_fail_" + run.RunID
	sql := "CREATE TRIGGER " + trigger + " BEFORE UPDATE ON tool_execution FOR EACH ROW BEGIN IF NEW.run_id = '" + run.RunID + "' AND NEW.name = 'switch_to_plan_execute' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'switch result unavailable'; END IF; END"
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER " + trigger)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"switch","status":"completed","output":[{"type":"function_call","id":"fc","call_id":"switch","name":"switch_to_plan_execute","arguments":"{\"reason\":\"dependent work\",\"summary\":\"implementation remains\"}"}]}`)
	}))
	defer server.Close()
	_, err = application.Resume(ctx, application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "switch result unavailable") || requests != 1 {
		t.Fatalf("err=%v requests=%d", err, requests)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil || saved.Mode != "react" || saved.PlanningContext != "" {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
}
