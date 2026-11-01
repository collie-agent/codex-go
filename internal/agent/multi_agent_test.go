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
	results, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect project", CompletionCriteria: []string{"task verified"}})
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
	result, err := o.Run(multiConfig(t, server.URL), &state.DurableState{RunId: "run", Goal: "inspect project", CompletionCriteria: []string{"task verified"}})
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
	durable := &state.DurableState{RunId: "run", Goal: "inspect project", CompletionCriteria: []string{"project inspected"}, PreviousRunOutcomes: []state.RunOutcome{{RunID: "prior", Result: "private prior outcome"}}}
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
	results, err := o.Run(multiConfig(t, server.URL), &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"task verified"}})
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
	go func() {
		_, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"task verified"}})
		done <- err
	}()
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

func TestMultiAgentReactBusinessFailureIsAResult(t *testing.T) {
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
			process, err := NewMultiAgentReactProcess(context.Background(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := process.Run(cfg, &state.DurableState{RunId: "task", Goal: "inspect source", CompletionCriteria: []string{"task verified"}})
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
			_, err := o.Run(multiConfig(t, server.URL), &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"task verified"}})
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
	results, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect evidence", CompletionCriteria: []string{"task verified"}})
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
	_, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"task verified"}})
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
			result, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "review evidence", CompletionCriteria: []string{"task verified"}})
			if err != nil || len(result) != 1 || result[0].Result.Answer != "final" || calls.Load() != 7 {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
			}
		})
	}
}

func TestMultiAgentSupervisorPreservesInitialCompletionCriteria(t *testing.T) {
	for _, initial := range [][]string{{"explain components"}, nil} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			var calls atomic.Int32
			generated := "identify agents with source evidence"
			want := generated
			if len(initial) > 0 {
				want = initial[0]
			}
			var saved []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Input string }
				if err := decodeModelRequest(r.Body, &req); err != nil {
					t.Error(err)
					return
				}
				n := calls.Add(1)
				if n == 1 && len(initial) == 0 {
					if !strings.Contains(req.Input, "# Multi-agent execution context") || !strings.Contains(req.Input, "GenerateEntryCompletionCriteria") {
						t.Error("first entry call must request criteria alongside its normal decision")
					}
				} else if n == 1 || n == 3 {
					if !strings.Contains(req.Input, "# CompletionCriteria\n\n- "+want) {
						t.Error("entry call lacks saved criteria")
					}
					if strings.Contains(req.Input, "GenerateEntryCompletionCriteria") {
						t.Error("must not request criteria again")
					}
				}
				switch n {
				case 1:
					multiResponse(w, `{"entry_completion_criteria":["identify agents with source evidence"],"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect","completion_criteria":["source checked"]}]}`)
				case 2:
					if len(saved) != 1 || saved[0] != want {
						t.Error("criteria must be checkpointed before delegation")
					}
					multiResponse(w, `{"result":{"status":"completed","answer":"source report"},"next":[]}`)
				case 3:
					multiResponse(w, `{"entry_completion_criteria":["must not overwrite"],"result":{"status":"completed","answer":"final"},"next":[]}`)
				default:
					t.Error("extra model request")
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
			o.SetCheckpoint(func(snapshot *state.DurableState, mode string) error {
				saved = append([]string(nil), snapshot.CompletionCriteria...)
				return nil
			})
			durable := &state.DurableState{RunId: "run", Goal: "explain agents", CompletionCriteria: initial}
			results, err := o.Run(multiConfig(t, server.URL), durable)
			if err != nil || len(results) != 1 || results[0].Result.Answer != "final" || calls.Load() != 3 {
				t.Fatalf("results=%+v calls=%d err=%v", results, calls.Load(), err)
			}
			if len(durable.CompletionCriteria) != 1 || durable.CompletionCriteria[0] != want {
				t.Fatalf("criteria=%v", durable.CompletionCriteria)
			}
		})
	}
}

