package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
)

// 模拟整批结果及快照已落盘，但实施阶段尚未更新的崩溃边界。
func TestResumeExplorationReconcilesSavedModificationEvidence(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "fix main.go", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, CompletionCriteria: []string{"fixed"},
		ToolExecutions: []*state.ToolResult{{ToolCallId: "patch", ToolName: "apply_patch", Arguments: `{"patch":"already executed"}`, Status: "success", Result: "[]", Batch: 1}}}
	progress := map[string]any{
		"ExecutionID": run.RunID, "UsedTurns": 3,
		"Exploration": map[string]any{
			"Policy":   map[string]int{"initial_turns": 1, "extension_turns": 1, "max_extensions": 1, "decision_retries": 1, "modification_window_turns": 1},
			"TaskType": "modification", "Phase": "implement", "Used": 1, "ModificationLeft": 0,
			"Handoff":  map[string]any{"target": "fix main.go", "target_files": []string{"main.go"}},
			"Handoffs": []any{map[string]any{"target": "fix main.go", "target_files": []string{"main.go"}}},
			"Accounting": []any{map[string]any{"Batch": 1, "Handoff": 1, "Purpose": "modification", "Calls": map[string]string{"patch": "modification"}, "Outcome": "known",
				"Before": map[string]any{"main.go": map[string]any{"Known": true, "Version": "absent"}},
				"After":  map[string]any{"main.go": map[string]any{"Known": true, "Version": "sha256:saved-result"}}}},
		}}
	durable.ExecutionProgress, _ = json.Marshal(progress)
	if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), "phase=exploring used=1/1") {
			t.Errorf("restored state missing: %s", raw)
		}
		writeResult(w, `{"status":"completed","answer":"fixed and verified"}`)
	}))
	defer server.Close()
	results, err := application.Resume(ctx, application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err != nil || len(results) != 1 || results[0].Answer != "fixed and verified" || requests != 1 {
		t.Fatalf("results=%v err=%v requests=%d", results, err, requests)
	}
}

func TestResumeExplorationRetainsEveryPhaseAndOriginalPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, phase, taskType                     string
		used, extensions, invalid, left, handoffs int
		pending, confirmed                        bool
		expectedUsed                              int
	}{
		{name: "unclassified", phase: "exploring", used: 2, expectedUsed: 3},
		{name: "threshold-correction", phase: "exploring", taskType: "modification", used: 3, invalid: 1, pending: true, expectedUsed: 3},
		{name: "approved-extension", phase: "exploring", taskType: "modification", used: 3, extensions: 1, expectedUsed: 4},
		{name: "first-modification", phase: "implement", taskType: "modification", used: 3, left: 2, handoffs: 1, expectedUsed: 3},
		{name: "followup-modification", phase: "implement", taskType: "modification", used: 3, left: 1, handoffs: 2, expectedUsed: 3},
		{name: "verification", phase: "exploring", taskType: "modification", used: 3, handoffs: 1, confirmed: true, expectedUsed: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := runStore(t)
			ctx := context.Background()
			run, err := store.Create(ctx, "", "fix main.go", t.TempDir(), "react")
			if err != nil {
				t.Fatal(err)
			}
			handoff := state.ExplorationDecision{Target: "fix main.go", TargetFiles: []string{"main.go"}, Evidence: "source inspected", NextOperation: "apply patch"}
			handoffs := []any{}
			for range tc.handoffs {
				handoffs = append(handoffs, handoff)
			}
			evidence := []any{}
			if tc.handoffs == 2 || tc.confirmed {
				evidence = append(evidence, map[string]any{"Handoff": 1, "Path": "main.go", "Before": "old", "After": "new", "Batch": 1})
			}
			exploration := map[string]any{"Policy": map[string]int{"initial_turns": 3, "extension_turns": 2, "max_extensions": 1, "decision_retries": 1, "modification_window_turns": 3}, "TaskType": tc.taskType, "Phase": tc.phase, "Used": tc.used, "Extensions": tc.extensions, "Invalid": tc.invalid, "Pending": tc.pending, "ModificationLeft": tc.left, "ModificationConfirmed": tc.confirmed, "Handoff": handoff, "Handoffs": handoffs, "ModificationEvidence": evidence}
			progress, _ := json.Marshal(map[string]any{"ExecutionID": run.RunID, "UsedTurns": 7, "Exploration": exploration})
			durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, CompletionCriteria: []string{"fixed"}, ExecutionProgress: progress}
			if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				raw, _ := io.ReadAll(r.Body)
				expected := fmt.Sprintf("task_type=%s phase=%s used=%d/%d decision_required=%t extensions=%d", tc.taskType, tc.phase, tc.used, 3+2*tc.extensions, tc.pending || (tc.used >= 3+2*tc.extensions && tc.phase != "implement"), tc.extensions)
				if !strings.Contains(string(raw), expected) {
					t.Errorf("missing restored state %q", expected)
				}
				http.Error(w, "model unavailable", http.StatusBadRequest)
			}))
			defer server.Close()
			path := agentConfig(t, server.URL)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("  react:\n    exploration_budget:\n      initial_turns: 99\n      extension_turns: 88\n      max_extensions: 77\n      decision_retries: 66\n      modification_window_turns: 55\n")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			_, err = application.Resume(ctx, application.Options{RunID: run.RunID, ConfigPath: path, Timeout: time.Second, Stderr: io.Discard})
			if err == nil || requests != 1 {
				t.Fatalf("err=%v requests=%d", err, requests)
			}
			saved, err := store.Execution(ctx, run.RunID)
			if err != nil {
				t.Fatal(err)
			}
			var after struct {
				UsedTurns   int
				Exploration map[string]json.RawMessage
			}
			if err := json.Unmarshal(saved.ExecutionProgress, &after); err != nil {
				t.Fatal(err)
			}
			exploration["Used"] = tc.expectedUsed
			if after.UsedTurns != 8 {
				t.Fatalf("total budget refreshed: %s", saved.ExecutionProgress)
			}
			for key, value := range exploration {
				expected, _ := json.Marshal(value)
				var a, b any
				_ = json.Unmarshal(expected, &a)
				_ = json.Unmarshal(after.Exploration[key], &b)
				if !reflect.DeepEqual(a, b) {
					t.Errorf("%s changed: expected %s got %s", key, expected, after.Exploration[key])
				}
			}
		})
	}
}

