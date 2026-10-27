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
