package agent

import (
	"encoding/json"
	"fmt"
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

func TestReactRejectedInvestigationCanPatchVerifyAndComplete(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(),
		modificationTool("denied", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}}),
		modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
		modificationTool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"changed and verified"}`)},
	}, policy)
	if err != nil {
		t.Fatalf("correction failed: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("content=%q err=%v", content, err)
	}
	var saved struct {
		Exploration struct {
			Used                  int
			ModificationConfirmed bool
			Accounting            []json.RawMessage
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 1 || !saved.Exploration.ModificationConfirmed || len(durable.ToolExecutions) != 3 || len(saved.Exploration.Accounting) != 3 {
		t.Fatalf("state=%s tools=%+v", durable.ExecutionProgress, durable.ToolExecutions)
	}
	for _, call := range durable.ToolExecutions {
		if call.ToolCallId == "denied" {
			t.Fatal("rejected tool was registered")
		}
	}
}

func TestReactRejectedInvestigationExhaustsWindowWithoutExecutingMixedBatch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	outputs := [][]any{modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff()}
	for i := 0; i < 3; i++ {
		batch := modificationTool(fmt.Sprintf("search%d", i), "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}})
		batch = append(batch, modificationTool(fmt.Sprintf("patch%d", i), "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"})...)
		outputs = append(outputs, batch)
	}
	durable, err := runCommandExploration(t, workspace, outputs, policy)
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		Exploration struct {
			Used, ModificationLeft int
			ModificationConfirmed  bool
			Rejection              string
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || string(content) != "package old\n" || len(durable.ToolExecutions) != 1 || saved.Exploration.Used != 1 || saved.Exploration.ModificationLeft != 0 || saved.Exploration.ModificationConfirmed || !strings.Contains(saved.Exploration.Rejection, "modification_left=0") {
		t.Fatalf("content=%q err=%v state=%s tools=%+v", content, err, durable.ExecutionProgress, durable.ToolExecutions)
	}
}

func TestReactImplementationExtensionDoesNotRefreshWindow(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	policy.ExtensionTurns = 1
	policy.MaxExtensions = 1
	extend := []any{observationMessage(`{"exploration":{"choice":"extend","gap":"missing caller contract","blocking_reason":"need contract before patch","investigation":"read main.go","exit_condition":"contract known"}}`)}
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(),
		modificationTool("denied", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}}),
		extend,
		modificationTool("allowed", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}}),
		extend,
		modificationTool("still-denied", "exec", map[string]any{"command": "rg", "args": []string{"old", "main.go"}}),
	}, policy)
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v state=%s", err, durable.ExecutionProgress)
	}
	var saved struct {
		Exploration struct{ Used, Extensions, ModificationLeft int }
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 2 || saved.Exploration.Extensions != 1 || saved.Exploration.ModificationLeft != 0 || len(durable.ToolExecutions) != 2 {
		t.Fatalf("state=%s tools=%+v", durable.ExecutionProgress, durable.ToolExecutions)
	}
}

func TestReactImplementationRejectedDecisionsShareWindow(t *testing.T) {
	for _, decision := range []string{
		`{"exploration":{"choice":"extend","gap":"caller contract"}}`,
		`{"exploration":{"choice":"extend","gap":"caller contract","blocking_reason":"unsafe patch","investigation":"read caller","exit_condition":"contract known"}}`,
	} {
		t.Run(decision, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = 1
			policy.MaxExtensions = 0
			durable, err := runCommandExploration(t, workspace, [][]any{
				modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(),
				{observationMessage(decision)}, modificationHandoff(),
				modificationTool("denied", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}}),
			}, policy)
			if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
				t.Fatalf("err=%v state=%s", err, durable.ExecutionProgress)
			}
			var saved struct {
				Exploration struct {
					Invalid, Extensions, Used, ModificationLeft int
					Pending                                     bool
					Handoffs                                    []json.RawMessage
				}
			}
			if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Exploration.Invalid != 0 || saved.Exploration.Pending || saved.Exploration.Extensions != 0 || saved.Exploration.Used != 1 || saved.Exploration.ModificationLeft != 0 || len(saved.Exploration.Handoffs) != 1 || len(durable.ToolExecutions) != 1 {
				t.Fatalf("state=%s", durable.ExecutionProgress)
			}
		})
	}
}

func TestReactRejectionCheckpointPrecedesCorrection(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		t.Run(fmt.Sprint(failSave), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			requests := 0
			persisted := false
			outputs := [][]any{modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(), modificationTool("denied", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}}), modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}), modificationTool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}), {observationMessage(`{"status":"completed","answer":"fixed"}`)}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Input json.RawMessage }
				_ = json.NewDecoder(r.Body).Decode(&body)
				if strings.Contains(string(body.Input), "你是 ReAct agent 的工具结果观察器") {
					writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
					return
				}
				if requests == 3 && (!persisted || !strings.Contains(string(body.Input), "Operation rejected") || !strings.Contains(string(body.Input), "modification_left=2") || !strings.Contains(string(body.Input), "extend")) {
					t.Error("missing persisted rejection feedback before correction")
				}
				if requests >= len(outputs) {
					t.Error("unexpected model request")
					writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"unexpected request"}`)})
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
			durable := &state.DurableState{ExecutionID: "rejection-checkpoint", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}}
			process.SetCheckpoint(func() error {
				var saved struct {
					Exploration struct {
						Rejection        string
						ModificationLeft int
					}
				}
				if len(durable.ExecutionProgress) > 0 {
					if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
						return err
					}
				}
				if strings.Contains(saved.Exploration.Rejection, "Operation rejected") {
					if saved.Exploration.ModificationLeft != 2 {
						t.Errorf("remaining window=%d", saved.Exploration.ModificationLeft)
					}
					if failSave {
						return fmt.Errorf("rejection storage unavailable")
					}
					persisted = true
				}
				return nil
			})
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = 1
			_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ExecMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
			content, readErr := os.ReadFile(filepath.Join(workspace, "main.go"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if failSave {
				if err == nil || !strings.Contains(err.Error(), "rejection storage unavailable") || requests != 3 || len(durable.ToolExecutions) != 1 || string(content) != "package old\n" {
					t.Fatalf("err=%v requests=%d tools=%+v content=%q", err, requests, durable.ToolExecutions, content)
				}
			} else if err != nil || requests != 6 || string(content) != "package main\n" {
				t.Fatalf("err=%v requests=%d content=%q", err, requests, content)
			}
		})
	}
}

