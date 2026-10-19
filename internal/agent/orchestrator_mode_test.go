package agent

import (
	"context"
	"encoding/json"
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
		{"", "react", []string{"task_analyzer", "react"}},
		{"", "plan-execute", []string{"task_analyzer", "plan", "react", "react"}},
	} {
		t.Run(tt.mode+"/"+tt.configName, func(t *testing.T) {
			models := make(chan string, 10)
			reactCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Model, Input string }
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				models <- req.Model
				text := `{"status":"completed","answer":"done"}`
				switch req.Model {
				case "task_analyzer":
					text = `{"NeedsTools":false,"ExpectedSteps":1,"ReasoningSummary":"simple task","CompletionCriteria":[]}`
				case "plan":
					text = `{"StepExecutions":[{"Revision":1,"StepId":"inspect","Status":"pending","Intent":"inspect the question","CompletionCriteria":"question inspected","CompletionEvidence":[]},{"Revision":1,"StepId":"answer","Status":"pending","Intent":"answer the question","CompletionCriteria":"question answered","CompletionEvidence":[]}]}`
				case "react":
					if len(tt.wantModels) > 2 {
						text = `{"status":"completed","answer":"done","observation":["observed step result"],"CompletionEvidence":["step criteria verified"]}`
						if !strings.Contains(req.Input, stepReactResultInstructions) {
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
			yaml := "name: " + tt.configName + "\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: react\nagents:\n"
			for _, model := range []string{"task_analyzer", "plan"} {
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
			// 检查实际模型调用顺序，确保显式模式没有调用分析器或错误的代理配置。
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
	if o.rootAgentType != TaskAnalyzerAgentType {
		t.Fatal("invalid mode changed the entry processor")
	}
}
