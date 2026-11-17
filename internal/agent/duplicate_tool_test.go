package agent_test

import (
	"encoding/json"
	"fmt"
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

func TestReactSkipsRepeatedToolCall(t *testing.T) {
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		var req struct {
			Input []struct {
				Role    string
				Content string
				Output  string
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
		replayedCall, replayedOutput := false, false
		for _, msg := range req.Input {
			if msg.Role == "user" && strings.Contains(msg.Content, "already called") {
				t.Error("duplicate feedback must not be a user message")
			}
			if msg.Type == "function_call" && msg.CallID == "duplicate_call" {
				replayedCall = true
			}
			if msg.Type == "function_call_output" && msg.CallID == "duplicate_call" && strings.Contains(msg.Output, "already called") && strings.Contains(msg.Output, "original_call") {
				replayedOutput = true
			}
		}
		if !replayedCall || !replayedOutput {
			t.Error("duplicate call must receive a paired native cached tool result")
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
					Input []struct{ Role, Content, Type, Output string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				found := false
				for _, msg := range req.Input {
					if msg.Type == "function_call_output" && strings.Contains(msg.Output, "already called") && strings.Contains(msg.Output, "saved_call") {
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

func TestReactStopsConsecutiveDuplicateBatches(t *testing.T) {
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_repeat", "call_id": fmt.Sprintf("repeat_%d", turns), "name": "list_dir", "arguments": `{"path":"."}`})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	durable := &state.DurableState{RunId: "repeat", Goal: "inspect", CompletionCriteria: []string{"inspected"}}
	_, _, err = process.Run(cfg, durable)
	if err == nil || !strings.Contains(err.Error(), "consecutive duplicate") {
		t.Fatalf("error=%v", err)
	}
	if turns != 4 {
		t.Fatalf("model turns=%d, want 4", turns)
	}
	if len(durable.ToolExecutions) != 1 {
		t.Fatalf("executed calls=%d", len(durable.ToolExecutions))
	}
}

// Commands observe mutable workspace state and must run again after a repair.
func TestReactRepeatsExecAfterWorkspaceChange(t *testing.T) {
	workspace := t.TempDir()
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		switch turns {
		case 1, 3:
			args, _ := json.Marshal(map[string]any{"command": "sed", "args": []string{"-i", "s/$/x/", "count"}})
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": "exec", "arguments": string(args)})
		case 2:
			args, _ := json.Marshal(map[string]any{"command": "sed", "args": []string{"-i", "s/.*/10/", "count"}})
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_repair", "call_id": "repair", "name": "exec", "arguments": string(args)})
		default:
			criteriaResponse(w, `{"status":"completed","answer":"done"}`)
		}
	}))
	defer server.Close()
	if err := os.WriteFile(filepath.Join(workspace, "count"), []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	durable := &state.DurableState{RunId: "exec-repair", Goal: "repair and retry", CompletionCriteria: []string{"command rerun after repair"}}
	if _, _, err := process.Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "count"))
	if err != nil || string(data) != "10x\n" {
		t.Fatalf("command did not run after repair: count=%q err=%v", data, err)
	}
}

// 目录结果过期后，重复调用的纠错必须携带原结果，而不能只引用已不可见的历史。
func TestReactDuplicateKeepsResumedDirectoryResult(t *testing.T) {
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		input, _ := json.Marshal(request["input"])
		if turns == 4 && !strings.Contains(string(input), "directory recovery evidence") {
			t.Error("resumed directory result missing from conversation history")
		}
		if turns <= 3 {
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("read_%d", turns), "name": "read_tool_result", "arguments": `{"call_id":"file_call","offset":0,"limit":10}`})
			return
		}
		if turns == 4 {
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_duplicate", "call_id": "duplicate", "name": "list_dir", "arguments": `{"path":"."}`})
			return
		}
		if !strings.Contains(string(input), "directory recovery evidence") {
			t.Error("duplicate feedback lost the restored result")
		}
		criteriaResponse(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "recover-directory", ExecutionID: "instance", Goal: "inspect", CompletionCriteria: []string{"inspected"}, ToolExecutions: []*state.ToolResult{
		{Batch: 1, ToolCallId: "dir_call", ToolName: "list_dir", Arguments: `{"path":"."}`, Status: filemcp.ToolCallSuceessStatus, Result: "directory recovery evidence"},
		{Batch: 1, ToolCallId: "file_call", ToolName: "read_file", Arguments: `{"path":"sample.txt"}`, Status: filemcp.ToolCallSuceessStatus, Result: "file evidence"},
	}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err != nil || result.Answer != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if turns != 5 || len(durable.ToolExecutions) != 5 {
		t.Fatalf("turns=%d executions=%d", turns, len(durable.ToolExecutions))
	}
}

// 修改文件后相同参数的读取必须重新执行，不能返回修改前的缓存。
func TestReactReadsFreshFileAfterPatch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []struct {
				Content string
				Type    string
				CallID  string `json:"call_id"`
				Output  string
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.Input) > 0 && strings.Contains(req.Input[0].Content, "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		turn++
		if turn == 4 {
			fresh := false
			for _, item := range req.Input {
				if item.Type == "function_call_output" && item.CallID == "read_after" && strings.Contains(item.Output, "after") && !strings.Contains(item.Output, "before") {
					fresh = true
				}
			}
			if !fresh {
				t.Error("read after patch returned stale content")
			}
			criteriaResponse(w, `{"status":"completed","answer":"done"}`)
			return
		}
		name, id, arguments := "read_file", "read_before", `{"path":"sample.txt","offset":1,"limit":20}`
		if turn == 2 {
			name, id, arguments = "apply_patch", "patch", `{"patch":"--- sample.txt\n+++ sample.txt\n@@ -1 +1 @@\n-before\n+after\n"}`
		}
		if turn == 3 {
			id = "read_after"
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": arguments})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "fresh", Goal: "edit and verify", CompletionCriteria: []string{"verified"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err != nil || result.Answer != "done" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(durable.ToolExecutions) != 3 {
		t.Fatalf("actual executions=%d, want 3", len(durable.ToolExecutions))
	}
}

func TestReactResumeInvalidatesReadsBeforeWorkspaceChange(t *testing.T) {
	for _, mutation := range []string{"apply_patch", "exec", "exec_start", "exec_poll", "exec_stop"} {
		for _, sameBatch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same_batch=%v", mutation, sameBatch), func(t *testing.T) {
				workspace := t.TempDir()
				if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("current content\n"), 0600); err != nil {
					t.Fatal(err)
				}
				turn := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					turn++
					if turn == 1 {
						criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_fresh", "call_id": "fresh", "name": "read_file", "arguments": `{"path":"sample.txt","offset":1,"limit":20}`})
						return
					}
					var req struct {
						Input []struct {
							Type   string
							CallID string `json:"call_id"`
							Output string
						}
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					fresh := false
					for _, item := range req.Input {
						if item.Type == "function_call_output" && item.CallID == "fresh" && strings.Contains(item.Output, "current content") {
							fresh = true
						}
					}
					if !fresh {
						t.Error("resume reused a read from before the workspace change")
					}
					criteriaResponse(w, `{"status":"completed","answer":"done"}`)
				}))
				defer server.Close()
				process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
				if err != nil {
					t.Fatal(err)
				}
				mutationBatch := 2
				if sameBatch {
					mutationBatch = 1
				}
				durable := &state.DurableState{RunId: "fresh-resume", ExecutionID: "instance", Goal: "verify", CompletionCriteria: []string{"verified"}, ToolExecutions: []*state.ToolResult{
					{Batch: 1, ToolCallId: "old", ToolName: "read_file", Arguments: `{"path":"sample.txt","offset":1,"limit":20}`, Status: filemcp.ToolCallSuceessStatus, Result: "stale content"},
					// 失败的写入也可能留下部分修改，不能据此保留旧读取缓存。
					{Batch: mutationBatch, ToolCallId: "change", ToolName: mutation, Arguments: `{}`, Status: "failed", Error: "partial failure"},
				}}
				cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
				result, _, err := process.Run(cfg, durable)
				if err != nil || result.Answer != "done" {
					t.Fatalf("result=%+v error=%v", result, err)
				}
				if len(durable.ToolExecutions) != 3 {
					t.Fatalf("executions=%d, want 3", len(durable.ToolExecutions))
				}
			})
		}
	}
}
