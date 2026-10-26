package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
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

func TestMultiAgentSupervisorReceivesDelegatedResult(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		wantAgents := `"agents":[{"name":"supervisor","instruction":"supervise-only"},{"name":"worker","instruction":"work-only"}]`
		if !strings.Contains(req.Input, wantAgents) {
			t.Errorf("missing complete agent instructions: %s", req.Input)
		}
		switch calls.Add(1) {
		case 1:
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect source","completion_criteria":["source checked"],"context":"relevant detail"}]}`)
		case 2:
			for _, want := range []string{"inspect source", "source checked", "relevant detail", "work-only", "task_id"} {
				if !strings.Contains(req.Input, want) {
					t.Errorf("worker missing %q: %s", want, req.Input)
				}
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"inspection complete"},"next":[{"agent":"supervisor","task":"report"}]}`)
		case 3:
			if !strings.Contains(req.Input, "inspection complete") {
				t.Error("supervisor did not receive worker result")
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"final report"},"next":[]}`)
		default:
			t.Error("unexpected model request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	cfg := multiConfig(t, server.URL)
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	results, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect project"})
	if err != nil || len(results) != 1 || results[0].Result.Answer != "final report" {
		t.Fatalf("results=%+v error=%v", results, err)
	}
}

// A failed worker returns control even if its response suggests another handoff.
func TestMultiAgentWorkerFailureReturnsToSupervisor(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		switch calls.Add(1) {
		case 1:
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect source","completion_criteria":["source checked"],"context":"source unavailable"}]}`)
		case 2:
			multiResponse(w, `{"result":{"status":"failed","answer":"source is missing"},"next":[{"agent":"worker","task":"retry","completion_criteria":["retry complete"]}]}`)
		case 3:
			for _, want := range []string{`"failure":"source is missing"`, `"worker":"worker"`, `"goal":"inspect source"`, `"task_id":"run/delegation-2"`} {
				if !strings.Contains(req.Input, want) {
					t.Errorf("supervisor missing %s: %s", want, req.Input)
				}
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"request source from user"},"next":[]}`)
		default:
			t.Error("unexpected model request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	result, err := o.Run(multiConfig(t, server.URL), &state.DurableState{RunId: "run", Goal: "inspect project"})
	if err != nil || len(result) != 1 || result[0].Result.Answer != "request source from user" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestMultiAgentDelegationsReceiveUpstreamReports(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		switch calls.Add(1) {
		case 1:
			multiResponse(w, `{"result":{"status":"completed","answer":"first task"},"next":[{"agent":"worker","task":"inspect A","completion_criteria":["A checked"],"context":"only A detail"}]}`)
		case 2:
			if strings.Contains(req.Input, "private prior outcome") {
				t.Error("root history leaked into worker")
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"A private finding"},"next":[]}`)
		case 3:
			multiResponse(w, `{"result":{"status":"completed","answer":"second task"},"next":[{"agent":"worker","task":"inspect B","completion_criteria":["B checked"],"context":"only B detail"}]}`)
		case 4:
			for _, forbidden := range []string{"only A detail", "private prior outcome"} {
				if strings.Contains(req.Input, forbidden) {
					t.Errorf("second delegation leaked %q", forbidden)
				}
			}
			for _, want := range []string{"run/delegation-3", "inspect B", "B checked", "only B detail", `"upstream_reports":[{`, `"task_id":"run/delegation-2"`, `"worker":"worker"`, `"goal":"inspect A"`, `"answer":"A private finding"`} {
				if !strings.Contains(req.Input, want) {
					t.Errorf("second delegation missing %q", want)
				}
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"B finding"},"next":[]}`)
		case 5:
			for _, want := range []string{"A private finding", "B finding"} {
				if !strings.Contains(req.Input, want) {
					t.Errorf("supervisor missing %q", want)
				}
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"combined report"},"next":[]}`)
		default:
			t.Error("unexpected request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	durable := &state.DurableState{RunId: "run", Goal: "inspect project", PreviousRunOutcomes: []state.RunOutcome{{RunID: "prior", Result: "private prior outcome"}}}
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	result, err := o.Run(multiConfig(t, server.URL), durable)
	if err != nil || len(result) != 1 || result[0].Result.Answer != "combined report" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if durable.Goal != "inspect project" || durable.RunId != "run" {
		t.Fatalf("root identity overwritten: %+v", durable)
	}
}

func TestMultiAgentInfrastructureErrorStopsRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(req.Input, `"caller":"supervisor"`) {
			http.Error(w, "provider unavailable", http.StatusBadRequest)
			return
		}
		if strings.Contains(req.Input, `"reports":[{`) {
			t.Error("infrastructure error was delivered as business result")
		}
		multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect","completion_criteria":["checked"]}]}`)
	}))
	defer server.Close()
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	results, err := o.Run(multiConfig(t, server.URL), &state.DurableState{RunId: "run", Goal: "inspect"})
	if err == nil || !strings.Contains(err.Error(), "400 Bad Request") || len(results) != 0 {
		t.Fatalf("results=%+v error=%v", results, err)
	}
}

func TestMultiAgentCancellationStopsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(req.Input, `"caller":"supervisor"`) {
			close(started)
			<-r.Context().Done()
			return
		}
		multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect","completion_criteria":["checked"]}]}`)
	}))
	defer server.Close()
	cfg := multiConfig(t, server.URL)
	o := NewMultiAgentProcessorOrchestrator(ctx, filemcp.Config{Workspace: t.TempDir()})
	done := make(chan error, 1)
	go func() { _, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect"}); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop execution")
	}
}

func TestReactMultiAgentBusinessFailureIsAResult(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				output := `{"result":{"status":"failed","answer":"source missing"},"next":[]}`
				if schema == config.LineProtocolLLMOutputSchema {
					output = "BEGIN RESULT\nBEGIN RESULT\nSTATUS=\"failed\"\nANSWER=\"source missing\"\nEND RESULT\nEND RESULT"
				}
				multiResponse(w, output)
			}))
			defer server.Close()
			cfg := multiConfig(t, server.URL).ForAgent("worker")
			cfg.LLMConfig.OutputSchema = schema
			process, err := NewReactProcess[MultiAgentResult](context.Background(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := process.Run(cfg, &state.DurableState{RunId: "task", Goal: "inspect source"})
			if err != nil || result.Result.Status != FailedReactStatus || result.Result.Answer != "source missing" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestMultiAgentParallelDelegationsRespectLimit(t *testing.T) {
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
			cfg := multiConfig(t, server.URL)
			cfg.MultiAgent.MaxParallel = limit
			done := make(chan error, 1)
			o := NewMultiAgentProcessorOrchestrator(ctx, filemcp.Config{Workspace: t.TempDir()})
			go func() {
				result, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect"})
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
			if peak.Load() != int32(limit) {
				t.Fatalf("peak concurrency=%d, want %d", peak.Load(), limit)
			}
		})
	}
}

func TestMultiAgentRejectsInvalidDelegationBeforeStartingBatch(t *testing.T) {
	for _, invalid := range []string{
		`{"agent":"unknown","task":"inspect","completion_criteria":["checked"]}`,
		`{"agent":"worker","task":"","completion_criteria":["checked"]}`,
		`{"agent":"worker","task":"inspect","completion_criteria":[" "]}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			var workers atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Input string }
				if err := decodeModelRequest(r.Body, &req); err != nil {
					t.Error(err)
					return
				}
				if strings.Contains(req.Input, `"caller":"supervisor"`) {
					workers.Add(1)
				}
				multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"valid task","completion_criteria":["checked"]},`+invalid+`]}`)
			}))
			defer server.Close()
			o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
			_, err := o.Run(multiConfig(t, server.URL), &state.DurableState{RunId: "run", Goal: "inspect"})
			if err == nil || workers.Load() != 0 {
				t.Fatalf("workers=%d error=%v", workers.Load(), err)
			}
		})
	}
}

func TestMultiAgentWorkerExecutesToolsThroughMCPCommand(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex-mcp")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp").CombinedOutput(); err != nil {
		t.Fatalf("build MCP: %v\n%s", err, output)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("verified file evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		switch calls.Add(1) {
		case 1:
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"read evidence.txt","completion_criteria":["content verified"],"context":"report exact content"}]}`)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"tool","status":"completed","output":[{"type":"function_call","call_id":"read-evidence","name":"read_file","arguments":"{\"path\":\"evidence.txt\"}"}]}`)
		case 3:
			for _, want := range []string{"verified file evidence", "read-evidence", "report exact content"} {
				if !strings.Contains(string(raw), want) {
					t.Errorf("worker tool turn missing %q", want)
				}
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"verified file evidence"},"next":[]}`)
		case 4:
			multiResponse(w, `{"result":{"status":"completed","answer":"inspection verified"},"next":[]}`)
		default:
			t.Error("unexpected request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 3\n  max_parallel: 1\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n  allow_tools: [read_file]\nagents:\n  supervisor:\n    next_agents: [worker]\n  worker:\n    next_agents: [supervisor]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	o := NewMultiAgentProcessorOrchestratorWithCommand(ctx, binary, "--workspace", workspace)
	results, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect evidence"})
	if err != nil || len(results) != 1 || results[0].Result.Answer != "inspection verified" {
		t.Fatalf("results=%+v error=%v", results, err)
	}
}

func TestMultiAgentRoundBudgetStopsFurtherDelegation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect","completion_criteria":["checked"]}]}`)
	}))
	defer server.Close()
	cfg := multiConfig(t, server.URL)
	cfg.MultiAgent.MaxRounds = 1
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	_, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect"})
	if err == nil || !strings.Contains(err.Error(), "max_rounds") || calls.Load() != 1 {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

func TestMultiAgentUpstreamReportsSurviveNestedDelegation(t *testing.T) {
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
							Result ReactResult
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
			cfg, err := config.LoadAgentConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
			result, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "review evidence"})
			if err != nil || len(result) != 1 || result[0].Result.Answer != "final" || calls.Load() != 7 {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
			}
		})
	}
}
