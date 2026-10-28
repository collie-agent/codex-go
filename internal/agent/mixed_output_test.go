package agent_test

import (
	"encoding/json"
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

func TestReactRejectsFinalResultWithToolCalls(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, "notes.txt")
			if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
				t.Fatal(err)
			}
			final := `{"status":"completed","answer":"done"}`
			if schema == config.LineProtocolLLMOutputSchema {
				final = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct {
					Input []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
						Type    string `json:"type"`
					} `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if calls == 1 {
					args, _ := json.Marshal(map[string]string{"patch": "*** Begin Patch\n*** Update File: notes.txt\n@@\n-original\n+changed\n*** End Patch"})
					criteriaResponse(w, final, map[string]any{"type": "function_call", "id": "fc_patch", "call_id": "patch", "name": "apply_patch", "arguments": string(args)})
					return
				}
				if len(request.Input) != 3 {
					t.Errorf("expected system, state, and separate correction; got %d messages", len(request.Input))
				}
				for index, item := range request.Input {
					if item.Type == "function_call" || item.Type == "function_call_output" {
						t.Error("rejected tool batch was replayed")
					}
					if index < 2 && strings.Contains(item.Content, "both a final result and tool calls") {
						t.Error("correction mixed into system/state")
					}
				}
				if len(request.Input) > 0 {
					last := request.Input[len(request.Input)-1]
					if last.Role != "user" || !strings.Contains(last.Content, "both a final result and tool calls") || !strings.Contains(last.Content, "not executed") {
						t.Errorf("missing independent conflict correction: %+v", last)
					}
				}
				criteriaResponse(w, final)
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			checkpoints := 0
			process.SetCheckpoint(func() error { checkpoints++; return nil })
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema}}
			got, _, err := process.Run(cfg, &state.DurableState{RunId: "mixed", Goal: "answer", CompletionCriteria: []string{"answer provided"}})
			if err != nil || got.Answer != "done" || calls != 2 {
				t.Fatalf("result=%+v err=%v requests=%d", got, err, calls)
			}
			contents, err := os.ReadFile(path)
			if err != nil || string(contents) != "original\n" || checkpoints != 0 {
				t.Fatalf("rejected batch changed workspace or passed execution checkpoint: contents=%q err=%v checkpoints=%d", contents, err, checkpoints)
			}
		})
	}
}

func TestReactMixedOutputSharesFormatRetryLimit(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			calls := 0
			final := `{"status":"completed","answer":"done"}`
			if schema == config.LineProtocolLLMOutputSchema {
				final = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					criteriaResponse(w, "invalid result")
					return
				}
				criteriaResponse(w, final, map[string]any{"type": "function_call", "id": "fc_criteria", "call_id": "criteria", "name": "update_completion_criteria", "arguments": `{"completion_criteria":["must not be applied"]}`})
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			checkpoints := 0
			process.SetCheckpoint(func() error { checkpoints++; return nil })
			durable := &state.DurableState{RunId: "mixed-limit", Goal: "answer"}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema}}
			_, _, err = process.Run(cfg, durable)
			if err == nil || !strings.Contains(err.Error(), "output format errors exceeded limit (3)") || !strings.Contains(err.Error(), "both a final result and tool calls") || calls != 4 {
				t.Fatalf("expected shared format retry limit: requests=%d err=%v", calls, err)
			}
			if checkpoints != 0 || len(durable.CompletionCriteria) != 0 {
				t.Fatalf("rejected control tool executed: checkpoints=%d criteria=%v", checkpoints, durable.CompletionCriteria)
			}
		})
	}
}

func TestReactAllowsExplanatoryTextWithToolCalls(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					criteriaResponse(w, "I will first define the completion criteria.", map[string]any{"type": "function_call", "id": "fc_criteria", "call_id": "criteria", "name": "update_completion_criteria", "arguments": `{"completion_criteria":["answer provided"]}`})
					return
				}
				final := `{"status":"completed","answer":"done"}`
				if schema == config.LineProtocolLLMOutputSchema {
					final = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
				}
				criteriaResponse(w, final)
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			durable := &state.DurableState{RunId: "explanatory-text", Goal: "answer"}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema}}
			got, _, err := process.Run(cfg, durable)
			if err != nil || got.Answer != "done" || calls != 2 || len(durable.CompletionCriteria) != 1 || durable.CompletionCriteria[0] != "answer provided" {
				t.Fatalf("explanatory text prevented tool execution: result=%+v err=%v requests=%d criteria=%v", got, err, calls, durable.CompletionCriteria)
			}
		})
	}
}
