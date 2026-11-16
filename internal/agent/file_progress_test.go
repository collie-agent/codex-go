package agent_test

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

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactFileStallRecoveryCanRepairAndVerify(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turn, stage := 0, 0
	recovered := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		turn++
		name, args := "read_file", fmt.Sprintf(`{"path":"value.txt","limit":%d}`, 20+turn)
		switch stage {
		case 0:
			if strings.Contains(string(body), "File investigation recovery") {
				for _, want := range []string{"Repair value", "value is fixed and verified", "value.txt", "call_1", "remaining", "no new", "modify immediately and verify", "do not reread", "tool results have since resolved", "specific gap and its completion criterion"} {
					if !strings.Contains(string(body), want) {
						t.Errorf("recovery missing %q: %s", want, body)
					}
				}
				recovered = true
				name, args = "apply_patch", `{"patch":"--- value.txt\n+++ value.txt\n@@ -1 +1 @@\n-broken\n+fixed\n"}`
				stage = 1
			} else if turn > 10 {
				t.Error("no recovery feedback before bounded test deadline")
				criteriaResponse(w, `{"status":"failed","answer":"missing recovery"}`)
				return
			}
		case 1:
			name, args = "exec", `{"command":"grep","args":["-x","fixed","value.txt"]}`
			stage = 2
		case 2:
			// Verify through the model's native tool response, not through the filesystem.
			var req struct {
				Input []struct {
					Type, Output string
					CallID       string `json:"call_id"`
				}
			}
			_ = json.Unmarshal(body, &req)
			verified := false
			for _, item := range req.Input {
				if item.Type == "function_call_output" && item.CallID == fmt.Sprintf("call_%d", turn-1) && strings.Contains(item.Output, "fixed") {
					verified = true
				}
			}
			if !verified {
				t.Error("verification result did not reach model")
			}
			criteriaResponse(w, `{"status":"completed","answer":"fixed and verified","observation":["patched value"],"CompletionEvidence":["grep matched fixed"]}`)
			return
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turn), "call_id": fmt.Sprintf("call_%d", turn), "name": name, "arguments": args})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "file-recovery", Goal: "Repair value", ExecutionID: "repair", StepExecutions: []state.StepExecution{{ExecutionID: "repair", Status: state.RunningExecutionStatus, Intent: "Repair value", CompletionCriteria: "value is fixed and verified"}}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ApplyPatchMCPToolName: {}, config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err != nil || result.Status != agent.CompletedReactStatus || !recovered {
		t.Fatalf("result=%+v recovered=%v error=%v", result, recovered, err)
	}
}

func TestReactFileRecoveryFailureIsBounded(t *testing.T) {
	for _, mode := range []string{"exact", "vary_limit", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("value\n"), 0600); err != nil {
				t.Fatal(err)
			}
			turns, recoveries, summaries := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "Execution budget closing:") {
					summaries++
					criteriaResponse(w, `{"status":"failed","answer":"Investigation remains unfinished"}`)
					return
				}
				turns++
				if strings.Contains(string(body), "File investigation recovery") {
					recoveries++
				}
				if turns > 15 {
					t.Error("stagnation did not fail within bounded recovery")
					criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
					return
				}
				limit := 20
				if mode != "exact" {
					limit += turns
				}
				calls := []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": "read_file", "arguments": fmt.Sprintf(`{"path":"value.txt","limit":%d}`, limit)}}
				if mode == "mixed" {
					calls = append(calls, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_cached_%d", turns), "call_id": fmt.Sprintf("cached_%d", turns), "name": "read_file", "arguments": `{"path":"value.txt","limit":20}`})
				}
				criteriaResponse(w, "", calls...)
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2, MaxRecoveries: 1})
			durable := &state.DurableState{RunId: "stalled", Goal: "inspect", CompletionCriteria: []string{"inspect value"}}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
			result, _, err := process.Run(cfg, durable)
			if err == nil || !strings.Contains(err.Error(), "file investigation stalled") || result.Status == agent.CompletedReactStatus {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if turns != 5 || recoveries != 2 || summaries != 1 {
				t.Fatalf("turns=%d recovery decisions=%d summaries=%d, want 5, 2 and 1", turns, recoveries, summaries)
			}
		})
	}
}

