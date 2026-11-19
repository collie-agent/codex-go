package application_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
)

// Public checkpoints seed an unfinished execution; Resume must keep its policy
// even when the role's current YAML grants a much larger allowance.
func TestScheduledExplorationResumeModifiesVerifiesAndCompletes(t *testing.T) {
	for _, mode := range []string{"plan-execute", "multi-agent"} {
		t.Run(mode, func(t *testing.T) {
			_, store := runStore(t)
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			run, err := store.Create(t.Context(), "", "fix main.go", workspace, mode)
			if err != nil {
				t.Fatal(err)
			}
			id := run.RunID + "/original"
			handoff := map[string]any{"target": "correct package", "target_files": []string{"main.go"}, "evidence": "old package inspected", "next_operation": "replace package"}
			progress := map[string]any{"ExecutionID": id, "UsedTurns": 5, "Exploration": map[string]any{
				"Policy":   map[string]int{"initial_turns": 1, "extension_turns": 2, "max_extensions": 1, "decision_retries": 1, "modification_window_turns": 2},
				"TaskType": "modification", "Phase": "implement", "Used": 3, "Extensions": 1, "Invalid": 1, "ModificationLeft": 1, "Handoff": handoff, "Handoffs": []any{handoff},
			}}
			raw, _ := json.Marshal(progress)
			local := &state.DurableState{RunId: run.RunID, ExecutionID: id, Goal: "fix main.go", CompletionCriteria: []string{"changed and tested"}, ExecutionProgress: raw}
			durable := local.Clone()
			if mode == "plan-execute" {
				durable.StepExecutions = []state.StepExecution{{StepId: "fix", ExecutionID: id, Revision: 1, Status: state.RunningExecutionStatus, Intent: "fix main.go", CompletionCriteria: "changed and tested"}}
			} else {
				graph := state.DelegationGraph[state.MultiAgentResult, state.DelegatedTask]{Nodes: map[string]*state.DelegationNode[state.MultiAgentResult, state.DelegatedTask]{run.RunID + "/delegation-1": {AgentName: "builder", Turns: []*state.DelegationTurn[state.DelegatedTask]{{Local: local}}}}}
				durable.MultiAgentCheckpoint, _ = json.Marshal(graph)
			}
			if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, mode); err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "你是 ReAct agent 的工具结果观察器") {
					writeResult(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
					return
				}
				calls++
				if calls == 1 && !strings.Contains(string(body), "phase=implement used=3/3") {
					t.Error("handoff or original budget lost")
				}
				switch calls {
				case 1, 2:
					command, args := "sed", []string{"-i", "s/package old/package main/", "main.go"}
					if calls == 2 {
						command, args = "go", []string{"test", "main.go"}
					}
					arguments, _ := json.Marshal(map[string]any{"command": command, "args": args})
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "tool", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": fmt.Sprint("fc", calls), "call_id": fmt.Sprint("call", calls), "name": "exec", "arguments": string(arguments)}}})
				default:
					result := `{"status":"completed","answer":"changed and tested","Observation":["package corrected"],"CompletionEvidence":["go test main.go passed"]}`
					if mode == "multi-agent" {
						result = `{"result":` + result + `,"next":[]}`
					}
					writeResult(w, result)
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent.yaml")
			name, role := "react", "react"
			graphConfig := ""
			if mode == "multi-agent" {
				name, role = "multi-agent", "builder"
				graphConfig = "multi-agent:\n  entry: builder\n  max_rounds: 10\n  max_parallel: 1\n"
			}
			yaml := "name: " + name + "\n" + graphConfig + "base:\n  allow_tools: [exec]\n  exploration_budget:\n    initial_turns: 99\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  " + role + ": {}\n"
			if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
				t.Fatal(err)
			}
			results, err := application.Resume(t.Context(), application.Options{RunID: run.RunID, ConfigPath: path, Timeout: 20 * time.Second, Stderr: io.Discard})
			if err != nil || len(results) != 1 || results[0].Answer != "changed and tested" || calls != 3 {
				t.Fatalf("results=%+v err=%v calls=%d", results, err, calls)
			}
			saved, err := store.ExecutionByID(t.Context(), run.RunID, id)
			if err != nil {
				t.Fatal(err)
			}
			var after struct {
				Exploration struct {
					Used, Extensions, Invalid, ModificationLeft int
					ModificationConfirmed                       bool
					Policy                                      map[string]int
				}
			}
			if err := json.Unmarshal(saved.ExecutionProgress, &after); err != nil {
				t.Fatal(err)
			}
			e := after.Exploration
			if e.Used != 3 || e.Extensions != 1 || e.Invalid != 1 || e.ModificationLeft != 0 || !e.ModificationConfirmed || e.Policy["initial_turns"] != 1 {
				t.Fatalf("state refreshed: %s", saved.ExecutionProgress)
			}
			content, _ := os.ReadFile(filepath.Join(workspace, "main.go"))
			if string(content) != "package main\n" {
				t.Fatalf("source=%q", content)
			}
		})
	}
}

