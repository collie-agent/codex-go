package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

// 检查实际发给模型的纠错消息，避免把请求中原有的工具历史误认成新的提示。
func rejectionFeedback(t *testing.T, command, path string, maxExtensions int) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\nfunc observe() {}\nfunc other() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outputs := [][]any{
		modificationTool("observe-page", "read_file", map[string]any{"path": "main.go", "offset": 1, "limit": 2}),
		modificationTool("other-page", "read_file", map[string]any{"path": "main.go", "offset": 3, "limit": 1}),
		modificationHandoff(),
		modificationTool("denied", "exec", map[string]any{"command": command, "args": []string{"-n", "^func observe", path}}),
		{observationMessage(`{"exploration":{"choice":"unable","reason":"test complete","execution_state":"inspected source"}}`)},
		{observationMessage(`{"status":"failed","answer":"test complete"}`)},
	}
	var feedback string
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if requests >= len(outputs) {
			t.Error("unexpected request")
			http.Error(w, "unexpected request", 400)
			return
		}
		if requests == 4 {
			for _, m := range body.Input {
				if _, tail, ok := strings.Cut(m.Content, "Operation rejected:"); ok {
					feedback += tail
				} else if feedback != "" {
					feedback += m.Content
				}
			}
		}
		writeObservationResponse(w, outputs[requests])
		requests++
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 2
	policy.MaxExtensions = maxExtensions
	_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, &state.DurableState{ExecutionID: "rejection-hint", Goal: "fix main.go"})
	if feedback == "" {
		t.Fatalf("missing rejection feedback: requests=%d err=%v", requests, err)
	}
	return feedback
}

func TestRejectedSearchIdentifiesMatchingHistoricalPage(t *testing.T) {
	feedback := rejectionFeedback(t, "rg", "main.go", 1)
	if !strings.Contains(feedback, `read_tool_result({"call_id":"observe-page"`) || !strings.Contains(feedback, "main.go:1-2") || strings.Contains(feedback, "other-page") {
		t.Fatalf("expected matching historical page in rejection: %s", feedback)
	}
}

func TestRejectedSearchPointsToExtensionForMissingSource(t *testing.T) {
	feedback := rejectionFeedback(t, "rg", "fixture.go", 1)
	for _, want := range []string{"fixture.go", "choice=extend", "gap", "blocking_reason", "investigation", "exit_condition"} {
		if !strings.Contains(feedback, want) {
			t.Fatalf("missing %q in correction: %s", want, feedback)
		}
	}
}

func TestRejectedSearchOmitsUnavailableExtension(t *testing.T) {
	feedback := rejectionFeedback(t, "rg", "fixture.go", 0)
	if !strings.Contains(feedback, "fixture.go") || !strings.Contains(feedback, "unable") || strings.Contains(feedback, "extend") || strings.Contains(feedback, "申请探索延期") {
		t.Fatalf("incorrect next step with extensions exhausted: %s", feedback)
	}
}

func TestRejectedUnknownCommandPointsToHandoffEvidence(t *testing.T) {
	feedback := rejectionFeedback(t, "unrecognized", "main.go", 1)
	if !strings.Contains(feedback, "交接目标") || !strings.Contains(feedback, `read_tool_result({"call_id":"other-page"`) {
		t.Fatalf("missing handoff fallback: %s", feedback)
	}
}
