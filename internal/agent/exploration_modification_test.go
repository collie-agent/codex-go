package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactHandoffAppliesRealPatchAndCompletes(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input        json.RawMessage
			Instructions string `json:"instructions"`
			Tools        []struct{ Name string }
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Patch observation uses the same external model boundary and is independent of business decisions.
		if strings.Contains(string(body.Input), "你是 ReAct agent 的工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		calls++
		var names []string
		for _, tool := range body.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		switch calls {
		case 1:
			for _, name := range []string{"read_file", "list_dir", "glob", "grep", "load_skill"} {
				if !slices.Contains(names, name) {
					t.Errorf("exploration request missing %s: %v", name, names)
				}
			}
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc-list", "call_id": "list", "name": "list_dir", "arguments": `{"path":"."}`}})
		case 2:
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"implement","target":"main.go: replace old with new","evidence":"existing line is old","next_operation":"apply patch"}}`)})
		case 3:
			want := []string{"apply_patch", "exec", "exec_poll", "exec_start", "read_tool_result"}
			if !slices.Equal(names, want) {
				t.Errorf("implementation request tools = %v, want %v", names, want)
			}
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "patch", "name": "apply_patch", "arguments": `{"patch":"--- main.go\n+++ main.go\n@@ -1 +1 @@\n-old\n+new\n"}`}})
		default:
			writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"fixed"}`)})
		}
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{ExecutionID: "modification", Goal: "fix main.go", CompletionCriteria: []string{"new line"}}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	cfg := &config.AgentConfig{
		ExplorationBudget: policy,
		AllowTools: map[config.McpToolName]struct{}{
			config.ApplyPatchMCPToolName: {}, config.ListDirMCPToolName: {}, config.ReadFileMCPToolName: {},
			config.GlobMCPToolName: {}, config.GrepMCPToolName: {}, config.ExecMCPToolName: {},
			config.ExecStartMCPToolName: {}, config.ExecPollMCPToolName: {},
		},
		LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
	}
	var checkpoint []byte
	process.SetCheckpoint(func() error {
		if calls != 2 {
			return nil
		}
		var err error
		checkpoint, err = json.Marshal(durable)
		return err
	})
	assertPatch := func(process *ReactProcess[ReactResult], durable *state.DurableState) {
		t.Helper()
		result, _, err := process.Run(cfg, durable)
		if err != nil || result.Status != CompletedReactStatus || calls != 4 {
			t.Fatalf("result=%+v requests=%d err=%v", result, calls, err)
		}
		content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
		if err != nil || string(content) != "new\n" {
			t.Fatalf("file=%q err=%v", content, err)
		}
		var saved struct {
			Exploration struct {
				Used                  int
				ModificationConfirmed bool
			}
		}
		if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil || saved.Exploration.Used != 1 || !saved.Exploration.ModificationConfirmed {
			t.Fatalf("unexpected exploration progress: %s, err=%v", durable.ExecutionProgress, err)
		}
	}
	assertPatch(process, durable)

	var restored state.DurableState
	if err := json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls = 2 // 恢复后直接检查第一轮实施请求。
	process, err = NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	assertPatch(process, &restored)
}

