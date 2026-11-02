package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestMultiAgentReactUsesDelegationAndResolvedAgentConfig(t *testing.T) {
	const delegation = `{"caller":"supervisor","next_agents":["supervisor"],"delegated_task":{"context":"inspect the assigned source"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input string
			Model string
			Tools []struct{ Name string }
		}
		if err := decodeModelRequest(r.Body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		for _, want := range []string{delegation, "work-only", "GenerateEntryCompletionCriteria", "MultiAgentResult"} {
			if !strings.Contains(request.Input, want) {
				t.Errorf("model input missing %q: %s", want, request.Input)
			}
		}
		if strings.Contains(request.Input, "supervise-only") || request.Model != "worker-model" {
			t.Errorf("resolved worker config replaced: model=%q input=%s", request.Model, request.Input)
		}
		hasLoadSkill := false
		for _, tool := range request.Tools {
			if tool.Name == string(config.SwitchToPlanExecuteMCPToolName) {
				t.Error("multi-agent exposed plan switch")
			}
			hasLoadSkill = hasLoadSkill || tool.Name == string(config.LoadSkillMCPToolName)
		}
		if !hasLoadSkill {
			t.Error("built-in skill loader missing")
		}
		multiResponse(w, `{"result":{"status":"completed","answer":"ready to report"},"next":[{"agent":"supervisor","task":"report"}],"entry_completion_criteria":["source verified"]}`)
	}))
	defer server.Close()
	cfg := multiConfig(t, server.URL).ForAgent("worker")
	cfg.LLMConfig.Model = "worker-model"
	cfg.LLMConfig.OutputSchema = config.JsonLLMOutputSchema
	cfg.AllowTools[config.SwitchToPlanExecuteMCPToolName] = struct{}{}
	process, err := NewMultiAgentReactProcess(t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetDelegationContext(delegation, true)
	result, next, err := process.Run(cfg, &state.DurableState{RunId: "worker-run", Goal: "inspect source"})
	if err != nil || result.Result.Answer != "ready to report" || len(result.Next) != 1 || result.Next[0].Agent != "supervisor" || len(result.EntryCompletionCriteria) != 1 || result.EntryCompletionCriteria[0] != "source verified" || len(next) != 1 || next[0] != EndAgentType {
		t.Fatalf("Run() = %+v, %v, %v", result, next, err)
	}
	if _, ok := cfg.AllowTools[config.LoadSkillMCPToolName]; ok {
		t.Error("skill permission leaked into caller config")
	}
	if _, ok := cfg.AllowTools[config.SwitchToPlanExecuteMCPToolName]; !ok {
		t.Error("caller tool permissions changed")
	}
}

func TestMultiAgentReactExecutesToolsThroughMCPCommand(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex-mcp")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp").CombinedOutput(); err != nil {
		t.Fatalf("build MCP: %v\n%s", err, output)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "evidence.txt"), []byte("source verified"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		switch calls.Add(1) {
		case 1:
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "call_id": "read-source", "name": "read_file", "arguments": `{"path":"evidence.txt"}`}})
		case 2:
			hasEvidence := false
			for _, item := range request.Input {
				if item.Type == "function_call_output" && item.CallID == "read-source" && strings.Contains(item.Output, "source verified") {
					hasEvidence = true
				}
			}
			if !hasEvidence {
				t.Errorf("model request omitted tool evidence: %+v", request.Input)
			}
			multiResponse(w, `{"result":{"status":"completed","answer":"source verified"},"next":[]}`)
		default:
			t.Error("unexpected model request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	process, err := NewMultiAgentReactProcessWithCommand(t.Context(), exec.CommandContext(t.Context(), binary, "--workspace", workspace))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := 0
	process.SetCheckpoint(func() error { checkpoints++; return nil })
	cfg := multiConfig(t, server.URL).ForAgent("worker")
	cfg.LLMConfig.OutputSchema = config.JsonLLMOutputSchema
	cfg.AllowTools[config.ReadFileMCPToolName] = struct{}{}
	result, _, err := process.Run(cfg, &state.DurableState{RunId: "worker-tools", Goal: "read evidence.txt"})
	// Persist the execution identity, pending tool call, confirmed tool result,
	// and final execution result so each recovery boundary is durable.
	if err != nil || result.Result.Answer != "source verified" || checkpoints != 4 {
		t.Fatalf("Run() = %+v, %v; checkpoints=%d", result, err, checkpoints)
	}
}
