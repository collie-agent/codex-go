package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

func TestReactExhaustedExplorationChecksWorkspaceThenPatchesAndVerifies(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = workspace
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}),
		modificationHandoff(),
		modificationTool("status", "exec", map[string]any{"command": "git", "args": []string{"status", "--short"}}),
		modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
		modificationTool("test", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"changed and verified"}`)},
	}, policy)
	if err != nil {
		t.Fatalf("workspace check must allow implementation: %v; %s", err, durable.ExecutionProgress)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	var saved struct {
		Exploration struct {
			Used, ModificationLeft int
			ModificationConfirmed  bool
			Accounting             []struct{ Purpose string }
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 || !saved.Exploration.ModificationConfirmed || len(saved.Exploration.Accounting) != 4 || saved.Exploration.Accounting[1].Purpose != "workspace_check" {
		t.Fatalf("accounting: %s", durable.ExecutionProgress)
	}
	if len(durable.ToolExecutions) != 4 {
		t.Fatalf("tools=%+v", durable.ToolExecutions)
	}
	for _, call := range durable.ToolExecutions {
		if call.Status != "success" {
			t.Fatalf("tool failed: %+v", call)
		}
		if call.ToolName == "exec" {
			var result struct {
				ExitCode *int `json:"exit_code"`
			}
			if !commandResultPayload(call, &result) || result.ExitCode == nil || *result.ExitCode != 0 {
				t.Fatalf("command did not succeed: %+v", call)
			}
		}
	}
}

func TestReactWorkspaceChecksExhaustWindowWithoutConfirmingModification(t *testing.T) {
	for _, gitWorkspace := range []bool{true, false} {
		t.Run(map[bool]string{true: "dirty-git", false: "non-git"}[gitWorkspace], func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package dirty\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if gitWorkspace {
				cmd := exec.Command("git", "init", "--quiet")
				cmd.Dir = workspace
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v %s", err, output)
				}
			}
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = 1
			durable, err := runCommandExploration(t, workspace, [][]any{
				modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(),
				modificationTool("status1", "exec", map[string]any{"command": "git", "args": []string{"status", "-s"}}),
				modificationTool("status2", "exec", map[string]any{"command": "git", "args": []string{"status", "--porcelain"}}),
				modificationTool("status3", "exec", map[string]any{"command": "git", "args": []string{"status", "--short", "--branch"}}),
				{observationMessage(`{"status":"completed","answer":"claimed modified"}`)},
			}, policy)
			if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
				t.Fatalf("err=%v state=%s", err, durable.ExecutionProgress)
			}
			var saved struct {
				Exploration struct {
					Used, ModificationLeft int
					ModificationConfirmed  bool
				}
			}
			if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Exploration.Used != 1 || saved.Exploration.ModificationLeft != 0 || saved.Exploration.ModificationConfirmed || len(durable.ToolExecutions) != 4 {
				t.Fatalf("state=%s tools=%+v", durable.ExecutionProgress, durable.ToolExecutions)
			}
			for _, call := range durable.ToolExecutions[1:] {
				var result struct {
					ExitCode *int `json:"exit_code"`
				}
				if !commandResultPayload(call, &result) || result.ExitCode == nil || (gitWorkspace && *result.ExitCode != 0) || (!gitWorkspace && *result.ExitCode == 0) {
					t.Fatalf("unexpected status result: %+v", call)
				}
			}
			content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
			if err != nil || string(content) != "package dirty\n" {
				t.Fatalf("content=%q err=%v", content, err)
			}
		})
	}
}

func TestReactWorkspaceMixedBatchPreservesInvestigationPriority(t *testing.T) {
	for _, allowance := range []int{1, 2} {
		t.Run(fmt.Sprint(allowance), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("git", "init", "--quiet")
			cmd.Dir = workspace
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, output)
			}
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = allowance
			batch := modificationTool("status", "exec", map[string]any{"command": "git", "args": []string{"status", "--short"}})
			batch = append(batch, modificationTool("source", "exec", map[string]any{"command": "git", "args": []string{"diff", "--", "main.go"}})...)
			batch = append(batch, modificationTool("search", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}})...)
			batch = append(batch, modificationTool("edit", "exec", map[string]any{"command": "sed", "args": []string{"-i", "s/package old/package main/", "main.go"}})...)
			batch = append(batch, modificationTool("verify", "exec", map[string]any{"command": "git", "args": []string{"status", "--porcelain"}})...)
			if allowance == 1 {
				batch = append(batch, modificationTool("test-in-rejected-batch", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}})...)
			}
			durable, err := runCommandExploration(t, workspace, [][]any{
				modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(), batch,
				modificationTool("test", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
				{observationMessage(`{"status":"completed","answer":"changed and verified"}`)},
			}, policy)
			var saved struct {
				Exploration struct {
					Used                  int
					ModificationConfirmed bool
					Accounting            []struct {
						Purpose string
						Calls   map[string]string
					}
				}
			}
			if decodeErr := json.Unmarshal(durable.ExecutionProgress, &saved); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			content, readErr := os.ReadFile(filepath.Join(workspace, "main.go"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if allowance == 1 {
				if err == nil || !strings.Contains(err.Error(), "implementation handoff has no confirmed target modification") || len(durable.ToolExecutions) != 2 || string(content) != "package old\n" || saved.Exploration.ModificationConfirmed {
					t.Fatalf("batch escaped: err=%v content=%q tools=%+v", err, content, durable.ToolExecutions)
				}
			} else {
				if err != nil || string(content) != "package main\n" || !saved.Exploration.ModificationConfirmed || len(durable.ToolExecutions) != 7 {
					t.Fatalf("batch failed: err=%v content=%q tools=%+v", err, content, durable.ToolExecutions)
				}
				if len(saved.Exploration.Accounting) != 3 || saved.Exploration.Accounting[1].Purpose != "investigation" || len(saved.Exploration.Accounting[1].Calls) != 5 {
					t.Fatalf("batch accounting=%s", durable.ExecutionProgress)
				}
			}
			if saved.Exploration.Used != allowance {
				t.Fatalf("budget=%s", durable.ExecutionProgress)
			}
		})
	}
}

func TestReactWorkspaceChecksChargeWindowOncePerBusinessTurn(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = workspace
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	batch := append(modificationTool("status1", "exec", map[string]any{"command": "git", "args": []string{"status", "-s"}}), modificationTool("status2", "exec", map[string]any{"command": "git", "args": []string{"status", "--porcelain"}})...)
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(), batch,
		modificationTool("status3", "exec", map[string]any{"command": "git", "args": []string{"status", "--short", "--branch"}}),
		modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
		{observationMessage(`{"status":"completed","answer":"changed"}`)},
	}, policy)
	if err != nil {
		t.Fatalf("checks consumed more than one window turn per batch: %v; %s", err, durable.ExecutionProgress)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("content=%q err=%v", content, err)
	}
}

func TestReactWorkspaceCheckExemptionRejectsUnknownCommandsAndDiffs(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		args          []string
	}{
		{"unknown-option", "git", []string{"status", "--unknown"}},
		{"compound", "sh", []string{"-c", "git status; true"}},
		{"script", "python3", []string{"-c", "print('verification')"}},
		{"repository-diff", "git", []string{"diff"}},
		{"historical-diff", "git", []string{"diff", "HEAD~1"}},
		{"unconfirmed-target-diff", "git", []string{"diff", "--", "main.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = 1
			durable, err := runCommandExploration(t, workspace, [][]any{
				modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(),
				modificationTool("claimed-verification", "exec", map[string]any{"command": tc.command, "args": tc.args}),
				{observationMessage(`{"status":"completed","answer":"claimed changed"}`)},
			}, policy)
			if err == nil || !strings.Contains(err.Error(), "implementation handoff has no confirmed target modification") || len(durable.ToolExecutions) != 1 {
				t.Fatalf("unsupported command escaped: err=%v tools=%+v", err, durable.ToolExecutions)
			}
		})
	}
}

func TestReactExhaustedExplorationOnlyVerifiesConfirmedTargetDiff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		allowed bool
	}{
		{"exact-target", []string{"diff", "--", "main.go"}, true},
		{"whole-workspace", []string{"diff"}, false},
		{"history", []string{"diff", "HEAD~1"}, false},
		{"unknown-target", []string{"diff", "--", "other.go"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"init", "--quiet"}, {"add", "main.go"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = workspace
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, output)
				}
			}
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = 1
			terminal := []any{observationMessage(`{"status":"completed","answer":"verified change"}`)}
			if !tc.allowed {
				terminal = []any{observationMessage(`{"exploration":{"choice":"unable","reason":"no investigation allowance remains","execution_state":"main.go changed; diff batch not executed"}}`)}
			}
			durable, err := runCommandExploration(t, workspace, [][]any{
				modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(),
				modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
				modificationTool("diff", "exec", map[string]any{"command": "git", "args": tc.args}), terminal,
			}, policy)
			wantTools := 2
			if tc.allowed {
				wantTools = 3
			}
			if (err == nil) != tc.allowed || len(durable.ToolExecutions) != wantTools {
				t.Fatalf("allowed=%v err=%v tools=%+v", tc.allowed, err, durable.ToolExecutions)
			}
			if tc.allowed {
				var result struct {
					ExitCode *int `json:"exit_code"`
				}
				if !commandResultPayload(durable.ToolExecutions[2], &result) || result.ExitCode == nil || *result.ExitCode != 0 {
					t.Fatalf("diff failed: %+v", durable.ToolExecutions[2])
				}
			}
		})
	}
}
