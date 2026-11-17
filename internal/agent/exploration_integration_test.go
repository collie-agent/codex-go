package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

// The script crosses the model API boundary; tools and scheduling remain real.
func implementationScript() [][]any {
	return [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}),
		{observationMessage(`{"exploration":{"task_type":"modification","basis":"fix requested","choice":"implement","target_files":["main.go"],"target":"use package main","evidence":"source declares old","next_operation":"replace package"}}`)},
		modificationTool("edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/package old/package main/", "main.go"}}),
		modificationTool("test", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"changed and verified","Observation":["package corrected"],"CompletionEvidence":["go test main.go passed"]}`)},
	}
}

func integrationWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPlanExplorationModifiesVerifiesAndIsolatesSteps(t *testing.T) {
	workspace := integrationWorkspace(t)
	script := implementationScript()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(string(req.Input), "你是 ReAct agent 的工具结果观察器") {
			multiResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		calls++
		// 第一轮正常探索；第二轮达到阈值；接受实施后不再重复注入。
		if calls <= len(script) {
			// 原始请求包含角色，预算必须出现在 user 消息中。
			var messages []struct {
				Role    string
				Content string
			}
			if err := json.Unmarshal(req.Input, &messages); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, message := range messages {
				if strings.Contains(message.Content, "Exploration budget:") {
					found = true
					if message.Role != "user" {
						t.Error("budget must use user role")
					}
				}
			}
			if calls <= 3 && found != (calls == 2) {
				t.Errorf("call %d: budget present=%t", calls, found)
			}

			writeObservationResponse(w, script[calls-1])
			return
		}
		if calls == len(script)+1 {
			multiResponse(w, `{"exploration":{"task_type":"analysis","basis":"second step only explains verified changes"}}`)
			return
		}
		multiResponse(w, `{"status":"completed","answer":"explained","Observation":["package main"],"CompletionEvidence":["previous step tested"]}`)
	}))
	defer server.Close()
	cfg := &config.AgentConfig{ExplorationBudget: config.DefaultExplorationBudget(), AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	cfg.ExplorationBudget.InitialTurns = 1
	durable := &state.DurableState{RunId: "plan-budget", Goal: "fix then explain", StepExecutions: []state.StepExecution{
		{StepId: "fix", Revision: 1, Status: PendingExecutionStatus, Intent: "fix main.go", CompletionCriteria: "source fixed and tested"},
		{StepId: "explain", Revision: 1, Status: PendingExecutionStatus, Intent: "explain change", CompletionCriteria: "explanation"},
	}}
	o := NewProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: workspace})
	if err := o.SetMode("plan-execute"); err != nil {
		t.Fatal(err)
	}
	results, err := o.Run(cfg, durable)
	if err != nil || len(results) != 1 || results[0].Answer != "explained" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if durable.StepExecutions[0].ExecutionID == durable.StepExecutions[1].ExecutionID {
		t.Fatal("steps shared execution instance")
	}
	for _, step := range durable.StepExecutions {
		if step.Status != CompletedExecutionStatus {
			t.Fatalf("step=%+v", step)
		}
	}
	var saved struct {
		Exploration struct {
			Used     int
			TaskType string
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 0 || saved.Exploration.TaskType != "analysis" {
		t.Fatalf("second step inherited budget: %s", durable.ExecutionProgress)
	}
	content, _ := os.ReadFile(filepath.Join(workspace, "main.go"))
	if string(content) != "package main\n" {
		t.Fatalf("source=%q", content)
	}
}

func TestMultiAgentExplorationModifiesVerifiesAndReports(t *testing.T) {
	workspace := integrationWorkspace(t)
	script := implementationScript()
	workerCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			http.Error(w, "request", 400)
			return
		}
		if strings.Contains(req.Input, "你是 ReAct agent 的工具结果观察器") {
			multiResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		_, task, _ := strings.Cut(req.Input, `"delegated_task":`)
		task, _, _ = strings.Cut(task, `,"reports":`)
		if !strings.Contains(task, `"worker":"worker"`) {
			if strings.Contains(req.Input, `"reports":[{`) {
				if !strings.Contains(req.Input, "changed and verified") {
					t.Error("missing worker report")
				}
				multiResponse(w, `{"result":{"status":"completed","answer":"delivered"},"next":[]}`)
			} else {
				multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"fix main.go","completion_criteria":["changed and tested"]}]}`)
			}
			return
		}
		if workerCalls >= len(script) {
			http.Error(w, "script exhausted", 400)
			return
		}
		output := script[workerCalls]
		workerCalls++
		if workerCalls == 2 || workerCalls == 5 {
			// Only the business result is nested; tools keep their existing protocol.
			message := `{"result":` + []string{"", `{"exploration":{"task_type":"modification","basis":"fix requested","choice":"implement","target_files":["main.go"],"target":"use package main","evidence":"source declares old","next_operation":"replace package"}}`, "", "", `{"status":"completed","answer":"changed and verified"}`}[workerCalls-1] + `,"next":[]}`
			multiResponse(w, message)
			return
		}
		writeObservationResponse(w, output)
	}))
	defer server.Close()
	// Resolve policy through the actual configured node, not the react role.
	path := filepath.Join(t.TempDir(), "multi.yaml")
	yaml := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 10\n  max_parallel: 1\nbase:\n  allow_tools: [read_file, exec]\n  exploration_budget:\n    initial_turns: 9\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    next_agents: [worker]\n  worker:\n    exploration_budget:\n      initial_turns: 1\n      max_extensions: 0\n      decision_retries: 0\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved *state.DurableState
	o := NewMultiAgentProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: workspace})
	o.SetCheckpoint(func(s *state.DurableState, _ string) error { saved = s.Clone(); return nil })
	results, err := o.Run(cfg, &state.DurableState{RunId: "multi-budget", Goal: "fix source", CompletionCriteria: []string{"tested"}})
	if err != nil || len(results) != 1 || results[0].Result.Answer != "delivered" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	var graph state.DelegationGraph[state.MultiAgentResult, state.DelegatedTask]
	if err := json.Unmarshal(saved.MultiAgentCheckpoint, &graph); err != nil {
		t.Fatal(err)
	}
	local := graph.Nodes["multi-budget/delegation-2"].Turns[0].Local
	var progress struct {
		Exploration struct {
			Used                  int
			ModificationConfirmed bool
			Policy                config.ExplorationBudget
		}
	}
	if err := json.Unmarshal(local.ExecutionProgress, &progress); err != nil {
		t.Fatal(err)
	}
	if !progress.Exploration.ModificationConfirmed || progress.Exploration.Used != 1 || progress.Exploration.Policy.InitialTurns != 1 || progress.Exploration.Policy.MaxExtensions != 0 || progress.Exploration.Policy.DecisionRetries != 0 {
		t.Fatalf("worker budget missing: %s", local.ExecutionProgress)
	}
	content, _ := os.ReadFile(filepath.Join(workspace, "main.go"))
	if string(content) != "package main\n" {
		t.Fatalf("source=%q", content)
	}
}

