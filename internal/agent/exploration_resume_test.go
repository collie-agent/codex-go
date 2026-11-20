package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
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

// 故障注入只发生在公开的检查点边界；恢复使用序列化成功的副本。
func TestReactResumeAfterResultBeforePhaseCheckpoint(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{ExecutionID: "result-before-phase", Goal: "fix main.go", CompletionCriteria: []string{"changed"}}
	outputs := [][]any{modificationHandoff(), modificationTool("edit", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-old\n+new\n"})}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(string(body.Input), "你是 ReAct agent 的工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		if requests >= len(outputs) {
			t.Error("model released after checkpoint failure")
			http.Error(w, "unexpected request", 400)
			return
		}
		output := outputs[requests]
		requests++
		writeObservationResponse(w, output)
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	var saved []byte
	resultSaved := false
	failure := errors.New("phase checkpoint unavailable")
	process.SetCheckpoint(func() error {
		if resultSaved {
			return failure
		}
		saved, _ = json.Marshal(durable)
		if len(durable.ToolExecutions) > 0 && durable.ToolExecutions[0].Status != "" {
			resultSaved = true
		}
		return nil
	})
	policy := config.DefaultExplorationBudget()
	policy.ModificationWindowTurns = 1
	_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if !errors.Is(err, failure) || !resultSaved || requests != 2 {
		t.Fatalf("err=%v resultSaved=%t requests=%d", err, resultSaved, requests)
	}
	var restored state.DurableState
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	err = runModificationScript(t, workspace, &restored, config.ExplorationBudget{}, [][]any{{observationMessage(`{"status":"completed","answer":"fixed"}`)}})
	if err != nil {
		t.Fatalf("confirmed edit lost at crash boundary: %v; progress=%s", err, restored.ExecutionProgress)
	}
	if len(restored.ToolExecutions) != 1 {
		t.Fatalf("tool replayed: %d", len(restored.ToolExecutions))
	}
	var progress struct {
		Exploration struct {
			ModificationEvidence  []any
			Used                  int
			ModificationConfirmed bool
		}
	}
	_ = json.Unmarshal(restored.ExecutionProgress, &progress)
	if !progress.Exploration.ModificationConfirmed || len(progress.Exploration.ModificationEvidence) != 1 || progress.Exploration.Used != 0 {
		t.Fatalf("progress=%s", restored.ExecutionProgress)
	}
}

func TestReactResumeDoesNotReopenExhaustedDecisionCorrections(t *testing.T) {
	durable := &state.DurableState{ExecutionID: "invalid-decision", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}, ExecutionProgress: json.RawMessage(`{"ExecutionID":"invalid-decision","UsedTurns":4,"Exploration":{"Policy":{"initial_turns":1,"extension_turns":1,"max_extensions":1,"decision_retries":1,"modification_window_turns":3},"Used":1,"Invalid":2,"Pending":true,"TaskType":"modification","Phase":"exploring"}}`)}
	err := runModificationScript(t, t.TempDir(), durable, config.ExplorationBudget{}, [][]any{modificationHandoff(), modificationTool("must-not-run", "apply_patch", map[string]string{"patch": "--- /dev/null\n+++ main.go\n@@ -0,0 +1 @@\n+new\n"})})
	if err == nil || !strings.Contains(err.Error(), "decision corrections exhausted") || len(durable.ToolExecutions) != 0 {
		t.Fatalf("err=%v tools=%v", err, durable.ToolExecutions)
	}
}

func TestReactResumeHonorsSavedUnableDecision(t *testing.T) {
	durable := &state.DurableState{ExecutionID: "unable", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}, ExecutionProgress: json.RawMessage(`{"ExecutionID":"unable","UsedTurns":4,"Exploration":{"Policy":{"initial_turns":1,"extension_turns":1,"max_extensions":1,"decision_retries":1,"modification_window_turns":3},"Used":1,"TaskType":"modification","Phase":"unable","Handoff":{"choice":"unable","reason":"missing credentials","execution_state":"no changes made"}}}`)}
	err := runModificationScript(t, t.TempDir(), durable, config.ExplorationBudget{}, [][]any{modificationHandoff(), modificationTool("must-not-run", "apply_patch", map[string]string{"patch": "--- /dev/null\n+++ main.go\n@@ -0,0 +1 @@\n+new\n"})})
	if err == nil || !strings.Contains(err.Error(), "missing credentials") || len(durable.ToolExecutions) != 0 {
		t.Fatalf("err=%v tools=%v", err, durable.ToolExecutions)
	}
}

