package agent_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactSkipsRepeatedToolCall(t *testing.T) {
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		var req struct {
			Input []struct {
				Role    string
				Content string
				Type    string
				CallID  string `json:"call_id"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if turns <= 2 {
			args := `{"path":"."}`
			if turns == 2 {
				args = " { \"path\" : " + strings.TrimPrefix(args, `{"path":`)
			}
			id := "original_call"
			if turns == 2 {
				id = "duplicate_call"
			}
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": "list_dir", "arguments": args})
			return
		}
		found := false
		for _, msg := range req.Input {
			if msg.Role == "user" && strings.Contains(msg.Content, "already called") && strings.Contains(msg.Content, "original_call") {
				found = true
			}
			if msg.Type == "function_call" && msg.CallID == "duplicate_call" {
				t.Error("duplicate call was executed and replayed")
			}
		}
		if !found {
			t.Error("missing user message with original call_id")
		}
		criteriaResponse(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "dedup", Goal: "inspect workspace", CompletionCriteria: []string{"workspace inspected"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err != nil || result.Answer != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(durable.ToolExecutions) != 1 {
		t.Fatalf("expected one actual call, got %d", len(durable.ToolExecutions))
	}
}

// 已确认的恢复历史与本次执行采用同一去重边界；参数和工具名仍须精确匹配。
func TestReactDeduplicatesAgainstRestoredHistory(t *testing.T) {
	for _, tc := range []struct {
		name, tool, arguments string
		duplicate             bool
	}{
		{"same_arguments_reordered", "list_dir", " { \"depth\": 1, \"path\": \".\" }", true},
		{"different_arguments", "list_dir", `{"path":".","depth":2}`, false},
		{"different_tool", "read_file", `{"path":".","depth":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turns := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turns++
				if turns == 1 {
					criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_new", "call_id": "new_call", "name": tc.tool, "arguments": tc.arguments})
					return
				}
				// read_file 的 schema 错误会返回给模型，不应被误判为重复。
				var req struct {
					Input []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				found := false
				for _, msg := range req.Input {
					if msg.Role == "user" && strings.Contains(msg.Content, "already called") && strings.Contains(msg.Content, "saved_call") {
						found = true
					}
				}
				if found != tc.duplicate {
					t.Errorf("duplicate feedback=%v, want %v", found, tc.duplicate)
				}
				criteriaResponse(w, `{"status":"completed","answer":"done"}`)
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			durable := &state.DurableState{RunId: "resume", Goal: "inspect workspace", ExecutionID: "restored-instance", CompletionCriteria: []string{"inspected"}, ToolExecutions: []*state.ToolResult{
				{ToolCallId: "saved_call", ToolName: "list_dir", Arguments: `{"path":".","depth":1}`, Status: filemcp.ToolCallSuceessStatus, Result: "[]"},
			}}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}, config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
			result, _, err := process.Run(cfg, durable)
			if err != nil || result.Answer != "done" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			want := 2
			if tc.duplicate {
				want = 1
			}
			if len(durable.ToolExecutions) != want {
				t.Errorf("tool calls=%d want=%d", len(durable.ToolExecutions), want)
			}
		})
	}
}
