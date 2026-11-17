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

func TestReactFormatRepairDisablesAllToolsAndPreservesEvidence(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("verified evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			turn := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turn++
				var request struct {
					Tools []struct{ Name string }
					Input json.RawMessage
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if turn == 3 || turn == 4 {
					if len(request.Tools) != 0 {
						t.Errorf("format repair exposed tools: %+v", request.Tools)
					}
				} else {
					names := map[string]bool{}
					for _, tool := range request.Tools {
						names[tool.Name] = true
					}
					if !names["read_file"] || !names["read_tool_result"] || !names["load_skill"] {
						t.Errorf("normal request missing remote or built-in tools: %+v", request.Tools)
					}
				}
				if turn >= 2 && turn <= 4 && !strings.Contains(string(request.Input), "verified evidence") {
					t.Error("existing tool evidence missing")
				}
				switch turn {
				case 1:
					criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_read", "call_id": "read", "name": "read_file", "arguments": `{"path":"notes.txt"}`})
				case 2, 3:
					criteriaResponse(w, "invalid result")
				default:
					final := `{"status":"completed","answer":"done"}`
					if schema == config.LineProtocolLLMOutputSchema {
						final = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
					}
					criteriaResponse(w, final)
				}
			}))
			defer server.Close()
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema}}
			for _, runID := range []string{"repair", "normal"} {
				process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
				if err != nil {
					t.Fatal(err)
				}
				got, _, err := process.Run(cfg, &state.DurableState{RunId: runID, Goal: "answer", CompletionCriteria: []string{"answer provided"}})
				if err != nil || got.Answer != "done" {
					t.Fatalf("run %s: result=%+v err=%v", runID, got, err)
				}
			}
			if turn != 5 {
				t.Fatalf("requests=%d, want 5", turn)
			}
		})
	}
}

func TestReactToolCallCorrectionKeepsToolsAvailable(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("corrected call evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		var request struct {
			Tools []struct{ Name string }
			Input json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		names := map[string]bool{}
		for _, tool := range request.Tools {
			names[tool.Name] = true
		}
		if !names["read_file"] || !names["read_tool_result"] || !names["load_skill"] {
			t.Errorf("tool call correction removed tools: %+v", request.Tools)
		}
		switch turn {
		case 1:
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_bad", "call_id": "bad", "name": "unknown_tool", "arguments": `{}`})
		case 2:
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_read", "call_id": "read", "name": "read_file", "arguments": `{"path":"notes.txt"}`})
		default:
			if !strings.Contains(string(request.Input), "corrected call evidence") {
				t.Error("corrected tool call was not executed")
			}
			criteriaResponse(w, `{"status":"completed","answer":"done"}`)
		}
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	got, _, err := process.Run(cfg, &state.DurableState{RunId: "tool-correction", Goal: "answer", CompletionCriteria: []string{"answer provided"}})
	if err != nil || got.Answer != "done" || turn != 3 {
		t.Fatalf("result=%+v err=%v requests=%d", got, err, turn)
	}
}