func TestMultiAgentEntryCriteriaLineProtocol(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		switch calls.Add(1) {
		case 1:
			if !strings.Contains(req.Input, "GenerateEntryCompletionCriteria") {
				t.Error("missing initial criteria request")
			}
			multiResponse(w, "BEGIN RESULT\nENTRY_COMPLETION_CRITERIA=\"cite verified source evidence\"\nBEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"delegate\"\nEND RESULT\nBEGIN NEXT\nAGENT=\"worker\"\nTASK=\"inspect source\"\nCOMPLETION_CRITERIA=\"source checked\"\nCONTEXT=\"read only\"\nEND NEXT\nEND RESULT")
		case 2:
			if strings.Contains(req.Input, "GenerateEntryCompletionCriteria") {
				t.Error("worker must not generate entry criteria")
			}
			multiResponse(w, "BEGIN RESULT\nBEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"source report\"\nEND RESULT\nEND RESULT")
		case 3:
			if !strings.Contains(req.Input, "# CompletionCriteria\n\n- cite verified source evidence") || strings.Contains(req.Input, "GenerateEntryCompletionCriteria") {
				t.Error("entry must reuse saved criteria")
			}
			multiResponse(w, "BEGIN RESULT\nBEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"verified\"\nEND RESULT\nEND RESULT")
		default:
			t.Error("extra model request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	cfg := multiConfig(t, server.URL)
	// 直接配置公开入口模型，验证整个委派链中的 Line Protocol。
	cfg = &config.AgentConfig{LLMConfig: cfg.LLMConfig, LogLevel: cfg.LogLevel, NextAgents: []string{"worker"}}
	cfg.LLMConfig.OutputSchema = config.LineProtocolLLMOutputSchema
	cfg.MultiAgent = &config.MultiAgentConfig{Entry: "supervisor", MaxRounds: 3, MaxParallel: 1}
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	durable := &state.DurableState{RunId: "line", Goal: "inspect source"}
	result, err := o.Run(cfg, durable)
	if err != nil || len(result) != 1 || result[0].Result.Answer != "verified" || calls.Load() != 3 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
	if len(durable.CompletionCriteria) != 1 || durable.CompletionCriteria[0] != "cite verified source evidence" {
		t.Fatalf("criteria=%v", durable.CompletionCriteria)
	}
}

func TestMultiAgentRecoversMisplacedEntryCriteria(t *testing.T) {
	var retried atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !strings.Contains(req.Input, "LLM output format error") {
			multiResponse(w, "BEGIN RESULT\nBEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"verified\"\nENTRY_COMPLETION_CRITERIA=\"cite verified source evidence\"\nEND RESULT\nEND RESULT")
			return
		}
		retried.Store(true)
		for _, want := range []string{"result.entry_completion_criteria", "Move ENTRY_COMPLETION_CRITERIA", "outer RESULT", "outside the nested RESULT", "instead of deleting it"} {
			if !strings.Contains(req.Input, want) {
				t.Errorf("correction missing %q", want)
			}
		}
		multiResponse(w, "BEGIN RESULT\nENTRY_COMPLETION_CRITERIA=\"cite verified source evidence\"\nBEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"verified\"\nEND RESULT\nEND RESULT")
	}))
	defer server.Close()
	cfg := multiConfig(t, server.URL)
	cfg = &config.AgentConfig{LLMConfig: cfg.LLMConfig, LogLevel: cfg.LogLevel, MultiAgent: cfg.MultiAgent, NextAgents: []string{"worker"}}
	cfg.LLMConfig.OutputSchema = config.LineProtocolLLMOutputSchema
	o := NewMultiAgentProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: t.TempDir()})
	durable := &state.DurableState{RunId: "misplaced-criteria", Goal: "inspect source"}
	results, err := o.Run(cfg, durable)
	if err != nil || len(results) != 1 || results[0].Result.Answer != "verified" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if !retried.Load() || len(durable.CompletionCriteria) != 1 || durable.CompletionCriteria[0] != "cite verified source evidence" {
		t.Fatalf("retried=%v criteria=%v", retried.Load(), durable.CompletionCriteria)
	}
}

