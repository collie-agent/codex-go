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
				if len(request.Input) != 6 {
					t.Errorf("expected system, two call/output pairs, and trailing user context: %+v", request.Input)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				for i, want := range []struct{ id, name, args, output string }{
					{"call_read", "read_file", `{"path":"notes.txt"}`, "file content"},
					{"call_error", "grep", `{}`, "worker-tool-error: access refused"},
				} {
					call, output := request.Input[2*i+1], request.Input[2*i+2]
					if call.Type != "function_call" || call.CallID != want.id || call.Name != want.name || call.Arguments != want.args {
						t.Errorf("unexpected function call: %+v", call)
					}
					if output.Type != "function_call_output" || output.CallID != want.id || !strings.Contains(output.Output, want.output) {
						t.Errorf("unexpected tool result: %+v", output)
					}
				}
				if request.Input[0].Role != "system" || !strings.Contains(request.Input[0].Content, "你是 ReAct agent 的单轮决策器。") {
					t.Errorf("missing ReAct system message: %+v", request.Input[0])
				}
				input := request.Input[len(request.Input)-1]
				formatMarker := "Output format: JSON."
				if schema == config.LineProtocolLLMOutputSchema {
					formatMarker = "Final text output format: Line Protocol."
				}
				if strings.Count(request.Input[0].Content, formatMarker) != 1 || !strings.Contains(request.Input[0].Content, "# Required result schema") {
					t.Error("expected one format contract and the context's result schema")
				}
				if strings.Contains(input.Content, "# sysPrompt") {
					t.Error("system prompt duplicated in user context")
				}
				if input.Role != "user" || !strings.Contains(input.Content, "inspect notes") || !strings.HasSuffix(input.Content, "> notes verified") {
					t.Errorf("expected ReAct context with trailing completion criteria: %+v", input)
				}
				// 工具结果必须先于本轮状态，格式约束只出现在 system 消息中。
				previous := -1
				for _, heading := range []string{"# DurableState\n\n", "# Observation\n\n", "# CompletionCriteria\n\n", "## CurrentStepCompletionCriteria\n\n"} {
					index := strings.Index(input.Content, heading)
					if index <= previous || strings.Count(input.Content, heading) != 1 {
						t.Errorf("missing, repeated or unordered section %q: %s", heading, input.Content)
					}
					previous = index
				}
				for _, marker := range []string{formatMarker, "# Required result schema", "# ReAct", "你是 ReAct agent"} {
					if strings.Contains(input.Content, marker) {
						t.Errorf("system instruction in user context: %q", marker)
					}
				}
				if strings.Contains(request.Input[0].Content, "# DurableState\n") {
					t.Error("runtime state included in system message")
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
				// 使用独有错误文本，避免与格式示例中的通用错误描述碰撞。
				for _, raw := range []string{"# MCP tools", "# ToolResult list", "file content", "worker-tool-error: access refused"} {
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
			rc.AppendToolResult(&fileContext.ToolResult{ToolCallId: "call_error", ToolName: "grep", Error: "worker-tool-error: access refused"})
			worker := NewWorker[ReactResult](context.Background(), &config.AgentConfig{AllowTools: map[config.McpToolName]struct{}{
				config.ReadFileMCPToolName: {}, config.GrepMCPToolName: {},
			}, LLMConfig: &config.LLMConfig{
				BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema,
			}}, process.mcpClientSess)
			got, _, calls, err := worker.Run(rc)
			if err != nil || got.Answer != "done" || len(calls) != 0 {
				t.Fatalf("Run() = %+v, %+v, %v", got, calls, err)
			}
			if strings.Contains(rc.Format(), "file content") || strings.Contains(rc.Format(), "worker-tool-error: access refused") || strings.Contains(rc.Format(), "# ToolResult list") {
				t.Fatal("ReAct context contains raw tool results or errors")
			}
			if len(rc.ToolResults) != 2 || rc.ToolResults[0].Result != "file content" || rc.ToolResults[1].Error != "worker-tool-error: access refused" {
				t.Fatal("formatting model input changed retained tool results")
			}
		})
	}
}

// 自定义实现验证 worker 仅依赖接口，并且上下文的工具限制优先于 worker 配置。
type workerTestContext struct {
	request fileContext.ModelRequest
}

func (c workerTestContext) ModelRequest() fileContext.ModelRequest { return c.request }
func (c workerTestContext) JsonFewShot() string                    { return "custom JSON contract" }
func (c workerTestContext) LineProtocolFewShot() string            { return "custom line contract" }

func TestWorkerAcceptsContextInterfaceAndHonorsToolRestriction(t *testing.T) {
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer process.mcpClientSess.Close()
	defer process.serverSess.Close()

	for _, unexpectedCall := range []bool{false, true} {
		t.Run(map[bool]string{false: "result", true: "unexpected tool call"}[unexpectedCall], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input string `json:"input"`
					Tools []any  `json:"tools"`
				}
				if err := decodeModelRequest(r.Body, &request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(request.Tools) != 0 {
					t.Error("context forbids tools but worker exposed MCP tools")
				}
				if strings.TrimSpace(request.Input) != "custom JSON contract\ncustom task\n\ncustom criteria" {
					t.Errorf("unexpected assembled input: %q", request.Input)
				}
				output := []any{observationMessage(`{"status":"completed","answer":"done","Observation":[],"CompletionEvidence":[]}`)}
				if unexpectedCall {
					output = []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read_file", "arguments": `{}`}}
				}
				writeObservationResponse(w, output)
			}))
			defer server.Close()
			worker := NewWorker[ReactResult](t.Context(), &config.AgentConfig{
				AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}},
				LLMConfig:  &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
			}, process.mcpClientSess)
			_, _, calls, err := worker.Run(workerTestContext{request: fileContext.ModelRequest{
				RunID: "custom", Input: "custom task", TrailingContext: "custom criteria", LogLabel: "custom",
			}})
			if unexpectedCall {
				if err == nil || !strings.Contains(err.Error(), "returned unexpected tool calls") {
					t.Fatalf("expected rejection of tool calls, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 0 {
				t.Fatalf("unexpected executable tools: %+v", calls)
			}
		})
	}
}
