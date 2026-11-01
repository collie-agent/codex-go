package application_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/testutil"
)

func assertNewTaskHistory(t *testing.T, run database.AgentRun, expected ...string) {
	t.Helper()
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		requests <- string(body)
		writeResult(w, `{"status":"completed","answer":"new task done"}`)
	}))
	defer server.Close()
	_, err := application.Run(context.Background(), application.Options{ConfigPath: agentConfig(t, server.URL), Task: "new task", ConvID: run.ConvID, Workspace: run.Workspace, Timeout: time.Second, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	input := <-requests
	for _, value := range expected {
		if !strings.Contains(input, value) {
			t.Errorf("history missing %q", value)
		}
	}
}

func TestInterruptStopsModelAndPreventsResume(t *testing.T) {
	db, store := runStore(t)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	interrupt := application.NewInterrupt()
	workspace := t.TempDir()
	opts := application.Options{ConfigPath: agentConfig(t, server.URL), Task: "finish task", Workspace: workspace, Timeout: time.Second, Stderr: io.Discard, Interrupt: interrupt}
	done := make(chan error, 1)
	go func() { _, err := application.Run(context.Background(), opts); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	interrupt.Interrupt()
	select {
	case err := <-done:
		if !errors.Is(err, application.ErrInterrupted) {
			t.Fatalf("interrupt error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	var run database.AgentRun
	if err := db.Where("workspace = ?", workspace).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != "interrupted" {
		t.Fatalf("status = %q", run.Status)
	}
	assertNewTaskHistory(t, run, "finish task", run.RunID)
	if _, err := application.Resume(context.Background(), application.Options{ConfigPath: opts.ConfigPath, RunID: run.RunID, Timeout: time.Second}); err == nil {
		t.Fatal("interrupted run resumed")
	}
	lock, err := store.AcquireRunLock(context.Background(), run.RunID)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = lock.Release(context.Background())
}

func TestConsecutiveInterruptsDoNotDuplicateInheritedHistory(t *testing.T) {
	db, _ := runStore(t)
	workspace := t.TempDir()
	var conv string
	for _, goal := range []string{"first-interrupted-goal", "second-interrupted-goal"} {
		started := make(chan string, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if testutil.RespondWithCriteria(t, w, r) {
				return
			}
			body, _ := io.ReadAll(r.Body)
			started <- string(body)
			<-r.Context().Done()
		}))
		interrupt := application.NewInterrupt()
		done := make(chan error, 1)
		go func() {
			_, err := application.Run(context.Background(), application.Options{ConfigPath: agentConfig(t, server.URL), ConvID: conv, Task: goal, Workspace: workspace, Timeout: time.Second, Stderr: io.Discard, Interrupt: interrupt})
			done <- err
		}()
		select {
		case input := <-started:
			if conv != "" && !strings.Contains(input, "first-interrupted-goal") {
				t.Error("second run did not receive the first history")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("model did not start")
		}
		interrupt.Interrupt()
		if err := <-done; !errors.Is(err, application.ErrInterrupted) {
			t.Fatalf("interrupt error: %v", err)
		}
		server.Close()
		var saved database.AgentRun
		if err := db.Where("workspace = ? AND goal = ?", workspace, goal).First(&saved).Error; err != nil {
			t.Fatal(err)
		}
		conv = saved.ConvID
	}
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		requests <- string(body)
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	_, err := application.Run(context.Background(), application.Options{ConfigPath: agentConfig(t, server.URL), ConvID: conv, Task: "final goal", Workspace: workspace, Timeout: time.Second, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	input := <-requests
	for _, goal := range []string{"first-interrupted-goal", "second-interrupted-goal"} {
		if n := strings.Count(input, goal); n != 1 {
			t.Errorf("history goal %q occurs %d times", goal, n)
		}
	}
}

func TestInterruptDuringPlanStepPreservesCompletedStep(t *testing.T) {
	_, store := runStore(t)
	run, err := store.Create(context.Background(), "", "finish both steps", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, StepExecutions: []state.StepExecution{
		{StepId: "first", Revision: 1, Status: state.CompletedExecutionStatus, Intent: "first", CompletionCriteria: "done", CompletionEvidence: []string{"first evidence"}},
		{StepId: "second", Revision: 1, Status: state.PendingExecutionStatus, Intent: "second", CompletionCriteria: "done"},
	}}
	if err := store.SaveDurableCheckpoint(context.Background(), &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	interrupt := application.NewInterrupt()
	done := make(chan error, 1)
	go func() {
		_, err := application.Resume(context.Background(), application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard, Interrupt: interrupt})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("step did not start")
	}
	interrupt.Interrupt()
	interrupt.Interrupt()
	select {
	case err := <-done:
		if !errors.Is(err, application.ErrInterrupted) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("plan did not stop")
	}
	saved, err := store.Get(context.Background(), run.RunID)
	if err != nil || saved.Status != "interrupted" {
		t.Fatalf("saved=%+v error=%v", saved, err)
	}
	steps, err := store.Steps(context.Background(), run.RunID)
	if err != nil || len(steps) != 2 || steps[0].Status != "completed" || len(steps[0].CompletionEvidence) != 1 || steps[0].CompletionEvidence[0] != "first evidence" {
		t.Fatalf("steps=%+v error=%v", steps, err)
	}
	assertNewTaskHistory(t, saved, "finish both steps", "first evidence", "second")
}

func TestInterruptStopsDelegatedWorkers(t *testing.T) {
	db, _ := runStore(t)
	started := make(chan struct{}, 2)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := requests.Add(1)
		if n == 1 {
			writeResult(w, `{"entry_completion_criteria":["checked"],"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"A","completion_criteria":["A checked"]},{"agent":"worker","task":"B","completion_criteria":["B checked"]}]}`)
			return
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 5\n  max_parallel: 2\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    next_agents: [worker]\n  worker:\n    next_agents: [supervisor]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	interrupt := application.NewInterrupt()
	done := make(chan error, 1)
	go func() {
		_, err := application.Run(context.Background(), application.Options{ConfigPath: path, Task: "check", Workspace: workspace, Timeout: time.Second, Stderr: io.Discard, Interrupt: interrupt})
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("workers did not start: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	interrupt.Interrupt()
	select {
	case err := <-done:
		if !errors.Is(err, application.ErrInterrupted) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not stop")
	}
	var run database.AgentRun
	if err := db.Where("workspace = ?", workspace).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != "interrupted" || requests.Load() != 3 {
		t.Fatalf("status=%q model calls=%d", run.Status, requests.Load())
	}
	assertNewTaskHistory(t, run, "check", "A checked", "B checked", "worker")
}

func TestInterruptConcurrentToolsKeepsUnknownOutcomes(t *testing.T) {
	db, store := runStore(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"tools","status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"a","name":"exec","arguments":"{\"command\":\"python3\",\"args\":[\"-c\",\"import pathlib,time; pathlib.Path('a').touch(); time.sleep(20)\"]}"},{"type":"function_call","id":"fc_b","call_id":"b","name":"exec","arguments":"{\"command\":\"python3\",\"args\":[\"-c\",\"import pathlib,time; pathlib.Path('b').touch(); time.sleep(20)\"]}"}]}`)
	}))
	defer server.Close()
	path := agentConfig(t, server.URL)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("  react:\n    allow_tools: [exec]\n")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	interrupt := application.NewInterrupt()
	done := make(chan error, 1)
	go func() {
		_, err := application.Run(context.Background(), application.Options{ConfigPath: path, Task: "run tools", Workspace: workspace, Timeout: 30 * time.Second, Stderr: io.Discard, Interrupt: interrupt})
		done <- err
	}()
	deadline := time.After(10 * time.Second)
	for {
		_, a := os.Stat(filepath.Join(workspace, "a"))
		_, b := os.Stat(filepath.Join(workspace, "b"))
		if a == nil && b == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("tools did not start: %v", err)
		case <-deadline:
			t.Fatal("tools did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	interrupt.Interrupt()
	select {
	case err := <-done:
		if !errors.Is(err, application.ErrInterrupted) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tools did not stop")
	}
	var run database.AgentRun
	if err := db.Where("workspace = ?", workspace).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	saved, err := store.Execution(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "interrupted" || len(saved.ToolExecutions) != 2 || saved.ToolExecutions[0].Status != "" || saved.ToolExecutions[1].Status != "" || requests.Load() != 1 {
		t.Fatalf("status=%s tools=%+v model calls=%d", run.Status, saved.ToolExecutions, requests.Load())
	}
}

func TestLateInterruptDoesNotOverwriteCompletedRun(t *testing.T) {
	db, store := runStore(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeResult(w, `{"status":"completed","answer":"finished","Observation":[],"CompletionEvidence":[]}`)
	}))
	defer server.Close()
	interrupt := application.NewInterrupt()
	workspace := t.TempDir()
	path := agentConfig(t, server.URL)
	results, err := application.Run(context.Background(), application.Options{ConfigPath: path, Task: "finish", Workspace: workspace, Timeout: time.Second, Stderr: io.Discard, Interrupt: interrupt})
	if err != nil || len(results) != 1 || results[0].Answer != "finished" {
		t.Fatalf("results=%v error=%v", results, err)
	}
	interrupt.Interrupt()
	var run database.AgentRun
	if err := db.Where("workspace = ?", workspace).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(context.Background(), run.RunID)
	if err != nil || saved.Status != "completed" {
		t.Fatalf("saved=%+v error=%v", saved, err)
	}
	resumed, err := application.Resume(context.Background(), application.Options{ConfigPath: path, RunID: run.RunID, Timeout: time.Second})
	if err != nil || len(resumed) != 1 || resumed[0].Answer != "finished" || requests.Load() != 1 {
		t.Fatalf("resumed=%v error=%v requests=%d", resumed, err, requests.Load())
	}
}

func TestInterruptReportsFailureToSaveStatus(t *testing.T) {
	db, _ := runStore(t)
	run, err := database.NewRunStore(db).Create(context.Background(), "", "finish", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	trigger := "interrupt_failure_" + run.RunID
	if err := db.Exec("CREATE TRIGGER " + trigger + " BEFORE UPDATE ON agent_run FOR EACH ROW BEGIN IF NEW.run_id = '" + run.RunID + "' AND NEW.status = 'interrupted' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'interrupt save failed'; END IF; END").Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER IF EXISTS " + trigger)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	interrupt := application.NewInterrupt()
	done := make(chan error, 1)
	go func() {
		_, err := application.Resume(context.Background(), application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard, Interrupt: interrupt})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	interrupt.Interrupt()
	select {
	case err := <-done:
		var failure *application.InterruptPersistenceError
		if !errors.Is(err, application.ErrInterrupted) || !errors.As(err, &failure) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
}
