package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
)

func TestResponsesFailureLoggedAtErrorLevel(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"request", `{"error":{"message":"provider unavailable"}}`, "provider unavailable", http.StatusBadRequest},
		{"response", `{"status":"failed","error":{"code":"server_error","message":"generation failed"}}`, "generation failed", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client, err := NewResponsesApiClient[struct {
				Answer string `json:"answer"`
			}](&config.LLMConfig{ApiKey: "secret", BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema}, nil)
			if err != nil {
				t.Fatal(err)
			}
			client.SetLogLevel(config.ErrorLogLevel)
			client.SetContextLogLabel("react")
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			_, _, _, err = client.Run(context.Background(), "failure-task", "prompt", nil)
			if err == nil || !strings.Contains(logs.String(), tc.want) || !strings.Contains(logs.String(), "task failure-task: [react] LLM ") {
				t.Fatalf("err=%v logs=%q", err, logs.String())
			}
		})
	}
}

func TestResponsesDisableThinkingAndExposeAPIError(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, withHistory := range []bool{false, true} {
			t.Run(string(schema)+"/history="+fmt.Sprint(withHistory), func(t *testing.T) {
				const apiKey = "test-secret-credential"
				const message = "The reasoning_text in the thinking mode must be passed back to the API."
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Reasoning struct {
							Effort string `json:"effort"`
						} `json:"reasoning"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request.Reasoning.Effort != "none" {
						t.Errorf("reasoning effort = %q, want none", request.Reasoning.Effort)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
						"message": message + " key=" + apiKey, "type": "invalid_request_error", "code": "invalid_request_error", "param": "input",
					}})
				}))
				defer server.Close()
				client, err := NewResponsesApiClient[struct {
					Answer string `json:"answer"`
				}](&config.LLMConfig{
					ApiKey: apiKey, BaseUrl: server.URL, Model: "test-model", OutputSchema: schema,
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				var history []ToolMessage
				if withHistory {
					history = []ToolMessage{{ChatCompletionToolMessageParam: *openai.ToolMessage("result", "call_1").OfTool, Name: "grep", Arguments: `{"pattern":"agent"}`}}
				}
				_, _, _, err = client.RunWithToolMessages(context.Background(), "error-test", "inspect", nil, history)
				if err == nil {
					t.Fatal("expected API error")
				}
				for _, want := range []string{"400 Bad Request", message, "invalid_request_error", "input", "<REDACTED>"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error missing %q: %v", want, err)
					}
				}
				if strings.Contains(err.Error(), apiKey) {
					t.Fatal("error exposes API key")
				}
				var apiErr *openai.Error
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
					t.Fatalf("lost API error chain: %v", err)
				}
			})
		}
	}
}

func TestNewResponsesApiClientRejectsNilConfig(t *testing.T) {
	if client, err := NewResponsesApiClient[any](nil, nil); err == nil || client != nil {
		t.Fatalf("nil config: client=%v, err=%v", client, err)
	}

}

func TestResponsesRunToolsWithoutHistoryAndClientReuse(t *testing.T) {
	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := newResponsesTestSession(t, ctx, workspace)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text struct {
				Format struct {
					Type   string         `json:"type"`
					Name   string         `json:"name"`
					Strict bool           `json:"strict"`
					Schema map[string]any `json:"schema"`
				} `json:"format"`
			} `json:"text"`
			Tools []struct {
				Name       string         `json:"name"`
				Strict     bool           `json:"strict"`
				Parameters map[string]any `json:"parameters"`
			} `json:"tools"`
			Input              string `json:"input"`
			PreviousResponseID string `json:"previous_response_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests++
		format := request.Text.Format
		if format.Type != "json_schema" || format.Name != "llm_output" || !format.Strict {
			t.Errorf("unexpected output format: %+v", format)
		}
		if format.Schema["type"] != "object" || format.Schema["additionalProperties"] != false {
			t.Errorf("unexpected output schema: %v", format.Schema)
		}
		properties, _ := format.Schema["properties"].(map[string]any)
		message, _ := properties["message"].(map[string]any)
		required, _ := format.Schema["required"].([]any)
		if message["type"] != "string" || len(required) != 1 || required[0] != "message" {
			t.Errorf("output schema does not describe result: %v", format.Schema)
		}
		tools := map[string]bool{}
		for _, tool := range request.Tools {
			tools[tool.Name] = true
			if !tool.Strict || tool.Parameters["type"] != "object" || tool.Parameters["additionalProperties"] != false {
				t.Errorf("tool %s has invalid schema: %v", tool.Name, tool.Parameters)
			}
			properties := tool.Parameters["properties"].(map[string]any)
			required, _ := tool.Parameters["required"].([]any)
			if len(required) != len(properties) {
				t.Errorf("tool %s must require every property: %v", tool.Name, tool.Parameters)
			}
			for name := range properties {
				property := properties[name].(map[string]any)
				if types, ok := property["type"].([]any); ok && slices.Contains(types, any("null")) {
					t.Errorf("tool %s property %s still allows null", tool.Name, name)
				}
				if !slices.Contains(required, any(name)) {
					t.Errorf("tool %s missing required property %s", tool.Name, name)
				}
			}
			if tool.Name == "list_dir" {
				depth := properties["depth"].(map[string]any)
				if depth["type"] != "integer" {
					t.Errorf("depth must be an integer: %v", depth)
				}
				encoded, _ := json.Marshal(tool.Parameters)
				var schema jsonschema.Schema
				if err := json.Unmarshal(encoded, &schema); err != nil {
					t.Fatal(err)
				}
				resolved, err := schema.Resolve(nil)
				if err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"path": ".", "depth": 2, "offset": 0, "limit": 200}
				if err := resolved.Validate(args); err != nil {
					t.Errorf("valid list_dir arguments rejected: %v", err)
				}
				args["depth"] = "2"
				if err := resolved.Validate(args); err == nil {
					t.Error("list_dir schema accepted string depth")
				}
				args["depth"] = nil
				if err := resolved.Validate(args); err == nil {
					t.Error("list_dir schema accepted null depth")
				}
				delete(args, "depth")
				result, err := session.CallTool(r.Context(), &sdk.CallToolParams{Name: "list_dir", Arguments: args})
				if err != nil {
					t.Errorf("call list_dir with defaults: %v", err)
				} else if result.IsError {
					t.Errorf("list_dir defaults rejected by MCP: %v", result.Content)
				}
			}
		}
		for _, name := range []string{"read_file", "list_dir", "glob", "grep", "apply_patch"} {
			if !tools[name] {
				t.Errorf("request missing tool %s", name)
			}
		}
		if request.Input != "Read notes.txt" {
			t.Errorf("unexpected input: %q", request.Input)
		}
		if request.PreviousResponseID != "" {
			t.Errorf("unexpected previous response ID: %q", request.PreviousResponseID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_final","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"message\":\"hello MCP\"}","annotations":[]}]}]}`))
	}))
	defer server.Close()
	type result struct {
		Message string `json:"message"`
	}
	allowTools := map[config.McpToolName]struct{}{}
	for _, name := range []string{"read_file", "list_dir", "glob", "grep", "apply_patch"} {
		allowTools[config.McpToolName(name)] = struct{}{}
	}
	client, err := NewResponsesApiClient[result](&config.LLMConfig{
		ApiKey: "test", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema,
	}, allowTools)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, toolCalls, reasoning, err := client.Run(ctx, "test-task", "Read notes.txt", session)
		if err != nil {
			t.Fatal(err)
		}
		if reasoning != "" {
			t.Fatalf("unexpected reasoning: %q", reasoning)
		}
		if len(toolCalls) != 0 {
			t.Fatalf("unexpected tool calls: %+v", toolCalls)
		}
		if got.Message != "hello MCP" {
			t.Fatalf("unexpected result: %+v", got)
		}
	}
	if requests != 2 {
		t.Fatalf("got %d requests, want 2", requests)
	}
}

func newResponsesTestSession(t *testing.T, ctx context.Context, workspace string) *sdk.ClientSession {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "responses-test-server"}, nil)
	if err := filemcp.RegisterTools(server, filemcp.Config{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := serverSession.Close(); err != nil {
			t.Errorf("close MCP server session: %v", err)
		}
	})
	client := sdk.NewClient(&sdk.Implementation{Name: "responses-test-client"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP client session: %v", err)
		}
	})
	return session
}

func TestResponsesRunToolCallsSkipParsing(t *testing.T) {
	for _, reasoning := range []string{"I need to read the file first.", ""} {
		t.Run(reasoning, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_tools", "status": "completed",
					"output": []any{
						map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{
							map[string]any{"type": "output_text", "text": reasoning, "annotations": []any{}},
						}},
						map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"notes.txt"}`},
					},
				})
			}))
			defer server.Close()
			type result struct {
				Message string `json:"message"`
			}
			client, err := NewResponsesApiClient[result](&config.LLMConfig{
				ApiKey: "test", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			session := newResponsesTestSession(t, ctx, t.TempDir())
			got, calls, gotReasoning, err := client.Run(ctx, "test-task", "Read notes.txt", session)
			if err != nil {
				t.Fatal(err)
			}
			if got != (result{}) || gotReasoning != reasoning {
				t.Fatalf("got result=%+v reasoning=%q, want zero result and reasoning=%q", got, gotReasoning, reasoning)
			}
			if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].CallID != "call_1" || calls[0].Arguments != `{"path":"notes.txt"}` {
				t.Fatalf("unexpected tool calls: %+v", calls)
			}
		})
	}
}

