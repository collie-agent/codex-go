package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestMultiAgentResumeDuringNestedWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var supervisor, worker1, worker2 atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		// Inspect only the current task: reports also contain other worker names.
		_, task, _ := strings.Cut(req.Input, `"delegated_task":`)
		task, _, _ = strings.Cut(task, `,"reports":`)
		switch {
		case strings.Contains(task, `"worker":"worker2"`):
			if worker2.Add(1) == 1 {
				cancel() // Simulate process cancellation while the leaf is executing.
				<-r.Context().Done()
				return
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"review done"},"next":[]}`)
		case strings.Contains(task, `"worker":"worker1"`):
			worker1.Add(1)
			if strings.Contains(req.Input, `"reports":[{`) {
				multiResponse(w, `{"result":{"status":"completed","answer":"comparison done"},"next":[]}`)
			} else {
				multiResponse(w, `{"result":{"status":"completed","answer":"request review"},"next":[{"agent":"worker2","task":"review","completion_criteria":["accurate"]}]}`)
			}
		default:
			supervisor.Add(1)
			if strings.Contains(req.Input, `"reports":[{`) {
				multiResponse(w, `{"result":{"status":"completed","answer":"final"},"next":[]}`)
			} else {
				multiResponse(w, `{"result":{"status":"completed","answer":"start comparison"},"next":[{"agent":"worker1","task":"compare","completion_criteria":["reviewed"]}]}`)
			}
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 15\n  max_parallel: 1\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise\n    next_agents: [worker1]\n  worker1:\n    instruction: compare\n    next_agents: [worker2, supervisor]\n  worker2:\n    instruction: review\n    next_agents: [worker1]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filemcp.Config{Workspace: t.TempDir()}
	var saved *state.DurableState
	save := func(s *state.DurableState, _ string) error { saved = s.Clone(); return nil }
	o := NewMultiAgentProcessorOrchestrator(ctx, workspace)
	o.SetCheckpoint(save)
	if _, err := o.Run(cfg, &state.DurableState{RunId: "nested", Goal: "compare", CompletionCriteria: []string{"done"}}); err == nil {
		t.Fatal("expected cancellation")
	}
	if saved == nil || worker2.Load() != 1 {
		t.Fatal("did not reach nested worker")
	}
	// Round-trip the snapshot to exercise process restart, not in-memory reuse.
	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	var restored state.DurableState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	o = NewMultiAgentProcessorOrchestrator(t.Context(), workspace)
	o.SetCheckpoint(save)
	results, err := o.Run(cfg, &restored)
	if err != nil || len(results) != 1 || results[0].Result.Answer != "final" {
		t.Fatalf("results=%+v error=%v", results, err)
	}
	// Each parent has one initial call and one continuation after its child.
	if supervisor.Load() != 2 || worker1.Load() != 2 || worker2.Load() != 2 {
		t.Fatalf("repeated parent work: supervisor=%d worker1=%d worker2=%d", supervisor.Load(), worker1.Load(), worker2.Load())
	}
}
