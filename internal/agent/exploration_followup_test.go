package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
)

func TestReactFollowupModificationAfterFailedVerification(t *testing.T) {
	workspace := t.TempDir()
	// 首次修改修复包名但保留未定义变量，第二次修改后验证才通过。
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\nvar value = broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationHandoff(),
		modificationTool("first-edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/package old/package main/", "main.go"}}),
		modificationTool("failing-test", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		modificationTool("investigate-failure", "read_file", map[string]string{"path": "main.go"}),
		{observationMessage(`{"exploration":{"choice":"implement","target":"main.go: define value","target_files":["main.go"],"evidence":"go test reports undefined broken; source confirms initializer","next_operation":"replace broken with 1"}}`)},
		modificationTool("second-edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/value = broken/value = 1/", "main.go"}}),
		modificationTool("passing-test", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"fixed and verified"}`)},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || string(content) != "package main\nvar value = 1\n" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	var saved struct {
		Exploration struct {
			Used, Extensions     int
			Handoffs             []struct{ Target string }
			ModificationEvidence []struct{ Handoff int }
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 || saved.Exploration.Extensions != 0 || len(saved.Exploration.Handoffs) != 2 || len(saved.Exploration.ModificationEvidence) != 2 {
		t.Fatalf("followup progress=%s", durable.ExecutionProgress)
	}
	if saved.Exploration.Handoffs[0].Target != "main.go: requested change" || saved.Exploration.Handoffs[1].Target != "main.go: define value" || saved.Exploration.ModificationEvidence[0].Handoff != 1 || saved.Exploration.ModificationEvidence[1].Handoff != 2 {
		t.Fatalf("handoff evidence=%s", durable.ExecutionProgress)
	}
	if len(durable.ToolExecutions) != 5 {
		t.Fatalf("executed tools=%d", len(durable.ToolExecutions))
	}
	for _, check := range []struct{ index, exitCode int }{{1, 1}, {4, 0}} {
		var blocks []struct{ Text string }
		if err := json.Unmarshal([]byte(durable.ToolExecutions[check.index].Result), &blocks); err != nil || len(blocks) != 1 {
			t.Fatalf("verification result=%v err=%v", blocks, err)
		}
		var command struct {
			ExitCode *int `json:"exit_code"`
		}
		if err := json.Unmarshal([]byte(blocks[0].Text), &command); err != nil || command.ExitCode == nil || *command.ExitCode != check.exitCode {
			t.Fatalf("verification exit=%v expected=%d err=%v", command.ExitCode, check.exitCode, err)
		}
	}
}

func TestReactFollowupWindowCannotReuseFirstModification(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.ModificationWindowTurns = 1
	durable := &state.DurableState{ExecutionID: "followup-window", Goal: "fix main.go", CompletionCriteria: []string{"changed"}}
	err := runModificationScript(t, workspace, durable, policy, [][]any{
		modificationHandoff(),
		modificationTool("first-patch", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-old\n+new\n"}),
		modificationHandoff(),
		modificationTool("empty-followup", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-new\n+new\n"}),
	})
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		Exploration struct {
			Handoffs              []any
			ModificationLeft      int
			ModificationConfirmed bool
			ModificationEvidence  []struct{ Handoff int }
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Exploration.Handoffs) != 2 || saved.Exploration.ModificationLeft != 0 || saved.Exploration.ModificationConfirmed || len(saved.Exploration.ModificationEvidence) != 1 || saved.Exploration.ModificationEvidence[0].Handoff != 1 {
		t.Fatalf("progress=%s", durable.ExecutionProgress)
	}
}

func TestReactSmallPatchDoesNotExemptFurtherInvestigation(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	durable := &state.DurableState{ExecutionID: "small-patch", Goal: "fix main.go", CompletionCriteria: []string{"changed"}}
	err := runModificationScript(t, workspace, durable, policy, [][]any{
		modificationHandoff(),
		modificationTool("patch", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-old\n+new\n"}),
		modificationTool("allowed-read", "read_file", map[string]string{"path": "main.go"}),
		modificationTool("blocked-list", "list_dir", map[string]string{"path": "."}),
		modificationTool("blocked-read", "read_file", map[string]string{"path": "main.go"}),
	})
	if err == nil || !strings.Contains(err.Error(), "decision corrections exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		Exploration struct {
			Used, Invalid                  int
			Pending, ModificationConfirmed bool
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if len(durable.ToolExecutions) != 2 || saved.Exploration.Used != 1 || saved.Exploration.Invalid != 2 || !saved.Exploration.Pending || !saved.Exploration.ModificationConfirmed {
		t.Fatalf("progress=%s tools=%d", durable.ExecutionProgress, len(durable.ToolExecutions))
	}
}

func TestReactFollowupExtensionsShareOriginalLimit(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns, policy.ExtensionTurns, policy.MaxExtensions = 1, 1, 1
	extend := []any{observationMessage(`{"exploration":{"choice":"extend","gap":"missing caller context","blocking_reason":"cannot choose fix without caller","investigation":"read caller","exit_condition":"caller identified"}}`)}
	durable := &state.DurableState{ExecutionID: "followup-extensions", Goal: "fix main.go", CompletionCriteria: []string{"changed"}}
	err := runModificationScript(t, workspace, durable, policy, [][]any{
		modificationHandoff(),
		modificationTool("first-patch", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-old\n+new\n"}),
		modificationTool("first-read", "read_file", map[string]string{"path": "main.go"}),
		extend,
		modificationTool("second-read", "list_dir", map[string]string{"path": "."}),
		modificationHandoff(),
		modificationTool("second-patch", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-new\n+fixed\n"}),
		extend,
		extend,
	})
	if err == nil || !strings.Contains(err.Error(), "extension limit exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		Exploration struct {
			Used, Extensions, Invalid int
			Handoffs                  []any
			ModificationEvidence      []struct{ Handoff int }
			Rejection                 string
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 2 || saved.Exploration.Extensions != 1 || saved.Exploration.Invalid != 2 || len(saved.Exploration.Handoffs) != 2 || len(saved.Exploration.ModificationEvidence) != 2 || saved.Exploration.ModificationEvidence[1].Handoff != 2 || !strings.Contains(saved.Exploration.Rejection, "extension limit exhausted") || len(durable.ToolExecutions) != 4 {
		t.Fatalf("progress=%s tools=%d", durable.ExecutionProgress, len(durable.ToolExecutions))
	}
}

func TestReactRepeatedFollowupHandoffCannotRefreshWindow(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{ExecutionID: "repeat-followup", Goal: "fix main.go", CompletionCriteria: []string{"changed"}}
	err := runModificationScript(t, workspace, durable, config.DefaultExplorationBudget(), [][]any{
		modificationHandoff(),
		modificationTool("first-patch", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-old\n+new\n"}),
		modificationHandoff(),
		modificationTool("locate-followup", "read_file", map[string]string{"path": "main.go"}),
		modificationHandoff(),
		modificationHandoff(),
	})
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		Exploration struct {
			Used, ModificationLeft int
			Handoffs               []any
			ModificationConfirmed  bool
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 || saved.Exploration.ModificationLeft != 0 || len(saved.Exploration.Handoffs) != 2 || saved.Exploration.ModificationConfirmed || len(durable.ToolExecutions) != 2 {
		t.Fatalf("progress=%s tools=%d", durable.ExecutionProgress, len(durable.ToolExecutions))
	}
}
