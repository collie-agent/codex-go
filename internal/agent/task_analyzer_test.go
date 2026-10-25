package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
)

func TestTaskAnalyzerFewShotMatchesOutputSchema(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input string `json:"input"`
				}
				if err := decodeModelRequest(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				wantLine := schema == config.LineProtocolLLMOutputSchema
				if strings.Contains(request.Input, "# Task analyzer Line Protocol examples") != wantLine {
					t.Error("task analyzer few-shot does not match configured output schema")
				}
				text := `{"NeedsTools":false,"ExpectedSteps":1,"ReasoningSummary":"direct answer","CompletionCriteria":[]}`
				if wantLine {
					text = "BEGIN TASK_ANALYZER_RESULT\nNEEDSTOOLS=false\nEXPECTEDSTEPS=1\nREASONINGSUMMARY=\"direct answer\"\nEND TASK_ANALYZER_RESULT"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_analyzer", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
						map[string]any{"type": "output_text", "text": text},
					}}},
				})
			}))
			defer server.Close()
			cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{
				BaseUrl: server.URL, ApiKey: "test", Model: "analyzer", OutputSchema: schema,
			}}
			_, _, err := NewTaskAnalyzerProcess[TaskAnalyzerResult](context.Background()).Run(cfg, &fileContext.DurableState{RunId: "few-shot", Goal: "translate hello"})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTaskAnalyzerRunUsesItsResolvedConfiguration(t *testing.T) {
	var captured struct {
		model string
		input string
		tools []any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
			Input string `json:"input"`
			Tools []any  `json:"tools"`
		}
		if err := decodeModelRequest(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		captured.model, captured.input, captured.tools = request.Model, request.Input, request.Tools
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_analyzer", "status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
				map[string]any{"type": "output_text", "text": `{"NeedsTools":false,"ExpectedSteps":1,"ReasoningSummary":"direct answer","CompletionCriteria":[]}`},
			}}},
		})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "agents.yaml")
	body := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: base-model\n  sys_prompt: base-prompt\nagents:\n  task_analyzer:\n    llm:\n      base_url: " + server.URL + "\n      api_key: test\n      model: analyzer-model\n    sys_prompt: analyzer-prompt\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	state := &fileContext.DurableState{RunId: "analyzer-task", Goal: "translate hello"}
	process := NewTaskAnalyzerProcess[TaskAnalyzerResult](context.Background())
	process.Workspace = t.TempDir()
	if _, _, err := process.Run(loaded.ForAgent("task_analyzer"), state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(captured.input, "# Workspace\n\n> "+process.Workspace) || !strings.Contains(captured.input, "应判定需要读取文件（NeedsTools 为 true）") {
		t.Fatalf("analyzer omitted workspace or project analysis rule: %s", captured.input)
	}
	if captured.model != "analyzer-model" {
		t.Fatalf("analyzer used model %q", captured.model)
	}
	if !strings.Contains(captured.input, "任务分析器") || !strings.Contains(captured.input, "analyzer-prompt") || strings.Contains(captured.input, "base-prompt") {
		t.Fatalf("analyzer used wrong prompt: %s", captured.input)
	}
	if len(captured.tools) != 0 {
		t.Fatalf("analyzer exposed tools: %v", captured.tools)
	}

	// An omitted internal-agent block must reach the model with base settings.
	fallbackPath := filepath.Join(t.TempDir(), "fallback.yaml")
	fallbackBody := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: base-model\n  sys_prompt: base-prompt\n"
	if err := os.WriteFile(fallbackPath, []byte(fallbackBody), 0600); err != nil {
		t.Fatal(err)
	}
	fallback, err := config.LoadAgentConfig(fallbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewTaskAnalyzerProcess[TaskAnalyzerResult](context.Background()).Run(fallback.ForAgent("task_analyzer"), state); err != nil {
		t.Fatal(err)
	}
	if captured.model != "base-model" || !strings.Contains(captured.input, "base-prompt") {
		t.Fatalf("unconfigured analyzer did not use base settings: model=%q input=%q", captured.model, captured.input)
	}
}

// TestTaskAnalyzerRunIntegration 只有显式启用时才调用真实 LLM，避免普通 go test 产生 API 请求。
func TestTaskAnalyzerRunIntegration(t *testing.T) {
	if os.Getenv("RUN_TASK_ANALYZER_INTEGRATION") != "1" {
		t.Skip("set RUN_TASK_ANALYZER_INTEGRATION=1 to enable real LLM requests")
	}
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is required", name)
		}
	}

	for _, format := range []struct {
		name   string
		schema config.LLMOutputSchema
	}{
		{"json", config.JsonLLMOutputSchema},
		{"line_protocol", config.LineProtocolLLMOutputSchema},
	} {
		t.Run(format.name, func(t *testing.T) {
			for _, task := range []struct {
				name       string
				goal       string
				needsTools bool
			}{
				{"direct_answer", "将提供的英文 hello 翻译为中文，仅需根据这段文本直接回答。", false},
				{"multi_step_tools", "读取工作目录中的 README.md，将标题改为 Codex Go Agent，保存文件后重新读取，确认标题已更新且其他内容保持原样。", true},
			} {
				t.Run(task.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					defer cancel()
					runConfig := &config.AgentConfig{
						LLMConfig: &config.LLMConfig{
							ApiKey:       os.Getenv("OPENAI_API_KEY"),
							BaseUrl:      os.Getenv("OPENAI_BASE_URL"),
							Model:        os.Getenv("OPENAI_MODEL"),
							OutputSchema: format.schema,
						},
						SysPrompt: "使用中文提供简短的 ReasoningSummary 和 CompletionCriteria。",
					}
					state := &fileContext.DurableState{
						RunId: "task-analyzer-integration-" + format.name + "-" + task.name,
						Goal:  task.goal,
					}
					var processor AgentProcessor[TaskAnalyzerResult] = NewTaskAnalyzerProcess[TaskAnalyzerResult](ctx)
					got, _, err := processor.Run(runConfig, state)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("decoded result: %+v", got)
					if got.NeedsTools != task.needsTools {
						t.Fatalf("NeedsTools = %v, want %v", got.NeedsTools, task.needsTools)
					}
					if strings.TrimSpace(got.ReasoningSummary) == "" {
						t.Fatal("ReasoningSummary must not be empty")
					}
					// 步骤估算和措辞允许变化，只断言任务分类及完成标准的业务约束。
					if task.needsTools {
						if got.ExpectedSteps <= 1 || len(got.CompletionCriteria) == 0 {
							t.Fatalf("multi-step tool task requires ExpectedSteps > 1 and CompletionCriteria: %+v", got)
						}
						for _, criterion := range got.CompletionCriteria {
							if strings.TrimSpace(criterion) == "" {
								t.Fatal("CompletionCriteria contains an empty item")
							}
						}
					} else if got.ExpectedSteps != 1 || len(got.CompletionCriteria) != 0 {
						t.Fatalf("direct answer requires ExpectedSteps = 1 and empty CompletionCriteria: %+v", got)
					}
					if state.Goal != task.goal || len(state.CompletionCriteria) != 0 {
						t.Fatalf("task analyzer changed durable state: %+v", state)
					}
				})
			}
		})
	}
}