func TestMultiAgentResumesSavedEntryResult(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer server.Close()
	durable := &state.DurableState{RunId: "root", Goal: "inspect", ExecutionID: "root", ExecutionResult: []byte(`{"entry_completion_criteria":["checked"],"result":{"status":"completed","answer":"saved"},"next":[]}`)}
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	results, err := o.Run(multiConfig(t, server.URL), durable)
	if err != nil || len(results) != 1 || results[0].Result.Answer != "saved" || calls.Load() != 0 {
		t.Fatalf("results=%v error=%v calls=%d", results, err, calls.Load())
	}
}

func TestMultiAgentResumeStopsAtUnknownEntryToolOutcome(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer server.Close()
	durable := &state.DurableState{RunId: "root", Goal: "inspect", ExecutionID: "root", ToolExecutions: []*state.ToolResult{{ToolCallId: "write", ToolName: "apply_patch"}}}
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	_, err := o.Run(multiConfig(t, server.URL), durable)
	if err == nil || !strings.Contains(err.Error(), "unknown tool outcome") || calls.Load() != 0 {
		t.Fatalf("error=%v calls=%d", err, calls.Load())
	}
}

func TestMultiAgentResumeReusesConfirmedEntryToolHistory(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if !strings.Contains(string(body), "previously verified") {
			t.Error("confirmed tool result missing from resumed context")
		}
		multiResponse(w, `{"result":{"status":"completed","answer":"done"},"next":[]}`)
	}))
	defer server.Close()
	durable := &state.DurableState{RunId: "root", Goal: "inspect", CompletionCriteria: []string{"checked"}, ExecutionID: "root", ToolExecutions: []*state.ToolResult{{ToolCallId: "read", ToolName: "read_file", Arguments: `{"path":"file"}`, Status: "success", Result: `[{"type":"text","text":"previously verified"}]`}}}
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	var saved *state.DurableState
	o.SetCheckpoint(func(s *state.DurableState, _ string) error { saved = s.Clone(); return nil })
	results, err := o.Run(multiConfig(t, server.URL), durable)
	if err != nil || len(results) != 1 || results[0].Result.Answer != "done" || calls.Load() != 1 {
		t.Fatalf("results=%v error=%v calls=%d", results, err, calls.Load())
	}
	if saved == nil || saved.RunId != "root" || saved.ExecutionID != "root" || len(saved.ExecutionResult) == 0 {
		t.Fatalf("entry checkpoint=%+v", saved)
	}
}