func TestReactModificationWindowRequiresNetTargetChange(t *testing.T) {
	for _, tc := range []struct{ name, patch string }{
		{"same-content", "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-old\n+old\n"},
		{"unrelated-file", "--- /dev/null\n+++ unrelated.go\n@@ -0,0 +1 @@\n+unrelated\n"},
		{"cancelled-changes", "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-old\n+temporary\n--- main.go\n+++ main.go\n@@ -1 +1 @@\n-temporary\n+old\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input        json.RawMessage
					Instructions string `json:"instructions"`
				}
				_ = json.NewDecoder(r.Body).Decode(&request)
				if strings.Contains(string(request.Input), "你是 ReAct agent 的工具结果观察器") {
					writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
					return
				}
				calls++
				switch {
				case calls == 1:
					writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"fix","choice":"implement","target":"main.go: fix line","target_files":["main.go"],"evidence":"old line","next_operation":"apply patch"}}`)})
				case calls <= 4:
					arguments, _ := json.Marshal(map[string]string{"patch": tc.patch})
					writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": fmt.Sprint("fc", calls), "call_id": fmt.Sprint("patch", calls), "name": "apply_patch", "arguments": string(arguments)}})
				default:
					writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"no target change"}`)})
				}
			}))
			defer server.Close()
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			durable := &state.DurableState{ExecutionID: "window", Goal: "fix main.go", CompletionCriteria: []string{"changed"}}
			_, _, err = process.Run(&config.AgentConfig{AllowTools: map[config.McpToolName]struct{}{config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
			if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
				t.Fatalf("expected window failure, err=%v", err)
			}
			var saved struct {
				Exploration struct {
					ModificationConfirmed bool
					ModificationLeft      int
				}
			}
			if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Exploration.ModificationConfirmed || saved.Exploration.ModificationLeft != 0 {
				t.Fatalf("invalid evidence: %s", durable.ExecutionProgress)
			}
		})
	}
}

func runModificationScript(t *testing.T, workspace string, durable *state.DurableState, policy config.ExplorationBudget, outputs [][]any) error {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(string(body.Input), "你是 ReAct agent 的工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		if calls >= len(outputs) {
			writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"script exhausted"}`)})
			return
		}
		output := outputs[calls]
		calls++
		if output == nil {
			http.Error(w, "temporary model outage", http.StatusBadRequest)
			return
		}
		writeObservationResponse(w, output)
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ApplyPatchMCPToolName: {}, config.ReadFileMCPToolName: {}, config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	return err
}
func modificationHandoff() []any {
	return []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"implement","target":"main.go: requested change","target_files":["main.go"],"evidence":"source inspected","next_operation":"apply patch"}}`)}
}
func modificationTool(id, name string, args any) []any {
	raw, _ := json.Marshal(args)
	return []any{map[string]any{"type": "function_call", "id": id, "call_id": id, "name": name, "arguments": string(raw)}}
}
func TestReactHandoffConfirmsFileCreationAndDeletion(t *testing.T) {
	for _, tc := range []struct {
		name, initial, patch, expected string
		exists                         bool
	}{
		{"create", "", "--- /dev/null\n+++ main.go\n@@ -0,0 +1 @@\n+new\n", "new\n", true},
		{"delete", "old\n", "--- main.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-old\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if tc.initial != "" {
				if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte(tc.initial), 0600); err != nil {
					t.Fatal(err)
				}
			}
			durable := &state.DurableState{ExecutionID: "files", Goal: "change main.go", CompletionCriteria: []string{"changed"}}
			err := runModificationScript(t, workspace, durable, config.ExplorationBudget{}, [][]any{modificationHandoff(), modificationTool("patch", "apply_patch", map[string]string{"patch": tc.patch}), {observationMessage(`{"status":"completed","answer":"done"}`)}})
			if err != nil {
				t.Fatal(err)
			}
			content, readErr := os.ReadFile(filepath.Join(workspace, "main.go"))
			if tc.exists && (readErr != nil || string(content) != tc.expected) {
				t.Fatalf("content=%q err=%v", content, readErr)
			}
			if !tc.exists && !os.IsNotExist(readErr) {
				t.Fatalf("delete err=%v", readErr)
			}
		})
	}
}
func TestReactRepeatedHandoffCannotRefreshWindow(t *testing.T) {
	workspace := t.TempDir()
	durable := &state.DurableState{ExecutionID: "repeat", Goal: "change main.go", CompletionCriteria: []string{"changed"}}
	err := runModificationScript(t, workspace, durable, config.ExplorationBudget{}, [][]any{modificationHandoff(), modificationTool("list", "list_dir", map[string]string{"path": "."}), modificationHandoff(), modificationHandoff()})
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		Exploration struct{ ModificationLeft int }
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.ModificationLeft != 0 {
		t.Fatalf("window refreshed: %s", durable.ExecutionProgress)
	}
}
func TestReactExhaustedExplorationDoesNotAllowWindowInvestigation(t *testing.T) {
	workspace := t.TempDir()
	durable := &state.DurableState{ExecutionID: "exhausted", Goal: "change main.go", CompletionCriteria: []string{"changed"}}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	err := runModificationScript(t, workspace, durable, policy, [][]any{modificationTool("list", "list_dir", map[string]string{"path": "."}), modificationHandoff(), modificationTool("read", "read_file", map[string]string{"path": "main.go"}), modificationTool("read2", "read_file", map[string]string{"path": "main.go"}), modificationTool("read3", "read_file", map[string]string{"path": "main.go"})})
	if err == nil || !strings.Contains(err.Error(), "exploration allowance exhausted") {
		t.Fatalf("err=%v", err)
	}
	if len(durable.ToolExecutions) != 1 {
		t.Fatalf("investigation escaped: %+v", durable.ToolExecutions)
	}
}