func TestMultiAgentExplorationDecisionDropsMixedTools(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct{ Input string }
				if err := decodeModelRequest(r.Body, &request); err != nil {
					t.Error(err)
					http.Error(w, "request", 400)
					return
				}
				if !strings.Contains(request.Input, "result.exploration") {
					t.Error("missing nested exploration output contract")
				}
				calls++
				if calls == 1 {
					decision := `{"result":{"exploration":{"task_type":"analysis","basis":"explanation only"}},"next":[]}`
					if schema == config.LineProtocolLLMOutputSchema {
						decision = "BEGIN RESULT\nBEGIN RESULT\nBEGIN EXPLORATION\nTASK_TYPE=\"analysis\"\nBASIS=\"explanation only\"\nEND EXPLORATION\nEND RESULT\nEND RESULT"
					}
					output := append([]any{observationMessage(decision)}, modificationTool("discard", "list_dir", map[string]any{"path": "."})...)
					writeObservationResponse(w, output)
					return
				}
				if schema == config.LineProtocolLLMOutputSchema {
					multiResponse(w, "BEGIN RESULT\nBEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"explained\"\nEND RESULT\nEND RESULT")
				} else {
					multiResponse(w, `{"result":{"status":"completed","answer":"explained"},"next":[]}`)
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "schema.yaml")
			data := "name: multi-agent\nmulti-agent:\n  entry: worker\n  max_rounds: 10\n  max_parallel: 1\nbase:\n  allow_tools: [list_dir]\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n    output_schema: " + string(schema) + "\nagents:\n  worker: {}\n"
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadAgentConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			var saved *state.DurableState
			o := NewMultiAgentProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: t.TempDir()})
			o.SetCheckpoint(func(s *state.DurableState, _ string) error { saved = s.Clone(); return nil })
			results, err := o.Run(cfg, &state.DurableState{RunId: "mixed", Goal: "explain", CompletionCriteria: []string{"explanation"}})
			if err != nil || len(results) != 1 || results[0].Result.Answer != "explained" {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			var p struct{ Exploration struct{ TaskType string } }
			_ = json.Unmarshal(saved.ExecutionProgress, &p)
			if p.Exploration.TaskType != "analysis" || len(saved.ToolExecutions) != 0 || calls != 2 {
				t.Fatalf("decision lost or mixed tool executed: progress=%s tools=%d calls=%d", saved.ExecutionProgress, len(saved.ToolExecutions), calls)
			}
		})
	}
}