func TestReactRejectedInvestigationStillRefundsExecutedFailedPatch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	policy.ModificationWindowTurns = 2
	durable, err := runCommandExploration(t, workspace, [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(),
		modificationTool("denied", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}}),
		modificationTool("bad-patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package absent\n+package main\n"}),
		modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
		modificationTool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
		{observationMessage(`{"status":"completed","answer":"fixed"}`)},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || string(content) != "package main\n" || len(durable.ToolExecutions) != 4 || durable.ToolExecutions[1].Status != "failed" {
		t.Fatalf("content=%q err=%v tools=%+v", content, err, durable.ToolExecutions)
	}
}

func TestReactMixedPhaseDecisionDoesNotEndCorrectionOrGrantExtension(t *testing.T) {
	for _, decision := range []string{
		`{"exploration":{"choice":"unable","reason":"missing contract","execution_state":"source read"}}`,
		`{"exploration":{"choice":"extend","gap":"caller contract","blocking_reason":"unsafe patch","investigation":"read caller","exit_condition":"contract known"}}`,
	} {
		t.Run(decision, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = 1
			mixed := append(modificationTool("mixed", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}}), observationMessage(decision))
			durable, err := runCommandExploration(t, workspace, [][]any{
				modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(), mixed,
				modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
				modificationTool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}),
				{observationMessage(`{"status":"completed","answer":"fixed"}`)},
			}, policy)
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Exploration struct{ Extensions, Used int }
			}
			if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Exploration.Extensions != 0 || saved.Exploration.Used != 1 || len(durable.ToolExecutions) != 3 {
				t.Fatalf("state=%s tools=%+v", durable.ExecutionProgress, durable.ToolExecutions)
			}
		})
	}
}

func TestReactImplementationExtensionRespectsRemainingTotalBudget(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	outputs := [][]any{modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(), {observationMessage(`{"exploration":{"choice":"extend","gap":"caller contract","blocking_reason":"unsafe patch","investigation":"read caller","exit_condition":"contract known"}}`)}, modificationTool("denied", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}})}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input json.RawMessage }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(string(body.Input), "你是 ReAct agent 的工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		if requests == 3 && !strings.Contains(string(body.Input), "insufficient total decision budget") {
			t.Error("missing total budget rejection")
		}
		if requests >= len(outputs) {
			writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"budget exhausted"}`)})
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
	process.maxTurn = 5
	durable := &state.DurableState{ExecutionID: "extension-total", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	policy.ExtensionTurns = 2
	_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil || !strings.Contains(err.Error(), "maximum decision budget exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		UsedTurns   int
		Exploration struct{ Extensions, Used, ModificationLeft int }
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.UsedTurns > 5 || saved.Exploration.Extensions != 0 || saved.Exploration.Used != 1 || saved.Exploration.ModificationLeft != 1 || len(durable.ToolExecutions) != 1 {
		t.Fatalf("state=%s tools=%+v", durable.ExecutionProgress, durable.ToolExecutions)
	}
}

func TestReactRejectedFollowupRetainsConfirmedProgress(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	outputs := [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(),
		modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}), modificationHandoff(),
	}
	for i := 0; i < 3; i++ {
		outputs = append(outputs, modificationTool(fmt.Sprintf("denied%d", i), "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}, "purpose": "verification"}))
	}
	durable, err := runCommandExploration(t, workspace, outputs, policy)
	if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		Exploration struct {
			Used, ModificationLeft int
			ModificationConfirmed  bool
			ModificationEvidence   []struct{ Path, Before, After string }
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "main.go"))
	if err != nil || string(content) != "package main\n" || len(durable.ToolExecutions) != 2 || saved.Exploration.Used != 1 || saved.Exploration.ModificationLeft != 0 || saved.Exploration.ModificationConfirmed || len(saved.Exploration.ModificationEvidence) != 1 {
		t.Fatalf("content=%q err=%v state=%s tools=%+v", content, err, durable.ExecutionProgress, durable.ToolExecutions)
	}
	evidence := saved.Exploration.ModificationEvidence[0]
	if evidence.Path != "main.go" || evidence.Before == "" || evidence.After == "" || evidence.Before == evidence.After {
		t.Fatalf("lost confirmed progress: %+v", evidence)
	}
}