func TestScheduledExplorationResumeStopsExhaustedWindows(t *testing.T) {
	for _, mode := range []string{"plan-execute", "multi-agent"} {
		for _, stop := range []string{"modification window exhausted", "exploration decision corrections exhausted"} {
			t.Run(mode+"/"+stop, func(t *testing.T) {
				_, store := runStore(t)
				run, err := store.Create(t.Context(), "", "fix source", t.TempDir(), mode)
				if err != nil {
					t.Fatal(err)
				}
				id := run.RunID + "/original"
				exploration := map[string]any{"Policy": map[string]int{"initial_turns": 1, "extension_turns": 1, "max_extensions": 0, "decision_retries": 0, "modification_window_turns": 1}, "TaskType": "modification", "Phase": "implement", "Used": 1, "ModificationLeft": 0}
				if strings.Contains(stop, "corrections") {
					exploration["Phase"] = "exploring"
					exploration["Pending"] = true
					exploration["Invalid"] = 1
					exploration["Rejection"] = "extension count exhausted"
				}
				raw, _ := json.Marshal(map[string]any{"ExecutionID": id, "UsedTurns": 7, "Exploration": exploration})
				local := &state.DurableState{RunId: run.RunID, ExecutionID: id, Goal: "fix source", CompletionCriteria: []string{"fixed"}, ExecutionProgress: raw}
				durable := local.Clone()
				if mode == "plan-execute" {
					durable.StepExecutions = []state.StepExecution{{StepId: "fix", Revision: 1, Status: state.RunningExecutionStatus, ExecutionID: id, Intent: "fix source", CompletionCriteria: "fixed"}}
				} else {
					graph := state.DelegationGraph[state.MultiAgentResult, state.DelegatedTask]{Nodes: map[string]*state.DelegationNode[state.MultiAgentResult, state.DelegatedTask]{run.RunID + "/delegation-1": {AgentName: "builder", Turns: []*state.DelegationTurn[state.DelegatedTask]{{Local: local}}}}}
					durable.MultiAgentCheckpoint, _ = json.Marshal(graph)
				}
				if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, mode); err != nil {
					t.Fatal(err)
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					// Closing cannot turn a model's success declaration into task completion.
					result := `{"status":"completed","answer":"untrusted success"}`
					if mode == "multi-agent" {
						result = `{"result":` + result + `,"next":[]}`
					}
					writeResult(w, result)
				}))
				defer server.Close()
				name, role, graphConfig := "react", "react", ""
				if mode == "multi-agent" {
					name, role = "multi-agent", "builder"
					graphConfig = "multi-agent:\n  entry: builder\n  max_rounds: 10\n  max_parallel: 1\n"
				}
				path := filepath.Join(t.TempDir(), "agent.yaml")
				yaml := "name: " + name + "\n" + graphConfig + "base:\n  exploration_budget:\n    initial_turns: 99\n    modification_window_turns: 99\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  " + role + ": {}\n"
				if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
					t.Fatal(err)
				}
				_, err = application.Resume(t.Context(), application.Options{RunID: run.RunID, ConfigPath: path, Timeout: 10 * time.Second, Stderr: io.Discard})
				if err == nil || !strings.Contains(err.Error(), stop) || calls != 1 {
					t.Fatalf("err=%v calls=%d", err, calls)
				}
				saved, err := store.ExecutionByID(t.Context(), run.RunID, id)
				if err != nil {
					t.Fatal(err)
				}
				if len(saved.ToolExecutions) != 0 || !strings.Contains(string(saved.ExecutionResult), stop) {
					t.Fatalf("failure missing or tool released: %+v", saved)
				}
				// A terminal business failure is replayed without another summary/model turn.
				_, _ = application.Resume(t.Context(), application.Options{RunID: run.RunID, ConfigPath: path, Timeout: 10 * time.Second, Stderr: io.Discard})
				if calls != 1 {
					t.Fatalf("terminal failure repeated model work: %d", calls)
				}
			})
		}
	}
}

