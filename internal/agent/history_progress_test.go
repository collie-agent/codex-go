package agent_test

import (
	"encoding/json"
	"fmt"
	"io"
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

func TestReactHistoryReadReportsActualCharacterRange(t *testing.T) {
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		body, _ := io.ReadAll(r.Body)
		if turn == 1 {
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_page", "call_id": "page", "name": "read_tool_result", "arguments": `{"call_id":"source","offset":1,"limit":9}`})
			return
		}
		var req struct {
			Input []struct {
				Type, Output string
				CallID       string `json:"call_id"`
			}
		}
		_ = json.Unmarshal(body, &req)
		found := false
		for _, item := range req.Input {
			if item.Type != "function_call_output" || item.CallID != "page" {
				continue
			}
			var blocks []struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(item.Output), &blocks); err != nil {
				t.Fatal(err)
			}
			var page map[string]any
			if err := json.Unmarshal([]byte(blocks[0].Text), &page); err != nil {
				t.Fatal(err)
			}
			if page["call_id"] != "source" || page["offset"] != float64(1) || page["next_offset"] != float64(3) || page["eof"] != true || page["content"] != "好!" {
				t.Errorf("unexpected history page: %v", page)
			}
			found = true
		}
		if !found {
			t.Error("history page did not reach model")
		}
		criteriaResponse(w, `{"status":"completed","answer":"read"}`)
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "history", ExecutionID: "history-execution", Goal: "read", CompletionCriteria: []string{"read"}, ToolExecutions: []*state.ToolResult{{Batch: 1, ToolCallId: "source", ToolName: "read_file", Status: filemcp.ToolCallSuceessStatus, Result: "你好!", Arguments: `{"path":"value"}`}}}
	cfg := historyProgressConfig(server.URL)
	if _, _, err := process.Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
}

func historyProgressConfig(url string) *config.AgentConfig {
	return &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: url, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
}

func TestReactHistoryRepeatedPageEntersRecoveryAndCompletes(t *testing.T) {
	for _, mode := range []string{"same-page", "eof", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("value\n"), 0600); err != nil {
				t.Fatal(err)
			}
			turn, recovered := 0, false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				turn++
				if strings.Contains(string(body), "investigation recovery") {
					recovered = true
					criteriaResponse(w, `{"status":"completed","answer":"used existing evidence"}`)
					return
				}
				if turn > 8 {
					t.Error("same history page did not enter recovery")
					criteriaResponse(w, `{"status":"completed","answer":"deadline"}`)
					return
				}
				offset := 0
				if mode == "eof" {
					offset = 900
				}
				calls := []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turn), "call_id": fmt.Sprintf("page_%d", turn), "name": "read_tool_result", "arguments": fmt.Sprintf(`{"call_id":"source","offset":%d,"limit":3}`, offset)}}
				if mode == "mixed" {
					calls = append(calls, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_file_%d", turn), "call_id": fmt.Sprintf("file_%d", turn), "name": "read_file", "arguments": fmt.Sprintf(`{"path":"value.txt","limit":%d}`, 20+turn)})
				}
				criteriaResponse(w, "", calls...)
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2})
			durable := &state.DurableState{RunId: "history-repeat", ExecutionID: "history-repeat-instance", Goal: "use evidence", CompletionCriteria: []string{"use evidence"}, ToolExecutions: []*state.ToolResult{{Batch: 1, ToolCallId: "source", ToolName: "exec", Status: filemcp.ToolCallSuceessStatus, Result: "source evidence"}}}
			result, _, err := process.Run(historyProgressConfig(server.URL), durable)
			if err != nil || result.Status != agent.CompletedReactStatus || !recovered {
				t.Fatalf("result=%+v recovery=%v err=%v", result, recovered, err)
			}
		})
	}
}

func TestReactHistoryNewPagesContinueWithoutRecovery(t *testing.T) {
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		turn++
		if strings.Contains(string(body), "investigation recovery") {
			t.Error("new history page was treated as stalled")
		}
		if turn == 7 {
			criteriaResponse(w, `{"status":"completed","answer":"read all pages"}`)
			return
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turn), "call_id": fmt.Sprintf("page_%d", turn), "name": "read_tool_result", "arguments": fmt.Sprintf(`{"call_id":"source","offset":%d,"limit":3}`, (turn-1)*3)})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 1})
	durable := &state.DurableState{RunId: "history-pages", ExecutionID: "pages", Goal: "read pages", CompletionCriteria: []string{"read pages"}, ToolExecutions: []*state.ToolResult{{Batch: 1, ToolCallId: "source", ToolName: "exec", Status: filemcp.ToolCallSuceessStatus, Result: "abcdefghijklmnopqr"}}}
	result, _, err := process.Run(historyProgressConfig(server.URL), durable)
	if err != nil || result.Answer != "read all pages" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReactHistoryCompactionAllowsOneNecessaryReread(t *testing.T) {
	turn, compactions := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "# 执行上下文压缩") {
			compactions++
			criteriaResponse(w, `{"Facts":[{"Text":"source is available","SourceCallIDs":["source"]}],"Actions":[],"OpenIssues":[]}`)
			return
		}
		turn++
		recovery := strings.Contains(string(body), "investigation recovery")
		if turn < 4 && recovery {
			t.Error("first reread after compaction was treated as stalled")
		}
		if turn == 4 {
			if !recovery {
				t.Error("repeated same-generation page did not trigger recovery")
			}
			criteriaResponse(w, `{"status":"completed","answer":"recalled after compaction"}`)
			return
		}
		call := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turn), "call_id": fmt.Sprintf("page_%d", turn), "name": "read_tool_result", "arguments": `{"call_id":"source","offset":0,"limit":3}`}
		if turn == 1 {
			// A provider usage report forces real compaction before the next decision.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "usage", "status": "completed", "usage": map[string]any{"input_tokens": 66000, "output_tokens": 1, "total_tokens": 66001}, "output": []any{call}})
			return
		}
		if !strings.Contains(string(body), "# Execution summary") {
			t.Error("reread was not made from a compacted model context")
		}
		criteriaResponse(w, "", call)
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 1})
	durable := &state.DurableState{RunId: "history-compact", ExecutionID: "compact-pages", Goal: "recall", CompletionCriteria: []string{"recall"}, ToolExecutions: []*state.ToolResult{{Batch: 1, ToolCallId: "source", ToolName: "exec", Status: filemcp.ToolCallSuceessStatus, Result: "original evidence"}}}
	result, _, err := process.Run(historyProgressConfig(server.URL), durable)
	if err != nil || result.Answer != "recalled after compaction" || compactions != 1 {
		t.Fatalf("result=%+v compactions=%d err=%v", result, compactions, err)
	}
}
