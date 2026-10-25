package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestOrchestratorModeRouting(t *testing.T) {
	for _, tt := range []struct {
		mode, configName string
		wantModels       []string
	}{
		{"react", "react", []string{"react"}},
		{"react", "plan-execute", []string{"react"}},
		{"plan-execute", "react", []string{"plan", "react", "react"}},
		{"plan-execute", "plan-execute", []string{"plan", "react", "react"}},
		{"", "react", []string{"react"}},
		{"", "plan-execute", []string{"react"}},
	} {
		t.Run(tt.mode+"/"+tt.configName, func(t *testing.T) {
			var logs bytes.Buffer
			writer := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(writer)
			models := make(chan string, 10)
			reactCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Model, Input string }
				if err := decodeModelRequest(r.Body, &req); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				models <- req.Model
				text := `{"status":"completed","answer":"done"}`
				switch req.Model {
				case "plan":
					text = `{"StepExecutions":[{"Revision":1,"StepId":"inspect","Status":"pending","Intent":"inspect the question","CompletionCriteria":"question inspected","CompletionEvidence":[]},{"Revision":1,"StepId":"answer","Status":"pending","Intent":"answer the question","CompletionCriteria":"question answered","CompletionEvidence":[]}]}`
				case "react":
					if len(tt.wantModels) > 2 {
						text = `{"status":"completed","answer":"done","observation":["observed step result"],"CompletionEvidence":["step criteria verified"]}`
						if !strings.Contains(req.Input, "return these fields for the current plan step only:") {
							t.Error("executor missing structured step result instructions")
						}
						// 后续步骤必须能看到前一步的观察和验收证据。
						if reactCalls > 0 && (!strings.Contains(req.Input, "observed step result") || !strings.Contains(req.Input, "step criteria verified")) {
							t.Error("executor missing previous step results")
						}
						wantIntent := []string{"inspect the question", "answer the question"}[reactCalls]
						if !strings.Contains(req.Input, "## CurrentStep\n\n> "+wantIntent) {
							t.Errorf("executor missing current step %q: %s", wantIntent, req.Input)
						}
					}
					reactCalls++
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_mode", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
						map[string]any{"type": "output_text", "text": text},
					}}},
				})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent.yaml")
			yaml := "name: " + tt.configName + "\nlog_level: error\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: react\nagents:\n"
			for _, model := range []string{"plan"} {
				yaml += "  " + model + ":\n    llm:\n      base_url: " + server.URL + "\n      api_key: test\n      model: " + model + "\n"
			}
			if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadAgentConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			o := NewProcessorOrchestrator(ctx, filemcp.Config{Workspace: t.TempDir()})
			if err := o.SetMode(tt.mode); err != nil {
				t.Fatal(err)
			}
			state := &fileContext.DurableState{RunId: "mode-test", Goal: "answer the question"}
			results, err := o.Run(cfg, state)
			if err != nil || len(results) != 1 || results[0].Answer != "done" {
				t.Fatalf("Run() = %+v, %v", results, err)
			}
			close(models)
			var got []string
			for model := range models {
				got = append(got, model)
			}
			// 检查实际模型调用顺序，确保模式选择使用正确的代理配置。
			if !slices.Equal(got, tt.wantModels) {
				t.Fatalf("models = %v; want %v", got, tt.wantModels)
			}
			if len(tt.wantModels) > 2 {
				for _, step := range state.StepExecutions {
					if !slices.Equal(step.Observation, []string{"observed step result"}) || !slices.Equal(step.CompletionEvidence, []string{"step criteria verified"}) {
						t.Fatalf("step results not persisted: %+v", step)
					}
				}
				if !slices.Equal(results[0].Observation, []string{"observed step result"}) || !slices.Equal(results[0].CompletionEvidence, []string{"step criteria verified"}) {
					t.Fatalf("structured result not returned: %+v", results)
				}
				if len(state.StepExecutions) != 2 || state.CurrentStepIndex != -1 || state.StepExecutions[0].Status != CompletedExecutionStatus || state.StepExecutions[1].Status != CompletedExecutionStatus {
					t.Fatalf("plan not completed: %+v", state)
				}
			}
		})
	}
}

func TestOrchestratorRejectsInvalidMode(t *testing.T) {
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{})
	if err := o.SetMode("unknown"); err == nil {
		t.Fatal("expected invalid mode error")
	}
	if o.rootAgentType != ReactAgentType {
		t.Fatal("invalid mode changed the entry processor")
	}
}