func TestResponsesRunLineProtocolWithoutJSONFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if text, ok := request["text"]; ok && string(text) != "{}" {
			t.Errorf("line protocol should not send JSON format: %s", text)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_line","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"BEGIN RESULT\nMESSAGE=\"hello\"\nEND RESULT","annotations":[]}]}]}`))
	}))
	defer server.Close()
	type result struct {
		Message string `json:"message"`
	}
	client, err := NewResponsesApiClient[result](&config.LLMConfig{
		ApiKey: "test", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.LineProtocolLLMOutputSchema,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _, err := client.Run(context.Background(), "line-task", "Say hello", nil)
	if err != nil || got.Message != "hello" {
		t.Fatalf("got result=%+v err=%v", got, err)
	}
}

func TestResponsesRunLineProtocolWithPreamble(t *testing.T) {
	answer := "该项目的 LLM API 是 OpenAI Responses API（v3）。证据：\n1. `internal/provider/responses.go` 导入 Responses SDK。"
	literal, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	text := "Based on the code examined, I have enough information to answer. The project uses the **OpenAI Responses API** (not the Chat Completions API).\n\nBEGIN RESULT\nSTATUS=\"completed\"\nANSWER=" + string(literal) + "\nEND RESULT"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_line", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
			}},
		})
	}))
	defer server.Close()
	type result struct {
		Status string `json:"status"`
		Answer string `json:"answer"`
	}
	client, err := NewResponsesApiClient[result](&config.LLMConfig{
		ApiKey: "test", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.LineProtocolLLMOutputSchema,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, calls, _, err := client.Run(context.Background(), "cli-task", "Identify the LLM API", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.Answer != answer || len(calls) != 0 {
		t.Fatalf("unexpected result=%+v calls=%+v", got, calls)
	}
}
