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

func TestReactImplementationPromptIsLastUserMessage(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outputs := [][]any{
		modificationTool("read", "read_file", map[string]any{"path": "main.go"}),
		modificationHandoff(),
		modificationTool("invalid", "unknown_tool", map[string]any{}),
		modificationTool("denied", "read_file", map[string]any{"path": "main.go"}),
		modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}),
		{observationMessage(`{"status":"completed","answer":"fixed"}`)},
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if strings.Contains(body.Input[0].Content, "你是 ReAct agent 的工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		if requests >= len(outputs) {
			t.Error("unexpected model request")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		const prefix = "已进入实施阶段，剩余修改窗口为 "
		implementing := requests >= 2 && requests <= 4
		for i, message := range body.Input {
			if strings.Contains(message.Content, prefix) && (!implementing || i != len(body.Input)-1) {
				t.Errorf("implementation prompt in wrong message: request=%d index=%d", requests, i)
			}
		}
		if implementing {
			last := body.Input[len(body.Input)-1]
			remaining := "3"
			if requests == 4 {
				remaining = "2"
			}
			for _, want := range []string{prefix + remaining + " 轮", "target_files", "next_operation", "工作区检查", "探索延期", "unable", "修改后执行验证"} {
				if last.Role != "user" || !strings.Contains(last.Content, want) {
					t.Errorf("missing last user instruction %q: %+v", want, last)
				}
			}
			if requests >= 3 && !strings.Contains(body.Input[len(body.Input)-2].Content, "LLM tool call error") {
				t.Error("expected correction message before implementation instruction")
			}
		}
		out := outputs[requests]
		requests++
		writeObservationResponse(w, out)
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: policy, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, &state.DurableState{ExecutionID: "implementation-prompt", Goal: "fix main.go", CompletionCriteria: []string{"fixed"}})
	if err != nil || requests != len(outputs) {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}