func TestMultiAgentExplorationExhaustionIsBusinessFailureWithoutEntryCriteria(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			// No extensions or corrections are available; no supplied entry criteria.
			multiResponse(w, `{"result":{"exploration":{"task_type":"modification","basis":"requested fix","choice":"extend","gap":"missing source","blocking_reason":"cannot locate target","investigation":"list directory","exit_condition":"find source"}},"next":[]}`)
		} else {
			multiResponse(w, `{"result":{"status":"completed","answer":"untrusted success"},"next":[{"agent":"worker","task":"must not run","completion_criteria":["none"]}]}`)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "entry.yaml")
	data := "name: multi-agent\nmulti-agent:\n  entry: worker\n  max_rounds: 10\n  max_parallel: 1\nbase:\n  exploration_budget:\n    initial_turns: 1\n    max_extensions: 0\n    decision_retries: 0\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  worker: {}\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	o := NewMultiAgentProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: t.TempDir()})
	results, err := o.Run(cfg, &state.DurableState{RunId: "exhausted", Goal: "fix source"})
	if err != nil || len(results) != 1 || results[0].Result.Status != FailedReactStatus || len(results[0].Next) != 0 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if !strings.Contains(results[0].Result.Answer, "extension") || calls != 2 {
		t.Fatalf("failure=%+v calls=%d", results, calls)
	}
}

func TestMultiAgentExplorationFailureAndIndependentDelegations(t *testing.T) {
	perTask := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			http.Error(w, "request", 400)
			return
		}
		if strings.Contains(req.Input, "你是 ReAct agent 的工具结果观察器") {
			multiResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		_, task, _ := strings.Cut(req.Input, `"delegated_task":`)
		task, _, _ = strings.Cut(task, `,"reports":`)
		if strings.Contains(task, `"worker":"supervisor"`) {
			if strings.Contains(req.Input, `"reports":[{`) {
				for _, want := range []string{`"status":"failed"`, `"failure":`, `"status":"completed"`, "extension"} {
					if !strings.Contains(req.Input, want) {
						t.Errorf("missing report %s", want)
					}
				}
				multiResponse(w, `{"result":{"status":"completed","answer":"one blocked, one explained"},"next":[]}`)
			} else {
				multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"blocked fix","completion_criteria":["fix"]},{"agent":"worker","task":"explain source","completion_criteria":["explain"]}]}`)
			}
			return
		}
		key := "explain"
		if strings.Contains(task, "blocked fix") {
			key = "blocked"
		}
		perTask[key]++
		n := perTask[key]
		switch {
		case n == 1:
			writeObservationResponse(w, modificationTool("read", "read_file", map[string]any{"path": "main.go"}))
		case key == "blocked" && n == 2:
			multiResponse(w, `{"result":{"exploration":{"task_type":"modification","basis":"fix requested","choice":"extend","gap":"missing target","blocking_reason":"cannot patch","investigation":"search","exit_condition":"target found"}},"next":[]}`)
		case key == "blocked":
			multiResponse(w, `{"result":{"status":"completed","answer":"untrusted success"},"next":[{"agent":"forbidden"}]}`)
		case n == 2:
			multiResponse(w, `{"result":{"exploration":{"task_type":"analysis","basis":"explanation only"}},"next":[]}`)
		default:
			multiResponse(w, `{"result":{"status":"completed","answer":"source explained"},"next":[]}`)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 10\n  max_parallel: 1\nbase:\n  allow_tools: [read_file]\n  exploration_budget:\n    initial_turns: 1\n    max_extensions: 0\n    decision_retries: 0\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    next_agents: [worker]\n  worker: {}\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved *state.DurableState
	o := NewMultiAgentProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: integrationWorkspace(t)})
	o.SetCheckpoint(func(s *state.DurableState, _ string) error { saved = s.Clone(); return nil })
	results, err := o.Run(cfg, &state.DurableState{RunId: "isolated", Goal: "fix and explain", CompletionCriteria: []string{"report both"}})
	if err != nil || len(results) != 1 || results[0].Result.Answer != "one blocked, one explained" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	var graph state.DelegationGraph[state.MultiAgentResult, state.DelegatedTask]
	if err := json.Unmarshal(saved.MultiAgentCheckpoint, &graph); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, id := range []string{"isolated/delegation-2", "isolated/delegation-3"} {
		local := graph.Nodes[id].Turns[0].Local
		var p struct{ Exploration struct{ Used int } }
		if err := json.Unmarshal(local.ExecutionProgress, &p); err != nil {
			t.Fatal(err)
		}
		if p.Exploration.Used != 1 || len(local.ToolExecutions) != 1 || ids[local.ExecutionID] {
			t.Fatalf("delegation contaminated: %+v", local)
		}
		ids[local.ExecutionID] = true
	}
}
