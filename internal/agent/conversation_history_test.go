package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestWorkerSeparatesResumedHistoryFromNewToolMessages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []struct {
				Role, Type, Content, Output string
				CallID                      string `json:"call_id"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if len(req.Input) != 5 {
			t.Errorf("unexpected messages: %+v", req.Input)
			return
		}
		if req.Input[0].Role != "system" || strings.Contains(req.Input[0].Content, "old-evidence") {
			t.Error("history leaked into system")
		}
		history := req.Input[1]
		if history.Role != "user" || !strings.HasPrefix(history.Content, "# Conversation history") {
			t.Errorf("missing history user: %+v", history)
		}
		for _, want := range []string{"previous-answer", "old-call", "old-evidence", "execution-1"} {
			if !strings.Contains(history.Content, want) {
				t.Errorf("history missing %q", want)
			}
		}
		if strings.Contains(history.Content, "new-evidence") {
			t.Error("new tool result leaked into historical snapshot")
		}
		if req.Input[2].Type != "function_call" || req.Input[2].CallID != "new-call" || req.Input[3].Type != "function_call_output" || req.Input[3].Output != "new-evidence" || req.Input[4].Role != "user" {
			t.Errorf("unexpected native tool ordering: %+v", req.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"done","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\"}"}]}]}`))
	}))
	defer server.Close()
	rc := state.NewReactContext(&state.DurableState{RunId: "run", ExecutionID: "execution-1", Goal: "continue", PreviousRunOutcomes: []state.RunOutcome{{RunID: "previous", Result: "previous-answer"}}})
	restored := &state.ToolResult{ToolCallId: "old-call", ToolName: "read_file", Arguments: `{"path":"old"}`, Status: "success", Result: "old-evidence"}
	rc.RestoreToolResults([]*state.ToolResult{restored})
	restored.Result = "changed-after-restore"
	rc.AppendToolResult(&state.ToolResult{ToolCallId: "new-call", ToolName: "read_file", Arguments: `{"path":"new"}`, Status: "success", Result: "new-evidence"})
	worker := NewWorker[ReactResult](t.Context(), &config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, nil)
	if _, _, _, err := worker.Run(rc); err != nil {
		t.Fatal(err)
	}
}

func TestReactResumePlacesSavedToolsInHistory(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req struct {
			Input []struct{ Role, Type, Content string } `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if len(req.Input) != 3 || req.Input[0].Role != "system" || req.Input[1].Role != "user" || !strings.Contains(req.Input[1].Content, "saved-evidence") || req.Input[2].Role != "user" {
			t.Errorf("unexpected resumed context: %+v", req.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"done","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\",\"CompletionEvidence\":[\"verified\"]}"}]}]}`))
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	durable := &state.DurableState{RunId: "run", ExecutionID: "execution", Goal: "continue", CompletionCriteria: []string{"verified"}, ToolExecutions: []*state.ToolResult{{ToolCallId: "saved", ToolName: "read_file", Arguments: `{}`, Status: "success", Result: "saved-evidence", Batch: 1}}}
	if _, _, err := process.Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(durable.ToolExecutions) != 1 {
		t.Fatalf("requests=%d records=%d", requests, len(durable.ToolExecutions))
	}
}

func TestCompactionFiltersResumedHistoryWithoutNativeReplay(t *testing.T) {
	rc := state.NewReactContext(&state.DurableState{RunId: "run", ExecutionID: "execution", PreviousRunOutcomes: []state.RunOutcome{{RunID: "prior", Result: "prior-answer"}}})
	rc.RestoreToolResults([]*state.ToolResult{
		{ToolCallId: "covered", Batch: 1, ToolName: "read_file", Status: "success", Result: "covered-evidence"},
		{ToolCallId: "retained", Batch: 2, ToolName: "read_file", Status: "success", Result: "retained-evidence"},
	})
	rc.AppendToolResult(&state.ToolResult{ToolCallId: "new", Batch: 3, ToolName: "read_file", Status: "success", Result: "new-evidence"})
	model := &compactModelContext{Contexter: rc, compaction: &ExecutionCompaction{ThroughBatch: 1}}
	request := model.ModelRequest()
	history := request.ConversationHistoryText()
	if strings.Contains(history, "covered-evidence") || !strings.Contains(history, "retained-evidence") || !strings.Contains(history, "prior-answer") {
		t.Errorf("unexpected compressed history: %s", history)
	}
	if len(request.ToolResults) != 1 || request.ToolResults[0].ToolCallId != "new" {
		t.Fatalf("resumed tools replayed natively: %+v", request.ToolResults)
	}
	if !strings.Contains(rc.ModelRequest().ConversationHistoryText(), "covered-evidence") {
		t.Error("compression mutated original context")
	}
}
