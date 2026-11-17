package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/provider"
)

func TestReactExplorationRejectionPublishesToolErrors(t *testing.T) {
	for _, decision := range []string{
		"",
		`{"exploration":{"choice":"extend","gap":"caller contract","blocking_reason":"unsafe patch","investigation":"read caller","exit_condition":"contract known"}}`,
		`{"exploration":{"choice":"unable","reason":"missing contract","execution_state":"source inspected"}}`,
	} {
		for _, window := range []int{1, 3} {
			t.Run(fmt.Sprintf("decision=%s/window=%d", decision, window), func(t *testing.T) {
				workspace := t.TempDir()
				path := filepath.Join(workspace, "main.go")
				if err := os.WriteFile(path, []byte("package old\n"), 0600); err != nil {
					t.Fatal(err)
				}
				patch := map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}
				denied := modificationTool("denied-read", "read_file", map[string]any{"path": "main.go"})
				denied = append(denied, modificationTool("denied-patch", "apply_patch", patch)...)
				if decision != "" {
					denied = append(denied, observationMessage(decision))
				}
				outputs := [][]any{
					modificationTool("read", "read_file", map[string]any{"path": "main.go"}),
					modificationHandoff(), denied,
					modificationTool("patch", "apply_patch", patch),
					{observationMessage(`{"status":"completed","answer":"fixed"}`)},
				}
				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct{ Input json.RawMessage }
					_ = json.NewDecoder(r.Body).Decode(&body)
					if strings.Contains(string(body.Input), "你是 ReAct agent 的工具结果观察器") {
						writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
						return
					}
					if requests >= len(outputs) {
						t.Error("unexpected model request")
						http.Error(w, "unexpected request", http.StatusBadRequest)
						return
					}
					output := outputs[requests]
					requests++
					writeObservationResponse(w, output)
				}))
				defer server.Close()
				output := &recoveryOutput{}
				process, err := NewReactProcess[ReactResult](provider.WithOutputer(t.Context(), output), filemcp.Config{Workspace: workspace})
				if err != nil {
					t.Fatal(err)
				}
				policy := config.DefaultExplorationBudget()
				policy.InitialTurns = 1
				policy.ModificationWindowTurns = window
				durable := &state.DurableState{ExecutionID: "rejected-output", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}}
				_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
				wantContent := "package main\n"
				if window == 1 {
					wantContent = "package old\n"
					if err == nil || !strings.Contains(err.Error(), "modification window exhausted") {
						t.Fatalf("err=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(path)
				if err != nil || string(content) != wantContent {
					t.Fatalf("content=%q err=%v", content, err)
				}
				for id, name := range map[string]string{"denied-read": "read_file", "denied-patch": "apply_patch"} {
					calls, terminals := 0, 0
					for _, event := range output.events {
						if event.Kind != provider.OutputToolCall && event.Kind != provider.OutputToolError && event.Kind != provider.OutputToolResult {
							continue
						}
						var payload provider.ToolResultOutput
						if err := json.Unmarshal(event.Data, &payload); err != nil {
							t.Fatal(err)
						}
						if payload.CallID != id {
							continue
						}
						if event.Kind == provider.OutputToolCall {
							calls++
							continue
						}
						terminals++
						if calls != 1 || event.Kind != provider.OutputToolError || payload.Name != name || !strings.Contains(payload.Error, "not executed") {
							t.Errorf("invalid rejection event: %+v", event)
						}
					}
					if calls != 1 || terminals != 1 {
						t.Errorf("call_id=%s: calls=%d terminal events=%d", id, calls, terminals)
					}
					for _, call := range durable.ToolExecutions {
						if call.ToolCallId == id {
							t.Errorf("rejected call registered as executed: %+v", call)
						}
					}
				}
			})
		}
	}
}
