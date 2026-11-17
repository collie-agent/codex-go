package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactSendsDiscoveryResultsAfterOtherToolsBeforeDurableState(t *testing.T) {
	durable := compactionHistory(1)
	durable.ToolExecutions = []*state.ToolResult{
		{Batch: 1, ToolCallId: "glob_1", ToolName: "glob", Arguments: `{"pattern":"*.go"}`, Status: "success", Result: "glob evidence"},
		{Batch: 1, ToolCallId: "read_1", ToolName: "read_file", Arguments: `{"path":"main.go"}`, Status: "success", Result: "file evidence"},
		{Batch: 1, ToolCallId: "dir_1", ToolName: "list_dir", Arguments: `{"path":"."}`, Status: "success", Result: "directory evidence"},
		{Batch: 1, ToolCallId: "grep_1", ToolName: "grep", Arguments: `{"pattern":"main"}`, Status: "success", Result: "grep evidence"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				CallID  string `json:"call_id"`
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var order []string
		for _, item := range body.Input {
			if item.CallID != "" {
				order = append(order, item.Type+":"+item.CallID)
			} else {
				order = append(order, item.Role)
				if item.Role == "user" && !strings.HasPrefix(item.Content, "# DurableState\n") {
					t.Error("expected DurableState user message")
				}
			}
		}
		want := []string{"system", "function_call:read_1", "function_call_output:read_1", "function_call:grep_1", "function_call_output:grep_1", "function_call:glob_1", "function_call_output:glob_1", "function_call:dir_1", "function_call_output:dir_1", "user"}
		if !reflect.DeepEqual(order, want) {
			t.Errorf("message order = %v; want %v", order, want)
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// 本测试检查本次执行新增的工具消息；恢复记录另由历史消息测试覆盖。
	defer process.mcpClientSess.Close()
	defer process.serverSess.Close()
	rc := state.NewReactContext(durable)
	for _, call := range durable.ToolExecutions {
		rc.AppendToolResult(call)
	}
	worker := NewWorker[ReactResult](t.Context(), compactionConfig(t, server.URL), process.mcpClientSess)
	if _, _, _, err := worker.Run(rc); err != nil {
		t.Fatal(err)
	}
	if durable.ToolExecutions[0].ToolCallId != "glob_1" || durable.ToolExecutions[1].ToolCallId != "read_1" {
		t.Error("request ordering changed durable execution history")
	}
}