func TestPlanOrchestratorContinuesSavedPendingStepWithoutReplanning(t *testing.T) {
	models := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model, Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		models <- req.Model
		if req.Model != "executor" || !strings.Contains(req.Input, "## CurrentStep\n\n> finish report") {
			t.Errorf("unexpected recovery request: model=%q input=%s", req.Model, req.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resume", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": `{"status":"completed","answer":"resumed","observation":["report saved"],"CompletionEvidence":["report checked"]}`}},
			}},
		})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	yaml := "name: plan-execute\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: executor\nagents:\n  plan:\n    llm:\n      base_url: " + server.URL + "\n      api_key: test\n      model: planner\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	state := &fileContext.DurableState{RunId: "saved-run", Goal: "inspect and report", CompletionCriteria: []string{"report checked"}, StepExecutions: []StepExecution{
		{Revision: 3, StepId: "inspect", Position: 0, Status: CompletedExecutionStatus, Intent: "inspect source", CompletionCriteria: "source inspected", Observation: []string{"source read"}, CompletionEvidence: []string{"read verified"}},
		{Revision: 2, StepId: "report", Position: 1, Status: PendingExecutionStatus, Intent: "finish report", CompletionCriteria: "report checked"},
	}}
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	if err := o.SetMode("plan-execute"); err != nil {
		t.Fatal(err)
	}
	var savedRunning, savedResult bool
	o.SetCheckpoint(func(snapshot *fileContext.DurableState, _ string) error {
		if step := snapshot.CurrentStep(); step != nil && step.Status == RunningExecutionStatus {
			savedRunning = true
		}
		if snapshot.Result != nil {
			savedResult = true
		}
		return nil
	})
	results, err := o.Run(cfg, state)
	if err != nil || len(results) != 1 || results[0].Answer != "resumed" {
		t.Fatalf("Run() = %+v, %v", results, err)
	}
	if len(models) != 1 || <-models != "executor" {
		t.Fatal("resume must execute only the saved pending step without calling planner")
	}
	if !savedRunning || !savedResult || state.StepExecutions[0].Observation[0] != "source read" || state.StepExecutions[0].Revision != 3 || state.StepExecutions[1].Revision != 2 {
		t.Fatalf("recovery lost checkpoint data: savedRunning=%v savedResult=%v state=%+v", savedRunning, savedResult, state)
	}
}

func TestPlanFailureCheckpointIncludesRunError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "failed-step", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": `{"status":"failed","answer":"permission denied"}`}},
			}},
		})
	}))
	defer server.Close()
	cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{ApiKey: "test", BaseUrl: server.URL, Model: "executor", OutputSchema: config.JsonLLMOutputSchema}}
	state := &fileContext.DurableState{
		RunId: "failed-run", Goal: "inspect protected file", CurrentStepIndex: 0,
		StepExecutions: []fileContext.StepExecution{{
			StepId: "read", Status: PendingExecutionStatus, Intent: "read the file",
			CompletionCriteria: "file content available", Revision: 1,
		}},
	}
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	if err := o.SetMode("plan-execute"); err != nil {
		t.Fatal(err)
	}
	var failedCheckpoint *fileContext.DurableState
	o.SetCheckpoint(func(snapshot *fileContext.DurableState, _ string) error {
		if step := snapshot.CurrentStep(); step != nil && step.Status == FailedExecutionStatus {
			failedCheckpoint = snapshot
		}
		return nil
	})
	if _, err := o.Run(cfg, state); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Run() error = %v, want permission denied", err)
	}
	if failedCheckpoint == nil || failedCheckpoint.Error == "" || failedCheckpoint.CurrentStep().Status != FailedExecutionStatus {
		t.Fatalf("failure checkpoint did not include the step and run error: %+v", failedCheckpoint)
	}
}

func TestPlanCheckpointFailureStopsStepScheduling(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "plan", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": `{"StepExecutions":[{"Revision":1,"StepId":"inspect","Status":"pending","Intent":"inspect files","CompletionCriteria":"files inspected","CompletionEvidence":[],"Observation":[]}]}`}},
			}},
		})
	}))
	defer server.Close()
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: t.TempDir()})
	if err := o.SetMode("plan-execute"); err != nil {
		t.Fatal(err)
	}
	o.SetCheckpoint(func(snapshot *fileContext.DurableState, _ string) error {
		if len(snapshot.StepExecutions) > 0 {
			return errors.New("plan checkpoint rejected")
		}
		return nil
	})
	_, err := o.Run(plannerTestConfig(server.URL, config.JsonLLMOutputSchema), &fileContext.DurableState{RunId: "plan-write-failure", Goal: "inspect files"})
	if err == nil || !strings.Contains(err.Error(), "plan checkpoint rejected") {
		t.Fatalf("Run() error = %v, want plan checkpoint failure", err)
	}
	if requests != 1 {
		t.Fatalf("model requests=%d; no step executor should run after plan persistence failed", requests)
	}
}
