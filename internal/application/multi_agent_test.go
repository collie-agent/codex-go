package application_test

import (
	"bytes"
	"context"
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
)

func TestMultiAgentYAMLRunAndCompletedResume(t *testing.T) {
	runStore(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeResult(w, `{"entry_completion_criteria":["project inspected with evidence"],"result":{"status":"completed","answer":"multi-agent final report"},"next":[]}`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 3\n  max_parallel: 1\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	results, err := application.Run(context.Background(), application.Options{ConfigPath: path, Task: "inspect project", Workspace: t.TempDir(), Timeout: time.Second, Stderr: &stderr})
	if err != nil || len(results) != 1 || results[0].Answer != "multi-agent final report" {
		t.Fatalf("results=%+v error=%v", results, err)
	}
	fields := strings.Fields(stderr.String())
	if len(fields) < 1 || !strings.HasPrefix(fields[0], "run_id=") {
		t.Fatalf("missing identity: %q", stderr.String())
	}
	runID := strings.TrimPrefix(fields[0], "run_id=")
	resumed, err := application.Resume(context.Background(), application.Options{ConfigPath: path, RunID: runID, Timeout: time.Second})
	if err != nil || len(resumed) != 1 || resumed[0].Answer != "multi-agent final report" {
		t.Fatalf("resumed=%+v error=%v", resumed, err)
	}
	if requests.Load() != 1 {
		t.Fatal("completed resume requested another model execution")
	}
}

func TestMultiAgentResumeAfterProviderFailure(t *testing.T) {
	runStore(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		writeResult(w, `{"entry_completion_criteria":["project inspected with evidence"],"result":{"status":"completed","answer":"recovered report"},"next":[]}`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 3\n  max_parallel: 1\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	_, err := application.Run(context.Background(), application.Options{ConfigPath: path, Task: "inspect project", Workspace: t.TempDir(), Timeout: time.Second, Stderr: &stderr})
	if err == nil {
		t.Fatal("provider failure did not stop run")
	}
	fields := strings.Fields(stderr.String())
	if len(fields) == 0 {
		t.Fatal("missing run identity")
	}
	runID := strings.TrimPrefix(fields[0], "run_id=")
	results, err := application.Resume(context.Background(), application.Options{ConfigPath: path, RunID: runID, Timeout: time.Second})
	if err != nil || len(results) != 1 || results[0].Answer != "recovered report" {
		t.Fatalf("results=%+v error=%v", results, err)
	}
}

func TestMultiAgentResumeSavedEntryResult(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "inspect project", t.TempDir(), "multi-agent")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, ExecutionResult: []byte(`{"entry_completion_criteria":["project inspected"],"result":{"status":"completed","answer":"saved report"},"next":[]}`)}
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "multi-agent"); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected model call", http.StatusBadRequest)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 3\n  max_parallel: 1\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	results, err := application.Resume(ctx, application.Options{ConfigPath: path, RunID: run.RunID, Timeout: time.Second})
	if err != nil || len(results) != 1 || results[0].Answer != "saved report" || requests.Load() != 0 {
		t.Fatalf("results=%v error=%v requests=%d", results, err, requests.Load())
	}
}

func TestMultiAgentBusinessFailureIsTerminal(t *testing.T) {
	_, store := runStore(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeResult(w, `{"entry_completion_criteria":["project inspected"],"result":{"status":"failed","answer":"source unavailable"},"next":[]}`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 3\n  max_parallel: 1\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	_, err := application.Run(context.Background(), application.Options{ConfigPath: path, Task: "inspect", Workspace: t.TempDir(), Timeout: time.Second, Stderr: &stderr})
	if err == nil || !strings.Contains(err.Error(), "source unavailable") {
		t.Fatalf("error=%v", err)
	}
	fields := strings.Fields(stderr.String())
	if len(fields) == 0 {
		t.Fatal("missing run ID")
	}
	runID := strings.TrimPrefix(fields[0], "run_id=")
	run, err := store.Get(context.Background(), runID)
	if err != nil || run.Status != "failed" || run.FailureType != "business" || !strings.Contains(string(run.Result), "source unavailable") {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	results, err := application.Resume(context.Background(), application.Options{ConfigPath: path, RunID: runID, Timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "source unavailable") || requests.Load() != 1 || len(results) != 1 || results[0].Status != "failed" || results[0].Answer != "source unavailable" {
		t.Fatalf("results=%+v error=%v requests=%d", results, err, requests.Load())
	}
}