func TestReactResumeRetainsModificationWindow(t *testing.T) {
	workspace := t.TempDir()
	durable := &state.DurableState{ExecutionID: "resume-window", Goal: "change main.go", CompletionCriteria: []string{"changed"}}
	err := runModificationScript(t, workspace, durable, config.ExplorationBudget{}, [][]any{modificationHandoff(), modificationTool("first", "list_dir", map[string]any{"path": ".", "limit": 1}), nil})
	if err == nil {
		t.Fatal("expected external model outage")
	}
	var saved struct {
		Exploration struct{ ModificationLeft int }
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.ModificationLeft != 2 {
		t.Fatalf("window=%s", durable.ExecutionProgress)
	}
	policy := config.DefaultExplorationBudget()
	policy.ModificationWindowTurns = 99
	err = runModificationScript(t, workspace, durable, policy, [][]any{modificationTool("second", "list_dir", map[string]any{"path": ".", "limit": 2}), modificationTool("third", "list_dir", map[string]any{"path": ".", "limit": 3})})
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.ModificationLeft != 0 {
		t.Fatalf("restored window refreshed: %s", durable.ExecutionProgress)
	}
}

func TestReactCachedReadsCannotOutliveModificationWindow(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{ExecutionID: "cached-window", Goal: "change main.go", CompletionCriteria: []string{"changed"}}
	read := modificationTool("read", "read_file", map[string]string{"path": "main.go"})
	err := runModificationScript(t, workspace, durable, config.ExplorationBudget{}, [][]any{modificationHandoff(), read, modificationTool("read2", "read_file", map[string]string{"path": "main.go"}), modificationTool("read3", "read_file", map[string]string{"path": "main.go"})})
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	if len(durable.ToolExecutions) != 1 {
		t.Fatalf("cached reads executed: %d", len(durable.ToolExecutions))
	}
}
func TestReactResumeExhaustedModificationWindowDoesNotRequestDecision(t *testing.T) {
	workspace := t.TempDir()
	durable := &state.DurableState{ExecutionID: "zero-window", Goal: "change main.go", CompletionCriteria: []string{"changed"}}
	durable.ExecutionProgress = json.RawMessage(`{"ExecutionID":"zero-window","UsedTurns":4,"Exploration":{"Policy":{"initial_turns":12,"decision_retries":1,"modification_window_turns":3},"TaskType":"modification","Phase":"implement","Used":0,"ModificationLeft":0,"Handoff":{"target":"main.go: change"}}}`)
	err := runModificationScript(t, workspace, durable, config.ExplorationBudget{}, [][]any{modificationTool("must-not-run", "list_dir", map[string]string{"path": "."})})
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	if len(durable.ToolExecutions) != 0 {
		t.Fatalf("exhausted resume executed tools: %d", len(durable.ToolExecutions))
	}
}
