package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactExplorationExtensionGrantsBoundedInvestigation(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1, 3:
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": string(rune('a' + requests)), "name": "list_dir", "arguments": `{"path":".","limit":` + string(rune('0'+requests)) + `}`}})
		case 2:
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"extend","gap":"missing target file","blocking_reason":"cannot patch unknown path","investigation":"list directory","exit_condition":"identify target"}}`), map[string]any{"type": "function_call", "id": "mixed", "call_id": "mixed", "name": "list_dir", "arguments": `{"path":"."}`}})
		case 4:
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"choice":"unable","reason":"target absent","execution_state":"two directory observations; no changes"}}`)})
		default:
			writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"target absent; no changes"}`)})
		}
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{Goal: "fix source", CompletionCriteria: []string{"fixed"}}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	policy.ExtensionTurns = 1
	_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil {
		t.Fatal("expected explicit unable result")
	}
	var saved struct {
		Exploration struct{ Used, Extensions int }
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if requests != 5 || len(durable.ToolExecutions) != 2 || saved.Exploration.Used != 2 || saved.Exploration.Extensions != 1 {
		t.Fatalf("requests=%d tools=%d saved=%s", requests, len(durable.ToolExecutions), durable.ExecutionProgress)
	}
}

func TestReactExplorationExtensionRefusalsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		maxExtensions, total int
		missing              string
		wantReason           string
	}{
		{"zero extensions", 0, 20, "", "extension limit exhausted"},
		{"missing gap", 2, 20, "gap", "extend requires"},
		{"missing blocking_reason", 2, 20, "blocking_reason", "extend requires"},
		{"missing investigation", 2, 20, "investigation", "extend requires"},
		{"missing exit_condition", 2, 20, "exit_condition", "extend requires"},
		{"reserved closing budget", 2, 4, "", "insufficient total decision budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "first", "call_id": "first", "name": "list_dir", "arguments": `{"path":"."}`}})
					return
				}
				decision := `{"exploration":{"task_type":"modification","basis":"requested fix","choice":"extend","gap":"missing target","blocking_reason":"cannot patch unknown path","investigation":"list source","exit_condition":"target identified"}}`
				if tc.missing != "" {
					var document map[string]map[string]string
					if err := json.Unmarshal([]byte(decision), &document); err != nil {
						t.Error(err)
						return
					}
					delete(document["exploration"], tc.missing)
					encoded, err := json.Marshal(document)
					if err != nil {
						t.Error(err)
						return
					}
					decision = string(encoded)
				}
				writeObservationResponse(w, []any{observationMessage(decision)})
			}))
			defer server.Close()
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			process.maxTurn = tc.total
			policy := config.DefaultExplorationBudget()
			policy.InitialTurns = 1
			policy.MaxExtensions = tc.maxExtensions
			durable := &state.DurableState{Goal: "fix source", CompletionCriteria: []string{"fixed"}}
			_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
			if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("err=%v", err)
			}
			var saved struct {
				Exploration struct {
					Used, Extensions, Invalid int
					Rejection                 string
				}
			}
			if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
				t.Fatal(err)
			}
			if requests != 4 || len(durable.ToolExecutions) != 1 || saved.Exploration.Used != 1 || saved.Exploration.Extensions != 0 || saved.Exploration.Invalid != 2 {
				t.Fatalf("requests=%d tools=%d progress=%s", requests, len(durable.ToolExecutions), durable.ExecutionProgress)
			}
			if tc.missing == "" && !strings.Contains(saved.Exploration.Rejection, tc.wantReason) {
				t.Fatalf("missing saved rejection: %s", durable.ExecutionProgress)
			}
		})
	}
}

func TestReactExplorationExtensionLimitDoesNotRenewAfterNewEvidence(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests%2 == 1 && requests <= 5 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": fmt.Sprint(requests), "call_id": fmt.Sprint(requests), "name": "list_dir", "arguments": fmt.Sprintf(`{"path":".","limit":%d}`, requests)}})
			return
		}
		writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"extend","gap":"missing target","blocking_reason":"cannot patch unknown file","investigation":"list another directory range","exit_condition":"target found"}}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	policy.ExtensionTurns = 1
	durable := &state.DurableState{Goal: "fix source", CompletionCriteria: []string{"fixed"}}
	_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil || !strings.Contains(err.Error(), "extension limit exhausted") {
		t.Fatalf("err=%v", err)
	}
	var saved struct {
		Exploration struct{ Used, Extensions int }
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if requests != 8 || len(durable.ToolExecutions) != 3 || saved.Exploration.Extensions != 2 || saved.Exploration.Used != 3 {
		t.Fatalf("requests=%d tools=%d progress=%s", requests, len(durable.ToolExecutions), durable.ExecutionProgress)
	}
}

func TestReactExplorationExtensionCheckpointFailureBlocksInvestigation(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"extend","gap":"missing target","blocking_reason":"cannot patch unknown file","investigation":"list source","exit_condition":"target found"}}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{Goal: "fix source", CompletionCriteria: []string{"fixed"}}
	process.SetCheckpoint(func() error {
		if len(durable.ExecutionProgress) == 0 {
			return nil
		}
		var saved struct{ Exploration struct{ Extensions int } }
		if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
			return err
		}
		if saved.Exploration.Extensions > 0 {
			return fmt.Errorf("extension storage unavailable")
		}
		return nil
	})
	_, _, err = process.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil || !strings.Contains(err.Error(), "extension storage unavailable") || requests != 1 || len(durable.ToolExecutions) != 0 {
		t.Fatalf("err=%v requests=%d tools=%d", err, requests, len(durable.ToolExecutions))
	}
	var saved struct{ Exploration struct{ Extensions int } }
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Extensions != 0 {
		t.Fatalf("failed checkpoint persisted grant: %s", durable.ExecutionProgress)
	}
}

func TestReactExplorationExtensionResumeRetainsGrantAndPolicy(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"extend","gap":"missing target","blocking_reason":"cannot patch unknown file","investigation":"list source","exit_condition":"target found"}}`)})
		case 2:
			http.Error(w, "interrupted", http.StatusBadRequest)
		case 3:
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "resumed", "call_id": "resumed", "name": "list_dir", "arguments": `{"path":"."}`}})
		case 4:
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"choice":"unable","reason":"source absent","execution_state":"directory inspected; no modifications"}}`)})
		default:
			writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"source absent"}`)})
		}
	}))
	defer server.Close()
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	policy.ExtensionTurns = 1
	policy.MaxExtensions = 1
	cfg := &config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	durable := &state.DurableState{Goal: "fix source", CompletionCriteria: []string{"fixed"}}
	workspace := t.TempDir()
	for attempt := 0; attempt < 2; attempt++ {
		process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = process.Run(cfg, durable)
		if err == nil {
			t.Fatal("expected incomplete execution")
		}
		encoded, err := json.Marshal(durable)
		if err != nil {
			t.Fatal(err)
		}
		durable = &state.DurableState{}
		if err := json.Unmarshal(encoded, durable); err != nil {
			t.Fatal(err)
		}
		cfg.ExplorationBudget = config.DefaultExplorationBudget()
		cfg.ExplorationBudget.InitialTurns = 100
	}
	var saved struct {
		Exploration struct {
			Used, Extensions int
			Policy           config.ExplorationBudget
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if requests != 5 || len(durable.ToolExecutions) != 1 || saved.Exploration.Extensions != 1 || saved.Exploration.Used != 2 || saved.Exploration.Policy.InitialTurns != 1 || saved.Exploration.Policy.MaxExtensions != 1 {
		t.Fatalf("requests=%d tools=%d saved=%s", requests, len(durable.ToolExecutions), durable.ExecutionProgress)
	}
}