func TestResumeLegacyExplorationNeverInventsModificationOrLosesKnownConsumption(t *testing.T) {
	for _, tc := range []struct {
		name     string
		progress bool
		unknown  bool
		used     int
	}{{"missing-progress", false, false, 2}, {"old-progress", true, false, 15}, {"old-progress-without-counter", true, false, 2}, {"unknown-outcome", true, true, 15}} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := runStore(t)
			ctx := context.Background()
			run, err := store.Create(ctx, "", "fix main.go", t.TempDir(), "react")
			if err != nil {
				t.Fatal(err)
			}
			durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, CompletionCriteria: []string{"fixed"}, ToolExecutions: []*state.ToolResult{{ToolCallId: "read", ToolName: "read_file", Arguments: `{"path":"main.go"}`, Status: "success", Batch: 1, Result: "[]"}, {ToolCallId: "patch", ToolName: "apply_patch", Arguments: `{"patch":"old operation"}`, Status: "success", Batch: 2, Result: "[]"}}}
			if tc.progress {
				durable.ExecutionProgress = json.RawMessage(fmt.Sprintf(`{"ExecutionID":%q,"UsedTurns":15,"ObservedThroughBatch":2}`, run.RunID))
			}
			if tc.name == "old-progress-without-counter" {
				durable.ExecutionProgress = json.RawMessage(fmt.Sprintf(`{"ExecutionID":%q,"ObservedThroughBatch":2}`, run.RunID))
			}
			if tc.unknown {
				durable.ToolExecutions[1].Status = ""
			}
			if err := store.SaveDurableCheckpoint(ctx, &run, durable, "react"); err != nil {
				t.Fatal(err)
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				raw, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(raw), fmt.Sprintf("phase=exploring used=%d/12", tc.used)) {
					t.Errorf("legacy consumption not retained")
				}
				http.Error(w, "model unavailable", 400)
			}))
			defer server.Close()
			opts := application.Options{RunID: run.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard}
			_, err = application.Resume(ctx, opts)
			if err == nil {
				t.Fatal("expected failure")
			}
			if tc.unknown {
				_, err = application.Resume(ctx, opts)
				if err == nil || !strings.Contains(err.Error(), "unknown tool outcome") || requests != 0 {
					t.Fatalf("unknown result replayed: err=%v requests=%d", err, requests)
				}
				return
			}
			saved, err := store.Execution(ctx, run.RunID)
			if err != nil {
				t.Fatal(err)
			}
			var p struct {
				UsedTurns   int
				Exploration struct {
					Used                  int
					ModificationConfirmed bool
					ModificationEvidence  []any
				}
			}
			if err := json.Unmarshal(saved.ExecutionProgress, &p); err != nil {
				t.Fatal(err)
			}
			if requests != 1 || p.UsedTurns < tc.used || p.Exploration.Used < tc.used || p.Exploration.ModificationConfirmed || len(p.Exploration.ModificationEvidence) > 0 {
				t.Fatalf("legacy progress=%s requests=%d", saved.ExecutionProgress, requests)
			}
		})
	}
}

