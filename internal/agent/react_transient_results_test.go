package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactDirectoryResultsLastThreeDecisions(t *testing.T) {
	durable := compactionHistory(1)
	durable.ToolExecutions = []*state.ToolResult{
		{Batch: 1, ToolCallId: "glob_call", ToolName: "glob", Arguments: `{}`, Status: "success", Result: "glob unique evidence"},
		{Batch: 1, ToolCallId: "dir_call", ToolName: "list_dir", Arguments: `{}`, Status: "success", Result: "directory unique evidence"},
		{Batch: 1, ToolCallId: "read_call", ToolName: "read_file", Arguments: `{}`, Status: "success", Result: "persistent file evidence"},
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests++
		input, _ := json.Marshal(body["input"])
		for _, evidence := range []string{"glob unique evidence", "directory unique evidence"} {
			if got := strings.Contains(string(input), evidence); got != (requests <= 3) {
				t.Errorf("request %d: evidence %q present=%v", requests, evidence, got)
			}
		}
		if !strings.Contains(string(input), "persistent file evidence") {
			t.Error("lost read_file result")
		}
		if requests <= 3 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_read", "call_id": fmt.Sprintf("recalled_%d", requests), "name": "read_tool_result", "arguments": fmt.Sprintf(`{"call_id":"read_call","offset":%d,"limit":10}`, requests-1)}})
			return
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := process.Run(compactionConfig(t, server.URL), durable); err != nil {
		t.Fatal(err)
	}
	if requests != 4 {
		t.Fatalf("requests=%d", requests)
	}
	if len(durable.ToolExecutions) != 6 || durable.ToolExecutions[0].Result != "glob unique evidence" || durable.ToolExecutions[1].Result != "directory unique evidence" {
		t.Fatal("lost durable tool results")
	}
}