func TestReactReadOnlyInvestigationContinuesWithNewRanges(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("one\ntwo\nthree\nfour\nfive\nsix\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "File investigation recovery") {
			t.Error("new file ranges incorrectly triggered recovery")
		}
		turn++
		if turn == 7 {
			criteriaResponse(w, `{"status":"completed","answer":"six lines investigated"}`)
			return
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turn), "call_id": fmt.Sprintf("call_%d", turn), "name": "read_file", "arguments": fmt.Sprintf(`{"path":"value.txt","offset":%d,"limit":1}`, turn)})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 1})
	durable := &state.DurableState{RunId: "read-only", Goal: "investigate", CompletionCriteria: []string{"investigate all six lines"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err != nil || result.Status != agent.CompletedReactStatus {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestReactFileChangeAllowsSameReadAgain(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		if strings.Contains(string(body), "File investigation recovery") {
			t.Error("changed file incorrectly triggered recovery")
		}
		turn++
		if turn == 4 {
			var req struct {
				Input []struct {
					Type, Output string
					CallID       string `json:"call_id"`
				}
			}
			_ = json.Unmarshal(body, &req)
			found := false
			for _, item := range req.Input {
				if item.Type == "function_call_output" && item.CallID == "call_3" && strings.Contains(item.Output, "new") {
					found = true
				}
			}
			if !found {
				t.Error("same read did not return changed file")
			}
			criteriaResponse(w, `{"status":"completed","answer":"new value verified"}`)
			return
		}
		name, args := "read_file", `{"path":"value.txt","limit":20}`
		if turn == 2 {
			name, args = "apply_patch", `{"patch":"--- value.txt\n+++ value.txt\n@@ -1 +1 @@\n-old\n+new\n"}`
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turn), "call_id": fmt.Sprintf("call_%d", turn), "name": name, "arguments": args})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 1})
	durable := &state.DurableState{RunId: "changed", Goal: "change", CompletionCriteria: []string{"new value verified"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	if result, _, err := process.Run(cfg, durable); err != nil || result.Status != agent.CompletedReactStatus {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestReactFileRecoveryRespectsRemainingTurnBudget(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		turns++
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": "read_file", "arguments": fmt.Sprintf(`{"path":"value.txt","limit":%d}`, 20+turns)})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 98, RecoveryWindow: 4})
	// Start with 100 decisions left so recovery reaches the actual budget boundary.
	durable := &state.DurableState{RunId: "budget", ExecutionID: "budget", Goal: "inspect", CompletionCriteria: []string{"inspect value"}, ExecutionProgress: json.RawMessage(`{"ExecutionID":"budget","UsedTurns":200}`)}
	cfg := &config.AgentConfig{ExplorationBudget: config.ExplorationBudget{InitialTurns: 1000, DecisionRetries: 1}, LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	_, _, err = process.Run(cfg, durable)
	if err == nil || !strings.Contains(err.Error(), "file investigation stalled") {
		t.Fatalf("expected bounded recovery failure, got %v", err)
	}
	if turns != 100 {
		t.Fatalf("recovery changed overall budget: turns=%d", turns)
	}
}

func TestReactFileRecoveryTotalRemainsBoundedAfterNewEvidence(t *testing.T) {
	workspace := t.TempDir()
	for _, path := range []string{"first.txt", "second.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte("value\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	turns, summaries := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "Execution budget closing:") {
			summaries++
			criteriaResponse(w, `{"status":"failed","answer":"Investigation remains unfinished"}`)
			return
		}
		turns++
		if turns == 4 && !strings.Contains(string(body), "File investigation recovery") {
			t.Error("first stall did not trigger recovery")
		}
		if turns > 6 {
			t.Error("new evidence refreshed total recovery allowance")
			criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
			return
		}
		path := "first.txt"
		if turns >= 4 {
			path = "second.txt"
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": "read_file", "arguments": fmt.Sprintf(`{"path":%q,"limit":%d}`, path, 20+turns)})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2, MaxRecoveries: 1})
	durable := &state.DurableState{RunId: "recovery-total", Goal: "inspect", CompletionCriteria: []string{"inspect both files"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	_, _, err = process.Run(cfg, durable)
	if err == nil || !strings.Contains(err.Error(), "recovery budget exhausted") || turns != 6 || summaries != 1 {
		t.Fatalf("turns=%d summaries=%d error=%v", turns, summaries, err)
	}
}
