package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewResponsesApiClientRejectsNilConfig(t *testing.T) {
	if client, err := NewResponsesApiClient[any](nil, filemcp.Config{}); err == nil || client != nil {
		t.Fatalf("nil config: client=%v, err=%v", client, err)
	}

}

func TestResponsesRunToolsWithoutHistoryAndClientReuse(t *testing.T) {
	workspace := t.TempDir()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Name       string         `json:"name"`
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
		tools := map[string]bool{}
		for _, tool := range request.Tools {
			tools[tool.Name] = true
			if tool.Parameters["type"] != "object" {
				t.Errorf("tool %s has invalid schema: %v", tool.Name, tool.Parameters)
			}
		}
		for _, name := range []string{"read_file", "list_dir", "glob", "grep", "apply_patch"} {
			if !tools[name] {
				t.Errorf("request missing tool %s", name)
			}
		}
		if request.Input != "Read notes.txt\n\n"+jsonFewShotPrompt {
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
	client, err := NewResponsesApiClient[result](&config.LLMConfig{
		ApiKey: "test", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema,
	}, filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	client.agentConfig.AllowTools = map[config.McpToolName]struct{}{}
	for _, name := range []string{"read_file", "list_dir", "glob", "grep", "apply_patch"} {
		client.agentConfig.AllowTools[config.McpToolName(name)] = struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session := newResponsesTestSession(t, ctx, workspace)
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
			}, filemcp.Config{})
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
