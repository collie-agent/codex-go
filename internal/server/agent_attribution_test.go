package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/entity"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/testutil"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func multiConfig(t *testing.T, endpoint string) *config.AgentConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 10\n  max_parallel: 2\nbase:\n  llm:\n    base_url: " + endpoint + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise-only\n    next_agents: [worker]\n  worker:\n    instruction: work-only\n    next_agents: [supervisor]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func multiResponse(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "multi", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}})
}

func decodeModelRequest(body io.Reader, target any) error {
	var request map[string]json.RawMessage
	if err := json.NewDecoder(body).Decode(&request); err != nil {
		return err
	}
	var items []struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(request["input"], &items) == nil && items != nil {
		var text string
		for _, item := range items {
			text += item.Content + "\n"
		}
		request["input"], _ = json.Marshal(text)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func TestParallelAgentAttributionSSE(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{}, 3)
			release := make(chan struct{})
			var active, peak atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Input string }
				if err := decodeModelRequest(r.Body, &req); err != nil {
					t.Error(err)
					return
				}
				if strings.Contains(req.Input, `"caller":"supervisor"`) {
					n := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); n > old; old = peak.Load() {
						if peak.CompareAndSwap(old, n) {
							break
						}
					}
					started <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					multiResponse(w, `{"result":{"status":"completed","answer":"checked"},"next":[]}`)
				} else if strings.Contains(req.Input, `"reports":[{`) {
					if strings.Count(req.Input, `"worker":"worker"`) != 3 {
						t.Error("missing delegated reports")
					}
					multiResponse(w, `{"result":{"status":"completed","answer":"all checked"},"next":[]}`)
				} else {
					multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"A","completion_criteria":["A checked"]},{"agent":"worker","task":"B","completion_criteria":["B checked"]},{"agent":"worker","task":"C","completion_criteria":["C checked"]}]}`)
				}
			}))
			defer server.Close()
			// Cancel before closing the HTTP server on a failing assertion.
			defer cancel()
			client, addr := testutil.StartRedis(t)
			cfg := multiConfig(t, server.URL)
			cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
			cfg.MultiAgent.MaxParallel = limit
			done := make(chan error, 1)
			o := agent.NewMultiAgentProcessorOrchestrator(ctx, filemcp.Config{Workspace: t.TempDir()})
			go func() {
				result, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"task verified"}})
				if err == nil && (len(result) != 1 || result[0].Result.Answer != "all checked") {
					err = fmt.Errorf("unexpected result: %+v", result)
				}
				done <- err
			}()
			for i := 0; i < limit; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("configured workers did not start concurrently")
				}
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("delegation deadlock")
			}
			verifyAgentSSE(t, rdb.NewRedisManagerWithClient(client), 3, false)
			if peak.Load() != int32(limit) {
				t.Fatalf("peak concurrency=%d, want %d", peak.Load(), limit)
			}
		})
	}
}

func TestNestedAgentAttributionSSE(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Input string }
				if err := decodeModelRequest(r.Body, &req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				switch calls.Add(1) {
				case 1:
					multiResponse(w, `{"result":{"status":"completed","answer":"investigate"},"next":[{"agent":"worker","task":"investigate source","completion_criteria":["report evidence"]}]}`)
				case 2:
					multiResponse(w, fmt.Sprintf(`{"result":{"status":%q,"answer":"source report","Observation":["observed source"],"CompletionEvidence":["source:42"]},"next":[]}`, status))
				case 3:
					multiResponse(w, `{"result":{"status":"completed","answer":"request review"},"next":[{"agent":"worker","task":"review report","completion_criteria":["verify report"]}]}`)
				case 4, 5, 6:
					marker := "# Multi-agent execution context\n"
					_, encoded, ok := strings.Cut(req.Input, marker)
					var execution struct {
						Upstream []struct {
							TaskID string `json:"task_id"`
							Worker string `json:"worker"`
							Goal   string `json:"goal"`
							Result agent.ReactResult
						} `json:"upstream_reports"`
						Caller string `json:"caller"`
					}
					if !ok {
						t.Error("missing execution context")
					} else if err := json.Unmarshal([]byte(encoded), &execution); err != nil {
						t.Error(err)
					} else {
						if len(execution.Upstream) != 1 {
							t.Errorf("expected one flat upstream report, got %d", len(execution.Upstream))
						} else {
							report := execution.Upstream[0]
							if report.TaskID != "run/delegation-2" || report.Worker != "worker" || report.Goal != "investigate source" || string(report.Result.Status) != status || report.Result.Answer != "source report" || len(report.Result.Observation) != 1 || report.Result.Observation[0] != "observed source" || len(report.Result.CompletionEvidence) != 1 || report.Result.CompletionEvidence[0] != "source:42" {
								t.Errorf("incomplete upstream report: %+v", report)
							}
						}
						if calls.Load() == 5 && execution.Caller != "worker" {
							t.Errorf("nested caller=%q", execution.Caller)
						}
					}
					if calls.Load() == 4 {
						multiResponse(w, `{"result":{"status":"completed","answer":"delegate verification"},"next":[{"agent":"reviewer","task":"verify evidence","completion_criteria":["verify source"]}]}`)
					} else {
						multiResponse(w, `{"result":{"status":"completed","answer":"verified"},"next":[]}`)
					}
				case 7:
					multiResponse(w, `{"result":{"status":"completed","answer":"final"},"next":[]}`)
				default:
					t.Error("unexpected request")
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "multi.yaml")
			data := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 10\n  max_parallel: 2\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    next_agents: [worker]\n  worker:\n    next_agents: [reviewer]\n  reviewer:\n    next_agents: []\n"
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			client, addr := testutil.StartRedis(t)
			cfg, err := config.LoadAgentConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
			o := agent.NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
			result, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "review evidence", CompletionCriteria: []string{"task verified"}})
			if err != nil || len(result) != 1 || result[0].Result.Answer != "final" || calls.Load() != 7 {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
			}
			verifyAgentSSE(t, rdb.NewRedisManagerWithClient(client), 2, true)
		})
	}
}

func verifyAgentSSE(t *testing.T, backend *rdb.RedisManager, workers int, nested bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := backend.WriteTaskStream(ctx, "run", &entity.LLMResponse{RunID: "run", Type: "task_completed", Delta: "{}"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(backend, nil))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/tasks/run/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	names := map[string]string{}
	parents := map[string]string{}
	counts := map[string]int{}
	summary := false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["run_id"] != "run" {
			t.Fatalf("wrong run: %v", event)
		}
		id := event["agent_instance_id"]
		if event["type"] == "task_completed" {
			if id != "" || event["parent_agent_instance_id"] != "" || event["agent_name"] != "" {
				t.Fatalf("summary attributed: %v", event)
			}
			summary = true
			break
		}
		if id == "" || event["agent_name"] == "" {
			t.Fatalf("missing identity: %v", event)
		}
		names[id] = event["agent_name"]
		parents[id] = event["parent_agent_instance_id"]
		counts[id]++
	}
	if !summary {
		t.Fatalf("missing summary: %v", scanner.Err())
	}
	root := ""
	gotWorkers := 0
	reviewers := 0
	for id, name := range names {
		switch name {
		case "supervisor":
			if root != "" {
				t.Fatal("main agent identity changed between turns")
			}
			root = id
		case "worker":
			gotWorkers++
		case "reviewer":
			reviewers++
		}
	}
	if root == "" || gotWorkers != workers || parents[root] != "" || counts[root] < 2 {
		t.Fatalf("identities=%v parents=%v counts=%v", names, parents, counts)
	}
	for id, name := range names {
		if name == "worker" && parents[id] != root {
			t.Fatalf("worker parent: %v", parents)
		}
		if name == "reviewer" && (names[parents[id]] != "worker" || parents[id] != "run/delegation-3") {
			t.Fatalf("nested parent: %v", parents)
		}
	}
	if nested && reviewers != 1 {
		t.Fatalf("reviewers=%d", reviewers)
	}
}
