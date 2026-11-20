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

func runPostPatchVerification(t *testing.T, replies [][]any) ([]string, *state.DurableState, error) {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package example\nconst value = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outputs := append([][]any{
		modificationTool("inspect", "read_file", map[string]string{"path": "main.go"}),
		modificationHandoff(),
		modificationTool("patch", "apply_patch", map[string]string{"patch": "--- main.go\n+++ main.go\n@@ -1,2 +1,2 @@\n package example\n-const value = 1\n+const value = 2\n"}),
	}, replies...)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if strings.Contains(string(body.Input), "你是 ReAct agent 的工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		index := len(requests)
		requests = append(requests, string(body.Input))
		if index >= len(outputs) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		writeObservationResponse(w, outputs[index])
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	durable := &state.DurableState{ExecutionID: "post-patch-verification", Goal: "change value and verify build"}
	result, _, err := process.Run(&config.AgentConfig{
		ExplorationBudget: policy,
		AllowTools:        map[config.McpToolName]struct{}{config.ApplyPatchMCPToolName: {}, config.ExecMCPToolName: {}, config.ReadFileMCPToolName: {}},
		LLMConfig:         &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
	}, durable)
	if err == nil && result.Status != CompletedReactStatus {
		t.Errorf("unexpected result: %+v", result)
	}
	return requests, durable, err
}

func TestReactPostPatchPromptRequestsStandaloneVerification(t *testing.T) {
	requests, _, err := runPostPatchVerification(t, [][]any{
		{observationMessage(`{"status":"completed","answer":"changed"}`)},
	})
	if err != nil || len(requests) != 4 {
		t.Fatalf("requests=%d err=%v", len(requests), err)
	}
	for _, want := range []string{"单独提交验证批次", "整批都不会执行", "choice=extend"} {
		if !strings.Contains(requests[3], want) {
			t.Errorf("post-patch request missing %q", want)
		}
	}
}

func TestReactRejectsMixedBatchAndAllowsOneStandaloneVerificationRetry(t *testing.T) {
	build := func(id string) []any {
		return modificationTool(id, "exec", map[string]any{"command": "go", "args": []string{"build", "main.go"}})
	}
	mixed := func(id string) []any {
		search := modificationTool(id+"-search", "exec", map[string]any{"command": "grep", "args": []string{"-R", "value", "."}})
		return append(search, build(id+"-build")...)
	}
	for _, tc := range []struct {
		name, final string
		retry       []any
		wantBuild   bool
	}{
		{"standalone build", `{"status":"completed","answer":"built successfully"}`, build("retry-build"), true},
		{"repeated mixed batch", `{"exploration":{"choice":"unable","reason":"investigation still needed","execution_state":"patched, build not run"}}`, mixed("repeat"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := [][]any{mixed("first"), tc.retry, {observationMessage(tc.final)}}
			if !tc.wantBuild {
				replies = append(replies, []any{observationMessage(`{"status":"failed","answer":"could not verify"}`)})
			}
			requests, durable, err := runPostPatchVerification(t, replies)
			if len(requests) < 5 || !strings.Contains(requests[4], "本批未执行") || !strings.Contains(requests[4], "可单独重试一次验证") {
				t.Fatalf("missing split-batch recovery feedback, requests=%d err=%v", len(requests), err)
			}
			if !tc.wantBuild {
				if len(requests) < 6 || !strings.Contains(requests[5], "exploration threshold requires classification and phase decision") {
					t.Fatalf("repeated mixed batch did not enter phase correction, requests=%d err=%v", len(requests), err)
				}
				if err == nil || !strings.Contains(err.Error(), "investigation still needed") || len(durable.ToolExecutions) != 2 {
					t.Fatalf("repeated mixed batch should require a phase decision, tools=%v err=%v", durable.ToolExecutions, err)
				}
				return
			}
			if err != nil || len(durable.ToolExecutions) != 3 {
				t.Fatalf("expected only the standalone build to execute, tools=%v err=%v", durable.ToolExecutions, err)
			}
			result := durable.ToolExecutions[2]
			if result.ToolCallId != "retry-build" || result.Status != "success" || !strings.Contains(result.Result, `"exit_code\":0`) {
				t.Fatalf("standalone build did not succeed: %+v", result)
			}
		})
	}
}