func TestReactResumeFinalizingPreservesConfirmedProgressWithoutAnotherSummary(t *testing.T) {
	durable := &state.DurableState{ExecutionID: "closing", Goal: "fix main.go", CompletionCriteria: []string{"tests pass"},
		ToolExecutions:    []*state.ToolResult{{ToolCallId: "verify", ToolName: "exec", Arguments: `{"command":"go","args":["test","main.go"]}`, Status: "success", Batch: 2, Result: `[{"type":"text","text":"test failed: undefined value"}]`}},
		ExecutionProgress: json.RawMessage(`{"ExecutionID":"closing","UsedTurns":8,"Finalizing":true,"StopReason":"modification window exhausted","Exploration":{"Policy":{"initial_turns":1,"extension_turns":1,"max_extensions":1,"decision_retries":1,"modification_window_turns":3},"Used":1,"TaskType":"modification","Phase":"implement","ModificationEvidence":[{"Handoff":1,"Path":"main.go","Before":"old-hash","After":"new-hash","Batch":1}],"Accounting":[{"Batch":2,"Purpose":"verification","Outcome":"known","Applied":true,"Calls":{"verify":"verification"}}]}}`)}
	// 若恢复重试总结，这个响应会掩盖原来的失败；模型不得再次被调用。
	err := runModificationScript(t, t.TempDir(), durable, config.ExplorationBudget{}, [][]any{{observationMessage(`{"status":"failed","answer":"unexpected new summary"}`)}})
	if err == nil {
		t.Fatal("expected preserved failure")
	}
	var result ReactResult
	if err := json.Unmarshal(durable.ExecutionResult, &result); err != nil {
		t.Fatal(err)
	}
	observations := strings.Join(result.Observation, "\n")
	if strings.Contains(result.Answer, "unexpected new summary") || !strings.Contains(observations, "Confirmed target change: main.go") || !strings.Contains(observations, "Recorded verification:") || !strings.Contains(observations, "test failed: undefined value") || !strings.Contains(observations, "tests pass") {
		t.Fatalf("result=%+v", result)
	}
}

func TestReactResumeLogsOriginalExplorationPolicy(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	durable := &state.DurableState{ExecutionID: "logged-resume", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}, ExecutionProgress: json.RawMessage(`{"ExecutionID":"logged-resume","UsedTurns":5,"Exploration":{"Policy":{"initial_turns":1,"extension_turns":2,"max_extensions":1,"decision_retries":1,"modification_window_turns":3},"Used":1,"Extensions":1,"TaskType":"modification","Phase":"implement","ModificationLeft":2}}`)}
	_ = runModificationScript(t, t.TempDir(), durable, config.ExplorationBudget{}, [][]any{nil})
	for _, expected := range []string{"instance=logged-resume restore=true legacy_exploration=false", "initial_turns=1 extension_turns=2 max_extensions=1 decision_retries=1 modification_window_turns=3", "exploration=1/3 extensions=1 invalid=0 modification_left=2 remaining_total=55"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("missing restore log %q", expected)
		}
	}
}

func TestReactResumeNeverUsesCurrentWorkspaceAsHistoricalModificationEvidence(t *testing.T) {
	workspace := t.TempDir()
	// 这些内容可能来自崩溃后的外部编辑，不能补作历史工具调用的 After 快照。
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{ExecutionID: "missing-after", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}, ToolExecutions: []*state.ToolResult{{ToolCallId: "edit", ToolName: "apply_patch", Arguments: `{"patch":"old patch"}`, Status: "success", Batch: 1, Result: "[]"}}, ExecutionProgress: json.RawMessage(`{"ExecutionID":"missing-after","UsedTurns":3,"Exploration":{"Policy":{"initial_turns":1,"extension_turns":1,"max_extensions":1,"decision_retries":1,"modification_window_turns":1},"TaskType":"modification","Phase":"implement","Used":1,"ModificationLeft":0,"Handoff":{"target_files":["main.go"]},"Handoffs":[{"target_files":["main.go"]}],"Accounting":[{"Batch":1,"Handoff":1,"Purpose":"modification","Calls":{"edit":"modification"},"Before":{"main.go":{"Known":true,"Version":"absent"}},"Outcome":"pending"}]}}`)}
	err := runModificationScript(t, workspace, durable, config.ExplorationBudget{}, [][]any{{observationMessage(`{"status":"completed","answer":"unsupported success"}`)}})
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	var p struct {
		Exploration struct {
			ModificationConfirmed bool
			ModificationEvidence  []any
			Accounting            []struct{ Outcome string }
		}
	}
	_ = json.Unmarshal(durable.ExecutionProgress, &p)
	if p.Exploration.ModificationConfirmed || len(p.Exploration.ModificationEvidence) > 0 || p.Exploration.Accounting[0].Outcome != "unknown" || len(durable.ToolExecutions) != 1 {
		t.Fatalf("progress=%s tools=%d", durable.ExecutionProgress, len(durable.ToolExecutions))
	}
	content, _ := os.ReadFile(filepath.Join(workspace, "main.go"))
	if string(content) != "new\n" {
		t.Fatalf("changes rolled back: %q", content)
	}
}