func TestDelegatedExplorationResumeKeepsOriginalInstanceState(t *testing.T) {
	for _, tc := range []struct {
		name, phase                          string
		extensions, invalid, left, usedAfter int
		pending, confirmed                   bool
	}{
		{name: "approved-extension", phase: "exploring", extensions: 1, usedAfter: 3},
		{name: "decision-correction", phase: "exploring", invalid: 1, pending: true, usedAfter: 2},
		{name: "modification-window", phase: "implement", left: 1, usedAfter: 2},
		{name: "verification", phase: "exploring", confirmed: true, usedAfter: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := runStore(t)
			run, err := store.Create(t.Context(), "", "fix source", t.TempDir(), "multi-agent")
			if err != nil {
				t.Fatal(err)
			}
			rootID, childID := run.RunID+"/delegation-1", run.RunID+"/delegation-2"
			instance := childID + "/turn-0"
			task := state.DelegatedTask{TaskID: childID, Worker: "builder", Goal: "fix source", CompletionCriteria: []string{"fixed"}}
			result := state.MultiAgentResult{Result: state.ReactResult{Status: state.CompletedReactStatus, Answer: "delegate"}, Next: []state.AgentHandoff{{Agent: "builder", Task: "fix source", CompletionCriteria: []string{"fixed"}}}}
			resultRaw, _ := json.Marshal(result)
			parent := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, Goal: "fix source", CompletionCriteria: []string{"fixed"}, ExecutionResult: resultRaw}
			handoff := state.ExplorationDecision{TargetFiles: []string{"main.go"}, Target: "fix package", Evidence: "old declaration", NextOperation: "apply patch"}
			exploration := map[string]any{"Policy": map[string]int{"initial_turns": 2, "extension_turns": 2, "max_extensions": 1, "decision_retries": 1, "modification_window_turns": 2}, "TaskType": "modification", "Phase": tc.phase, "Used": 2, "Extensions": tc.extensions, "Invalid": tc.invalid, "Pending": tc.pending, "ModificationLeft": tc.left, "ModificationConfirmed": tc.confirmed, "Handoff": handoff, "Handoffs": []any{handoff}}
			if tc.pending {
				exploration["Rejection"] = "required fields missing"
			}
			raw, _ := json.Marshal(map[string]any{"ExecutionID": instance, "UsedTurns": 8, "Exploration": exploration})
			local := &state.DurableState{RunId: run.RunID, ExecutionID: instance, Goal: "fix source", CompletionCriteria: []string{"fixed"}, ExecutionProgress: raw}
			graph := state.DelegationGraph[state.MultiAgentResult, state.DelegatedTask]{NextID: 2, Nodes: map[string]*state.DelegationNode[state.MultiAgentResult, state.DelegatedTask]{
				rootID:  {AgentName: "coordinator", Turns: []*state.DelegationTurn[state.DelegatedTask]{{Local: parent, Children: []state.DelegatedTask{task}}}},
				childID: {AgentName: "builder", Turns: []*state.DelegationTurn[state.DelegatedTask]{{Local: local}}},
			}}
			durable := parent.Clone()
			durable.MultiAgentCheckpoint, _ = json.Marshal(graph)
			if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, "multi-agent"); err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				body, _ := io.ReadAll(r.Body)
				wants := []string{instance, fmt.Sprintf("phase=%s used=2/%d", tc.phase, 2+2*tc.extensions)}
				if !tc.pending && tc.phase == "exploring" && !tc.confirmed && tc.extensions > 0 {
					wants = nil
				}
				for _, want := range wants {
					if !strings.Contains(string(body), want) {
						t.Errorf("restored request missing %s", want)
					}
				}
				http.Error(w, "model unavailable", http.StatusBadRequest)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "multi.yaml")
			yaml := "name: multi-agent\nmulti-agent:\n  entry: coordinator\n  max_rounds: 10\n  max_parallel: 1\nbase:\n  exploration_budget:\n    initial_turns: 99\n    max_extensions: 99\n    decision_retries: 99\n    modification_window_turns: 99\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  coordinator:\n    next_agents: [builder]\n  builder: {}\n"
			if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
				t.Fatal(err)
			}
			_, err = application.Resume(t.Context(), application.Options{RunID: run.RunID, ConfigPath: path, Timeout: 10 * time.Second, Stderr: io.Discard})
			if err == nil || requests != 1 {
				t.Fatalf("err=%v requests=%d", err, requests)
			}
			saved, err := store.ExecutionByID(t.Context(), run.RunID, instance)
			if err != nil {
				t.Fatal(err)
			}
			var after struct {
				UsedTurns   int
				Exploration struct {
					Phase                                       string
					Used, Extensions, Invalid, ModificationLeft int
					Pending, ModificationConfirmed              bool
					Policy                                      map[string]int
					Handoff                                     state.ExplorationDecision
				}
			}
			if err := json.Unmarshal(saved.ExecutionProgress, &after); err != nil {
				t.Fatal(err)
			}
			e := after.Exploration
			if after.UsedTurns != 9 || e.Phase != tc.phase || e.Used != tc.usedAfter || e.Extensions != tc.extensions || e.Invalid != tc.invalid || e.ModificationLeft != tc.left || e.Pending != tc.pending || e.ModificationConfirmed != tc.confirmed || e.Policy["initial_turns"] != 2 || e.Policy["max_extensions"] != 1 || e.Handoff.Target != "fix package" {
				t.Fatalf("state refreshed or contaminated: %s", saved.ExecutionProgress)
			}
		})
	}
}
