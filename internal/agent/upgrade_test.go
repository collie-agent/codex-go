package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactSwitchToPlanExecute(t *testing.T) {
	for _, failCheckpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "checkpoint failure"}[failCheckpoint], func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Input json.RawMessage
					Tools []struct{ Name string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				calls++
				hasSwitch := false
				for _, tool := range req.Tools {
					if tool.Name == "switch_to_plan_execute" {
						hasSwitch = true
					}
				}
				if hasSwitch != (calls == 1) {
					t.Errorf("request %d switch tool exposed=%v", calls, hasSwitch)
				}
				if calls == 1 {
					writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_switch", "call_id": "call_switch", "name": "switch_to_plan_execute", "arguments": `{"reason":"dependent implementation and verification","summary":"Verified API contract; implementation and tests remain"}`}})
					return
				}
				if !strings.Contains(string(req.Input), "Verified API contract") {
					t.Error("handoff missing from planner/executor")
				}
				text := `{"StepExecutions":[{"Revision":1,"StepId":"implement","Status":"pending","Intent":"implement and verify remaining changes","CompletionCriteria":"tests pass","CompletionEvidence":[],"Observation":[]}]}`
				if calls == 3 {
					text = `{"status":"completed","answer":"done","Observation":["implemented"],"CompletionEvidence":["tests pass"]}`
				}
				writeObservationResponse(w, []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}})
			}))
			defer server.Close()
			cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
			o := NewProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err := o.SetMode("react"); err != nil {
				t.Fatal(err)
			}
			upgraded := false
			sentinel := errors.New("checkpoint unavailable")
			o.SetCheckpoint(func(s *state.DurableState, mode string) error {
				if mode == "plan-execute" {
					if s.PlanningContext == "" {
						t.Error("upgrade persisted without handoff")
					}
					upgraded = true
					if failCheckpoint {
						return sentinel
					}
				} else if upgraded {
					t.Error("mode downgraded after switch")
				}
				return nil
			})
			durable := &state.DurableState{CompletionCriteria: []string{"original task verified"}, Goal: "implement feature", RunId: "switch-test"}
			results, err := o.Run(cfg, durable)
			if failCheckpoint {
				if !errors.Is(err, sentinel) || calls != 1 || durable.PlanningContext == "" {
					t.Fatalf("results=%v err=%v calls=%d state=%+v", results, err, calls, durable)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls != 3 || !upgraded || len(results) != 1 || results[0].Answer != "done" || durable.StepExecutions[0].Status != CompletedExecutionStatus {
				t.Fatalf("calls=%d upgraded=%v results=%+v state=%+v", calls, upgraded, results, durable)
			}
			if _, ok := cfg.AllowTools[config.SwitchToPlanExecuteMCPToolName]; ok {
				t.Error("caller configuration mutated")
			}
		})
	}
}

func TestWorkerRejectsMixedSwitchBatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeObservationResponse(w, []any{
			map[string]any{"type": "function_call", "id": "fc_switch", "call_id": "switch", "name": "switch_to_plan_execute", "arguments": `{"reason":"stages","summary":"remaining work"}`},
			map[string]any{"type": "function_call", "id": "fc_patch", "call_id": "patch", "name": "apply_patch", "arguments": `{"patch":"unused"}`},
		})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer process.mcpClientSess.Close()
	defer process.serverSess.Close()
	cfg := &config.AgentConfig{
		LLMConfig:  &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
		AllowTools: map[config.McpToolName]struct{}{config.SwitchToPlanExecuteMCPToolName: {}, config.ApplyPatchMCPToolName: {}},
	}
	worker := NewWorker[ReactResult](t.Context(), cfg, process.mcpClientSess)
	_, _, calls, err := worker.Run(state.NewReactContext(&state.DurableState{Goal: "implement feature"}))
	var invalid *toolCallError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "must be called alone") || len(calls) != 0 {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
}
