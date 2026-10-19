package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestWorkerSendsNativeToolResults(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer process.mcpClientSess.Close()
			defer process.serverSess.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Tools []struct{ Name string } `json:"tools"`
					Input []struct {
						Type, Role, Name, Arguments, Output, Content string
						CallID                                       string `json:"call_id"`
					} `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(request.Input) != 5 {
					t.Errorf("expected two call/output pairs and current context: %+v", request.Input)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				for i, want := range []struct{ id, name, args, output string }{
					{"call_read", "read_file", `{"path":"notes.txt"}`, "file content"},
					{"call_error", "grep", `{}`, "permission denied"},
				} {
					call, output := request.Input[2*i+1], request.Input[2*i+2]
					if call.Type != "function_call" || call.CallID != want.id || call.Name != want.name || call.Arguments != want.args {
						t.Errorf("unexpected function call: %+v", call)
					}
					if output.Type != "function_call_output" || output.CallID != want.id || !strings.Contains(output.Output, want.output) {
						t.Errorf("unexpected tool result: %+v", output)
					}
				}
				input := request.Input[0]
				if input.Role != "user" || !strings.Contains(input.Content, "inspect notes") || !strings.HasSuffix(input.Content, "> notes verified") {
					t.Errorf("expected ReAct context with trailing completion criteria: %+v", input)
				}
				// 验证实际请求末尾的顺序，防止输出协议插入目标、计划和验收标准之间。
				goalIndex := strings.Index(input.Content, "# Goal\n\n")
				if goalIndex < 0 {
					t.Error("missing trailing goal")
				} else {
					tail := input.Content[goalIndex:]
					wantPrefix := "# Goal\n\n> inspect notes\n\n# StepExecutions\n\n> "
					if !strings.HasPrefix(tail, wantPrefix) || !strings.Contains(tail, "\n\n# CurrentStepCompletionCriteria\n\n> notes verified") || strings.Contains(tail, "# Required result schema") {
						t.Errorf("unexpected trailing step context: %s", tail)
					}
				}
				for _, heading := range []string{"# Goal\n\n", "# StepExecutions\n\n", "# CurrentStepCompletionCriteria\n\n"} {
					if strings.Count(input.Content, heading) != 1 {
						t.Errorf("expected one %q section", heading)
					}
				}
				if len(request.Tools) != 2 {
					t.Errorf("expected only allowed MCP tools, got %+v", request.Tools)
				} else {
					for _, definition := range request.Tools {
						if definition.Name != "read_file" && definition.Name != "grep" {
							t.Errorf("unexpected MCP definition: %q", definition.Name)
						}
					}
				}
				for _, raw := range []string{"# MCP tools", "# ToolResult list", "file content", "permission denied"} {
					if strings.Contains(input.Content, raw) {
						t.Errorf("raw tool data duplicated in user text: %q", raw)
					}
				}
				text := `{"status":"completed","answer":"done"}`
				if schema == config.LineProtocolLLMOutputSchema {
					text = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
				}
				writeObservationResponse(w, []any{observationMessage(text)})
			}))
			defer server.Close()
			rc := fileContext.NewReactContext(&fileContext.DurableState{
				RunId: "tool-task", Goal: "inspect notes",
				StepExecutions: []fileContext.StepExecution{{Status: fileContext.RunningExecutionStatus, CompletionCriteria: "notes verified"}},
			})
			rc.AppendToolResult(&fileContext.ToolResult{ToolCallId: "call_read", ToolName: "read_file", Arguments: `{"path":"notes.txt"}`, Result: "file content"})
			rc.AppendToolResult(&fileContext.ToolResult{ToolCallId: "call_error", ToolName: "grep", Error: "permission denied"})
			worker := NewWorker[ReactResult](context.Background(), &config.AgentConfig{AllowTools: map[config.McpToolName]struct{}{
				config.ReadFileMCPToolName: {}, config.GrepMCPToolName: {},
			}, LLMConfig: &config.LLMConfig{
				BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema,
			}}, process.mcpClientSess)
			got, _, calls, err := worker.Run(rc)
			if err != nil || got.Answer != "done" || len(calls) != 0 {
				t.Fatalf("Run() = %+v, %+v, %v", got, calls, err)
			}
			if strings.Contains(rc.Format(), "file content") || strings.Contains(rc.Format(), "permission denied") || strings.Contains(rc.Format(), "# ToolResult list") {
				t.Fatal("ReAct context contains raw tool results or errors")
			}
			if len(rc.ToolResults) != 2 || rc.ToolResults[0].Result != "file content" || rc.ToolResults[1].Error != "permission denied" {
				t.Fatal("formatting model input changed retained tool results")
			}
		})
	}
}
