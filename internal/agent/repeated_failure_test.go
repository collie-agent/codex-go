package agent_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactBlocksUnchangedFailedCommand(t *testing.T) {
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		if turn <= 2 {
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turn), "call_id": fmt.Sprintf("call_%d", turn), "name": "exec", "arguments": `{"command":"sed","args":["-n","1p","missing.txt"]}`})
			return
		}
		var req struct {
			Input []struct {
				Type, Output string
				CallID       string `json:"call_id"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		found := false
		for _, item := range req.Input {
			if item.Type == "function_call_output" && item.CallID == "call_2" && strings.Contains(item.Output, "failed command") && strings.Contains(item.Output, "repair") && strings.Contains(item.Output, "missing.txt") {
				found = true
			}
		}
		if !found {
			t.Error("model did not receive actionable repeated failure feedback")
		}
		criteriaResponse(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "failure", Goal: "verify", CompletionCriteria: []string{"verified"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	if _, _, err := process.Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
	if len(durable.ToolExecutions) != 1 {
		t.Fatalf("actual executions=%d, want 1", len(durable.ToolExecutions))
	}
}

func TestReactAllowsFailedCommandAfterRepair(t *testing.T) {
	workspace := t.TempDir()
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
			found := false
			for _, item := range req.Input {
				if item.CallID == "repair_3" && item.Type == "function_call_output" && strings.Contains(item.Output, "fixed") {
					found = true
				}
			}
			if !found {
				t.Error("same command did not observe repaired source")
			}
			criteriaResponse(w, `{"status":"completed","answer":"done"}`)
			return
		}
		name, args := "exec", `{"command":"sed","args":["-n","1p","missing.txt"]}`
		if turn == 2 {
			name, args = "apply_patch", `{"patch":"--- /dev/null\n+++ missing.txt\n@@ -0,0 +1 @@\n+fixed\n"}`
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_repair_%d", turn), "call_id": fmt.Sprintf("repair_%d", turn), "name": name, "arguments": args})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "repair", Goal: "repair", CompletionCriteria: []string{"verified"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	if _, _, err := process.Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
}

func TestReactRestoredFailureStillPreventsBlindRetry(t *testing.T) {
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		if turn == 1 {
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": "fc_new", "call_id": "new", "name": "exec", "arguments": `{"command":"sed","args":["-n","1p","missing.txt"]}`})
			return
		}
		var req struct {
			Input []struct {
				Type, Output string
				CallID       string `json:"call_id"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		found := false
		for _, item := range req.Input {
			if item.Type == "function_call_output" && item.CallID == "new" && strings.Contains(item.Output, "failed command") && strings.Contains(item.Output, "No such file") {
				found = true
			}
		}
		if !found {
			t.Error("restored failure was blindly retried")
		}
		criteriaResponse(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "restore-failure", ExecutionID: "instance", Goal: "verify", CompletionCriteria: []string{"verified"}, ToolExecutions: []*state.ToolResult{{Batch: 1, ToolName: "exec", ToolCallId: "saved", Arguments: `{"command":"sed","args":["-n","1p","missing.txt"]}`, Status: filemcp.ToolCallSuceessStatus, Result: `[{"type":"text","text":"{\"exit_code\":2,\"stderr\":\"missing.txt: No such file or directory\",\"stdout\":\"\"}"}]`}}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	if _, _, err := process.Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
	if len(durable.ToolExecutions) != 1 {
		t.Errorf("executions=%d, want 1", len(durable.ToolExecutions))
	}
}

func TestReactBoundsTransientCommandRetries(t *testing.T) {
	workspace := t.TempDir()
	turn := 0
	script := `from pathlib import Path
import sys
p=Path('attempts')
n=int(p.read_text())+1 if p.exists() else 1
p.write_text(str(n))
print('connection reset by peer, attempt '+str(n),file=sys.stderr)
sys.exit(1)`
	args, _ := json.Marshal(map[string]any{"command": "python3", "args": []string{"-c", script}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.Input) > 0 && strings.Contains(req.Input[0].Content, "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		turn++
		if turn <= 3 {
			criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_transient_%d", turn), "call_id": fmt.Sprintf("transient_%d", turn), "name": "exec", "arguments": string(args)})
			return
		}
		criteriaResponse(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "transient", Goal: "verify", CompletionCriteria: []string{"verified"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	if _, _, err := process.Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
	if len(durable.ToolExecutions) != 2 {
		t.Errorf("executions=%d, want initial attempt plus one retry", len(durable.ToolExecutions))
	}
}