func TestResumeExplorationAcrossRepeatedFailuresEventuallyModifiesAndVerifies(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run, err := store.Create(ctx, "", "change package to main and verify", workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	run.CompletionCriteria = []string{"package main and go test passes"}
	if err := store.Save(ctx, &run); err != nil {
		t.Fatal(err)
	}
	tool := func(id, name string, args any) []any {
		raw, _ := json.Marshal(args)
		return []any{map[string]any{"type": "function_call", "id": id, "call_id": id, "name": name, "arguments": string(raw)}}
	}
	message := func(raw string) []any {
		return []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": raw}}}}
	}
	outputs := [][]any{
		tool("read", "read_file", map[string]string{"path": "main.go"}), nil,
		message(`{"exploration":{"task_type":"modification","basis":"requested package change","choice":"extend","gap":"missing module context","blocking_reason":"need package context","investigation":"list workspace","exit_condition":"package context confirmed"}}`),
		tool("list", "list_dir", map[string]string{"path": "."}), nil,
		message(`{"exploration":{"choice":"implement","target":"main.go: change package","target_files":["main.go"],"evidence":"main.go declares package old","next_operation":"apply patch"}}`),
		tool("noop", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package old\n"}), nil,
		tool("edit", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
		tool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}), nil,
		message(`{"status":"completed","answer":"changed and verified"}`),
	}
	step := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "你是 ReAct agent 的工具结果观察器") {
			writeResult(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		if step >= len(outputs) {
			t.Error("unexpected extra model decision")
			http.Error(w, "script exhausted", 400)
			return
		}
		output := outputs[step]
		step++
		if output == nil {
			http.Error(w, "model temporarily unavailable", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "decision", "status": "completed", "output": output})
	}))
	defer server.Close()
	path := agentConfig(t, server.URL)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("  react:\n    allow_tools: [read_file, list_dir, apply_patch, exec]\n    exploration_budget:\n      initial_turns: 2\n      extension_turns: 2\n      max_extensions: 1\n      modification_window_turns: 3\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	opts := application.Options{RunID: run.RunID, ConfigPath: path, Timeout: 20 * time.Second, Stderr: io.Discard}
	for attempt := 0; attempt < 5; attempt++ {
		results, resumeErr := application.Resume(ctx, opts)
		saved, err := store.Execution(ctx, run.RunID)
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			Exploration struct {
				Used, Extensions, ModificationLeft int
				Policy                             struct{ InitialTurns, ExtensionTurns, MaxExtensions, ModificationWindowTurns int }
				ModificationEvidence               []any
				Accounting                         []struct{ Purpose, Outcome string }
			}
		}
		// 配置字段使用 snake_case，保留原始值单独验证。
		var stateJSON struct {
			Exploration struct{ Policy map[string]int }
		}
		if err := json.Unmarshal(saved.ExecutionProgress, &p); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(saved.ExecutionProgress, &stateJSON)
		expectedUsed := 2
		if attempt > 0 {
			expectedUsed = 4
		}
		expectedExtensions := 0
		if attempt > 0 {
			expectedExtensions = 1
		}
		if p.Exploration.Used != expectedUsed || p.Exploration.Extensions != expectedExtensions || stateJSON.Exploration.Policy["initial_turns"] != 2 || stateJSON.Exploration.Policy["modification_window_turns"] != 3 {
			t.Fatalf("attempt %d progress=%s", attempt, saved.ExecutionProgress)
		}
		if attempt == 2 && p.Exploration.ModificationLeft != 2 {
			t.Fatalf("window refreshed: %s", saved.ExecutionProgress)
		}
		if attempt < 4 {
			if resumeErr == nil {
				t.Fatalf("attempt %d should be resumable failure", attempt)
			}
		} else {
			if resumeErr != nil || len(results) != 1 || results[0].Answer != "changed and verified" || len(p.Exploration.ModificationEvidence) != 1 || len(saved.ToolExecutions) != 5 {
				t.Fatalf("results=%v err=%v progress=%s tools=%d", results, resumeErr, saved.ExecutionProgress, len(saved.ToolExecutions))
			}
			var blocks []struct{ Text string }
			_ = json.Unmarshal([]byte(saved.ToolExecutions[4].Result), &blocks)
			if len(blocks) != 1 || !strings.Contains(blocks[0].Text, `"exit_code":0`) {
				t.Fatalf("verification not successful: %+v", blocks)
			}
		}
		if attempt == 0 {
			content, _ := os.ReadFile(path)
			content = []byte(strings.ReplaceAll(string(content), "initial_turns: 2", "initial_turns: 99"))
			content = []byte(strings.ReplaceAll(string(content), "modification_window_turns: 3", "modification_window_turns: 99"))
			if err := os.WriteFile(path, content, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || string(content) != "package main\n" || step != len(outputs) {
		t.Fatalf("content=%q err=%v decisions=%d", content, err, step)
	}
}

func TestNewExecutionDoesNotInheritConversationExplorationBudget(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	workspace := t.TempDir()
	previous, err := store.Create(ctx, "", "previous modification", workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	previous.Status = "interrupted"
	durable := &state.DurableState{RunId: previous.RunID, ExecutionID: previous.RunID, ExecutionProgress: json.RawMessage(fmt.Sprintf(`{"ExecutionID":%q,"UsedTurns":40,"Exploration":{"Policy":{"initial_turns":1,"extension_turns":1,"max_extensions":1,"decision_retries":0,"modification_window_turns":1},"Used":2,"Extensions":1,"Phase":"implement","TaskType":"modification","ModificationLeft":0}}`, previous.RunID))}
	if err := store.SaveDurableCheckpoint(ctx, &previous, durable, "react"); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Create(ctx, previous.ConvID, "new modification", workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	fresh.CompletionCriteria = []string{"fixed"}
	if err := store.Save(ctx, &fresh); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), "instance="+fresh.RunID+" task_type= phase=exploring used=0/12") || !strings.Contains(string(raw), "extensions=0") {
			t.Error("new instance inherited exploration budget")
		}
		http.Error(w, "model unavailable", 400)
	}))
	defer server.Close()
	opts := application.Options{RunID: previous.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard}
	if _, err := application.Resume(ctx, opts); err == nil || !strings.Contains(err.Error(), "interrupted") || requests != 0 {
		t.Fatalf("interrupted run resumed: err=%v requests=%d", err, requests)
	}
	opts.RunID = fresh.RunID
	if _, err := application.Resume(ctx, opts); err == nil || requests != 1 {
		t.Fatalf("new run failed to initialize: err=%v requests=%d", err, requests)
	}
	saved, err := store.Execution(ctx, fresh.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		UsedTurns   int
		Exploration struct {
			Used, Extensions, ModificationLeft int
			TaskType, Phase                    string
			Handoffs, ModificationEvidence     []any
		}
	}
	_ = json.Unmarshal(saved.ExecutionProgress, &p)
	if p.UsedTurns != 1 || p.Exploration.Used != 1 || p.Exploration.Extensions != 0 || p.Exploration.ModificationLeft != 0 || p.Exploration.TaskType != "" || p.Exploration.Phase != "exploring" || len(p.Exploration.Handoffs) > 0 || len(p.Exploration.ModificationEvidence) > 0 {
		t.Fatalf("new progress=%s", saved.ExecutionProgress)
	}
}
