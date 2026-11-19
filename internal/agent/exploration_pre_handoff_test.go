package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
)

func TestReactPreHandoffModificationCanVerifyAndCompleteAtThreshold(t *testing.T) {
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	_, err := runCommandExploration(t, t.TempDir(), [][]any{
		modificationTool("edit", "apply_patch", map[string]string{"patch": "--- /dev/null\n+++ main.go\n@@ -0,0 +1 @@\n+package main\n"}),
		modificationTool("inspect", "read_file", map[string]any{"path": "main.go"}),
		modificationTool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"changed and verified"}`)},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReactRejectsNoOperationImplementationHandoff(t *testing.T) {
	workspace := t.TempDir()
	durable := &state.DurableState{ExecutionID: "no-operation", Goal: "fix main.go"}
	// 使用 skip_next_operation=1 明确标记无需进一步操作，应被拒绝。
	err := runModificationScript(t, workspace, durable, config.DefaultExplorationBudget(), [][]any{
		{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"implement","target_files":["main.go"],"target":"fix main.go","evidence":"inspected","next_operation":"","skip_next_operation":1}}`)},
		modificationHandoff(),
		modificationTool("edit", "apply_patch", map[string]string{"patch": "--- /dev/null\n+++ main.go\n@@ -0,0 +1 @@\n+package main\n"}),
		{observationMessage(`{"status":"completed","answer":"fixed"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Exploration struct{ Handoffs []state.ExplorationDecision }
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Exploration.Handoffs) != 1 || saved.Exploration.Handoffs[0].NextOperation != "apply patch" {
		t.Fatalf("invalid handoff accepted: %s", durable.ExecutionProgress)
	}
}

func TestReactPreHandoffFailedVerificationCannotComplete(t *testing.T) {
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	_, err := runCommandExploration(t, t.TempDir(), [][]any{
		modificationTool("edit", "apply_patch", map[string]string{"patch": "--- /dev/null\n+++ main.go\n@@ -0,0 +1 @@\n+package main\n"}),
		modificationTool("inspect", "read_file", map[string]string{"path": "main.go"}),
		modificationTool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "missing.go"}}),
		{observationMessage(`{"status":"completed","answer":"claimed done"}`)},
	}, policy)
	if err == nil {
		t.Fatal("completion accepted without successful verification")
	}
}

func TestReactResumeRetainsPreHandoffCommandEvidence(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/package old/package main/", "main.go"}}),
		modificationTool("inspect", "read_file", map[string]string{"path": "main.go"}),
		modificationTool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"verified"}`)},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟完成结果提交之前的恢复，沿用已持久化的工具和预算证据。
	durable.ExecutionResult = nil
	raw, err := json.Marshal(durable)
	if err != nil {
		t.Fatal(err)
	}
	var restored state.DurableState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err := runModificationScript(t, workspace, &restored, policy, [][]any{{observationMessage(`{"status":"completed","answer":"verified"}`)}}); err != nil {
		t.Fatal(err)
	}
}

func TestReactPreHandoffLatestFailedVerificationOverridesEarlierSuccess(t *testing.T) {
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	_, err := runCommandExploration(t, t.TempDir(), [][]any{
		modificationTool("edit", "apply_patch", map[string]string{"patch": "--- /dev/null\n+++ main.go\n@@ -0,0 +1 @@\n+package main\n"}),
		modificationTool("inspect", "read_file", map[string]string{"path": "main.go"}),
		modificationTool("pass", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		modificationTool("fail", "exec", map[string]any{"command": "go", "args": []string{"test", "missing.go"}}),
		{observationMessage(`{"status":"completed","answer":"claimed done"}`)},
	}, policy)
	if err == nil {
		t.Fatal("old verification masked latest failure")
	}
}