func TestReactResumeReconcilesSettledProcessEvidenceWithoutReattachingSession(t *testing.T) {
	durable := &state.DurableState{ExecutionID: "settled-process", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}, ToolExecutions: []*state.ToolResult{{ToolCallId: "poll", ToolName: "exec_poll", Arguments: `{"session_id":"old-process"}`, Status: "success", Batch: 2, Result: `[{"type":"text","text":"{\"state\":\"exited\",\"exit_code\":0}"}]`}}, ExecutionProgress: json.RawMessage(`{"ExecutionID":"settled-process","UsedTurns":4,"Exploration":{"Policy":{"initial_turns":2,"extension_turns":1,"max_extensions":1,"decision_retries":1,"modification_window_turns":2},"TaskType":"modification","Phase":"implement","Used":1,"ModificationLeft":0,"Handoff":{"target_files":["main.go"]},"Handoffs":[{"target_files":["main.go"]}],"SessionPurposes":{"old-process":"modification"},"Accounting":[{"Batch":2,"SinceBatch":1,"Handoff":1,"Purpose":"modification","Calls":{"poll":"modification"},"Before":{"main.go":{"Known":true,"Version":"absent"}},"After":{"main.go":{"Known":true,"Version":"saved-hash"}},"Outcome":"known"}]}}`)}
	err := runModificationScript(t, t.TempDir(), durable, config.ExplorationBudget{}, [][]any{{observationMessage(`{"status":"completed","answer":"fixed"}`)}})
	if err != nil {
		t.Fatalf("settled process change lost: %v", err)
	}
	var p struct {
		Exploration struct {
			ModificationConfirmed bool
			ModificationEvidence  []any
			SessionPurposes       map[string]string
		}
	}
	_ = json.Unmarshal(durable.ExecutionProgress, &p)
	if !p.Exploration.ModificationConfirmed || len(p.Exploration.ModificationEvidence) != 1 || len(p.Exploration.SessionPurposes) > 0 || len(durable.ToolExecutions) != 1 {
		t.Fatalf("progress=%s", durable.ExecutionProgress)
	}
}

func TestReactRejectedDecisionPersistsReasonForResume(t *testing.T) {
	durable := &state.DurableState{ExecutionID: "rejected-reason", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}, ExecutionProgress: json.RawMessage(`{"ExecutionID":"rejected-reason","UsedTurns":2,"Exploration":{"Policy":{"initial_turns":1,"extension_turns":1,"max_extensions":1,"decision_retries":1,"modification_window_turns":3},"TaskType":"modification","Phase":"exploring","Used":1}}`)}
	err := runModificationScript(t, t.TempDir(), durable, config.ExplorationBudget{}, [][]any{{observationMessage(`{"exploration":{"choice":"implement"}}`)}, nil})
	if err == nil {
		t.Fatal("expected model outage after rejected decision")
	}
	var p struct {
		Exploration struct {
			Invalid   int
			Pending   bool
			Rejection string
		}
	}
	_ = json.Unmarshal(durable.ExecutionProgress, &p)
	if p.Exploration.Invalid != 1 || !p.Exploration.Pending || !strings.Contains(p.Exploration.Rejection, "implement requires target, evidence, next_operation") {
		t.Fatalf("correction reason lost: %s", durable.ExecutionProgress)
	}
}

func TestReactResumePreservesWindowOnRejectedInvestigation(t *testing.T) {
	workspace := t.TempDir()
	durable := &state.DurableState{ExecutionID: "rejected-read", Goal: "change main.go", CompletionCriteria: []string{"changed"}}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	policy.ModificationWindowTurns = 2
	read := func(id string) []any {
		return modificationTool(id, "list_dir", map[string]string{"path": "."})
	}
	checkBudget := func(d *state.DurableState, left int) {
		t.Helper()
		var progress struct {
			Exploration struct {
				Used, ModificationLeft int
				Phase                  string
			}
		}
		if err := json.Unmarshal(d.ExecutionProgress, &progress); err != nil {
			t.Fatal(err)
		}
		if progress.Exploration.Used != 1 || progress.Exploration.ModificationLeft != left || progress.Exploration.Phase != "implement" {
			t.Fatalf("unexpected budget after rejection: %+v", progress.Exploration)
		}
		if len(d.ToolExecutions) != 1 || d.ToolExecutions[0].ToolCallId != "initial" {
			t.Fatalf("unexpected tool history: %+v", d.ToolExecutions)
		}
	}
	err := runModificationScript(t, workspace, durable, policy, [][]any{read("initial"), modificationHandoff(), read("first-denied"), nil})
	if err == nil || !strings.Contains(err.Error(), "400 Bad Request") {
		t.Fatalf("expected outage after first rejection, got %v", err)
	}
	checkBudget(durable, 2)

	snapshot, err := json.Marshal(durable)
	if err != nil {
		t.Fatal(err)
	}
	var restored state.DurableState
	if err := json.Unmarshal(snapshot, &restored); err != nil {
		t.Fatal(err)
	}
	err = runModificationScript(t, workspace, &restored, config.ExplorationBudget{}, [][]any{read("last-denied"), nil})
	if err == nil || !strings.Contains(err.Error(), "400 Bad Request") {
		t.Fatalf("expected another correction request after rejection, got %v", err)
	}
	checkBudget(&restored, 2)
}