func TestMultiAgentResumeKeepsCompletedDelegation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect","completion_criteria":["done"]}]}`)
		case 2:
			multiResponse(w, `{"result":{"status":"completed","answer":"worker report"},"next":[]}`)
		case 3:
			multiResponse(w, `{"result":{"status":"completed","answer":"final"},"next":[]}`)
		default:
			t.Error("completed task was repeated")
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg := multiConfig(t, server.URL)
	initial := &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"done"}}
	var saved *state.DurableState
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	o.SetCheckpoint(func(s *state.DurableState, _ string) error {
		if saved != nil && strings.Contains(string(saved.MultiAgentCheckpoint), "worker report") {
			return errors.New("simulated crash")
		}
		saved = s.Clone()
		return nil
	})
	_, err := o.Run(cfg, initial)
	if err == nil || saved == nil {
		t.Fatalf("expected crash, got %v", err)
	}
	o.SetCheckpoint(func(s *state.DurableState, _ string) error { saved = s.Clone(); return nil })
	result, err := o.Run(cfg, saved)
	if err != nil || len(result) != 1 || result[0].Result.Answer != "final" || calls.Load() != 3 {
		t.Fatalf("result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
}

func TestMultiAgentRecoveryAtDelegationBoundaries(t *testing.T) {
	for _, scenario := range []struct {
		name, marker                     string
		calls                            int32
		wantEntry, wantWorker, wantFinal int32
	}{
		{"before delegation registration", `"Children":[{`, 1, 2, 1, 1},
		{"after delegation registration", `/delegation-2/turn-0`, 1, 1, 1, 1},
		{"parent continuation registration", `/delegation-1/turn-1`, 2, 1, 1, 1},
		{"parent result commit", `"answer":"final"`, 3, 1, 1, 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var total, entry, worker, final atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var request struct{ Input string }
				if err := decodeModelRequest(req.Body, &request); err != nil {
					t.Error(err)
					return
				}
				total.Add(1)
				switch {
				case strings.Contains(request.Input, `"worker":"worker"`) && strings.Contains(request.Input, `"caller":"supervisor"`):
					worker.Add(1)
					multiResponse(w, `{"result":{"status":"completed","answer":"worker report"},"next":[]}`)
				case strings.Contains(request.Input, `"reports":[{`):
					final.Add(1)
					multiResponse(w, `{"result":{"status":"completed","answer":"final"},"next":[]}`)
				default:
					entry.Add(1)
					multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect","completion_criteria":["done"]}]}`)
				}
			}))
			defer server.Close()
			cfg := multiConfig(t, server.URL)
			o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
			var saved *state.DurableState
			var injected bool
			o.SetCheckpoint(func(s *state.DurableState, _ string) error {
				if total.Load() == scenario.calls && strings.Contains(string(s.MultiAgentCheckpoint), scenario.marker) && !injected {
					injected = true
					return errors.New("injected checkpoint failure")
				}
				saved = s.Clone()
				return nil
			})
			_, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"done"}})
			if err == nil || !injected || saved == nil {
				t.Fatalf("failure not injected: %v", err)
			}
			o.SetCheckpoint(func(s *state.DurableState, _ string) error { saved = s.Clone(); return nil })
			result, err := o.Run(cfg, saved)
			if err != nil || len(result) != 1 || result[0].Result.Answer != "final" || entry.Load() != scenario.wantEntry || worker.Load() != scenario.wantWorker || final.Load() != scenario.wantFinal {
				t.Fatalf("result=%+v entry=%d worker=%d final=%d err=%v", result, entry.Load(), worker.Load(), final.Load(), err)
			}
		})
	}
}

func TestMultiAgentResumeStopsAtUnknownChildToolOutcome(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"inspect","completion_criteria":["done"]}]}`)
		case 2:
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_read", "call_id": "child-read", "name": "read_file", "arguments": `{"path":"evidence.txt"}`}})
		default:
			t.Error("unexpected model call")
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	o := NewMultiAgentProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: workspace})
	var saved *state.DurableState
	pending := false
	o.SetCheckpoint(func(s *state.DurableState, _ string) error {
		if pending {
			return errors.New("injected failure saving tool result")
		}
		saved = s.Clone()
		if strings.Contains(string(s.MultiAgentCheckpoint), `"ToolCallId":"child-read"`) {
			pending = true
		}
		return nil
	})
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 10\n  max_parallel: 2\nbase:\n  allow_tools: [read_file]\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    next_agents: [worker]\n  worker:\n    next_agents: [supervisor]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"done"}})
	if err == nil || !pending {
		t.Fatalf("pending tool was not captured: %v", err)
	}
	o.SetCheckpoint(func(s *state.DurableState, _ string) error { return nil })
	_, err = o.Run(cfg, saved)
	if err == nil || !strings.Contains(err.Error(), "child-read") || calls.Load() != 2 {
		t.Fatalf("expected unknown child tool outcome, calls=%d err=%v", calls.Load(), err)
	}
}
