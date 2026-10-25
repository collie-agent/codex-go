package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
)

func plannerTestConfig(baseURL string, schema config.LLMOutputSchema) *config.AgentConfig {
	return &config.AgentConfig{
		LLMConfig: &config.LLMConfig{ApiKey: "test", BaseUrl: baseURL, Model: "planner-model", OutputSchema: schema},
		SysPrompt: "使用中文描述计划。",
	}
}

func TestPlannerSelectsCurrentStep(t *testing.T) {
	for _, tt := range []struct {
		name     string
		statuses []ExecutionStatus
		want     int
		wantNext AgentProcessorType
	}{
		{"first pending", []ExecutionStatus{PendingExecutionStatus, PendingExecutionStatus}, 0, ReactAgentType},
		{"resume old plan", []ExecutionStatus{CompletedExecutionStatus, PendingExecutionStatus}, 1, ReactAgentType},
		{"completed plan", []ExecutionStatus{CompletedExecutionStatus, CompletedExecutionStatus}, -1, EndAgentType},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := &fileContext.DurableState{Goal: "test"}
			for _, status := range tt.statuses {
				state.StepExecutions = append(state.StepExecutions, StepExecution{Status: status})
			}
			_, next, err := NewPlannerProcess(context.Background()).Run(plannerTestConfig("http://unused.invalid", config.JsonLLMOutputSchema), state)
			if err != nil || state.CurrentStepIndex != tt.want || !slices.Equal(next, []AgentProcessorType{tt.wantNext}) {
				t.Fatalf("Run() index=%d next=%v err=%v", state.CurrentStepIndex, next, err)
			}
		})
	}
}

// 通过本地 Responses API 走完整请求和解码链路，无需真实 API 凭证。
func plannerTestServer(t *testing.T, text string, toolCall bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		output := []any{map[string]any{
			"type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}}
		if toolCall {
			output = append(output, map[string]any{
				"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"README.md"}`,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"id": "resp_planner", "status": "completed", "output": output}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestPlannerRun(t *testing.T) {
	want := PlannerResult{StepExecutions: []StepExecution{
		{Revision: 1, StepId: "inspect", Status: PendingExecutionStatus, Intent: "读取 README.md，确认当前标题。", CompletionCriteria: "当前标题已确认"},
		{Revision: 1, StepId: "update", Status: PendingExecutionStatus, Intent: "修改标题并重新读取，确认其他内容未变。", CompletionCriteria: "标题已更新且其他内容未变"},
	}}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		schema config.LLMOutputSchema
		text   string
	}{
		{"json", config.JsonLLMOutputSchema, string(encoded)},
		{"line_protocol", config.LineProtocolLLMOutputSchema, `BEGIN PLAN
BEGIN STEPEXECUTIONS
REVISION=1
STEPID="inspect"
STATUS="pending"
INTENT="读取 README.md，确认当前标题。"
COMPLETIONCRITERIA="当前标题已确认"
END STEPEXECUTIONS
BEGIN STEPEXECUTIONS
REVISION=1
STEPID="update"
STATUS="pending"
INTENT="修改标题并重新读取，确认其他内容未变。"
COMPLETIONCRITERIA="标题已更新且其他内容未变"
END STEPEXECUTIONS
END PLAN`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := plannerTestServer(t, tt.text, false)
			// 包装处理器检查出站请求，确保 planner 的配置、任务及无工具约束到达 API。
			handler := server.Config.Handler
			requests := make(chan struct{}, 1)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model string `json:"model"`
					Input string `json:"input"`
					Tools []any  `json:"tools"`
					Text  struct {
						Format struct {
							Type   string `json:"type"`
							Schema struct {
								Properties map[string]json.RawMessage `json:"properties"`
							} `json:"schema"`
						} `json:"format"`
					} `json:"text"`
				}
				if err := decodeModelRequest(r.Body, &request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- struct{}{}
				if r.Method != http.MethodPost || r.URL.Path != "/responses" || request.Model != "planner-model" || len(request.Tools) != 0 {
					t.Errorf("unexpected planner request: %s %s model=%q tools=%v", r.Method, r.URL.Path, request.Model, request.Tools)
				}
				for _, fragment := range []string{"plan-execute", "使用中文描述计划。", "> planner-task", "> 修改 README.md 标题\n> 保留其他内容"} {
					if !strings.Contains(request.Input, fragment) {
						t.Errorf("planner input missing %q: %s", fragment, request.Input)
					}
				}
				if tt.schema == config.JsonLLMOutputSchema {
					if request.Text.Format.Type != "json_schema" || len(request.Text.Format.Schema.Properties) != 1 || request.Text.Format.Schema.Properties["StepExecutions"] == nil {
						t.Errorf("unexpected planner output schema: %+v", request.Text.Format)
					}
				} else if request.Text.Format.Type != "" {
					t.Errorf("line protocol sent JSON format: %+v", request.Text.Format)
				}
				handler.ServeHTTP(w, r)
			}))
			defer proxy.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			state := &fileContext.DurableState{RunId: "planner-task", Goal: "修改 README.md 标题\n保留其他内容", CompletionCriteria: []string{"标题已更新"}}
			before, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			got, next, err := NewPlannerProcess(ctx).Run(plannerTestConfig(proxy.URL, tt.schema), state)
			if err != nil || !reflect.DeepEqual(got, want) || !slices.Equal(next, []AgentProcessorType{ReactAgentType}) {
				t.Fatalf("Run() = %+v, %v, %v; want %+v and ReAct", got, next, err, want)
			}
			after, err := json.Marshal(state)
			if err != nil || string(before) != string(after) {
				t.Fatalf("planner changed durable state: before=%s after=%s err=%v", before, after, err)
			}
			if len(requests) != 1 {
				t.Fatalf("got %d requests, want 1", len(requests))
			}
		})
	}
}

func TestPlannerRunValidation(t *testing.T) {
	p := NewPlannerProcess(context.Background())
	cfg := plannerTestConfig("http://unused.invalid", config.JsonLLMOutputSchema)
	state := &fileContext.DurableState{Goal: "test"}
	for _, tt := range []struct {
		name  string
		p     *PlannerProcess
		cfg   *config.AgentConfig
		state *fileContext.DurableState
		want  string
	}{
		{"nil planner", nil, cfg, state, "context is required"},
		{"nil context", NewPlannerProcess(nil), cfg, state, "context is required"},
		{"nil config", p, nil, state, "agent config with LLM config is required"},
		{"nil LLM config", p, &config.AgentConfig{}, state, "agent config with LLM config is required"},
		{"nil state", p, cfg, nil, "durable state with goal is required"},
		{"blank goal", p, cfg, &fileContext.DurableState{Goal: " \n\t"}, "durable state with goal is required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, next, err := tt.p.Run(tt.cfg, tt.state)
			if err == nil || !strings.Contains(err.Error(), tt.want) || got.StepExecutions != nil || next != nil {
				t.Fatalf("Run() = %+v, %v, %v; want zero result and error containing %q", got, next, err, tt.want)
			}
		})
	}
}

func TestPlannerRunRejectsInvalidResponses(t *testing.T) {
	valid := StepExecution{Revision: 1, StepId: "inspect", Status: PendingExecutionStatus, Intent: "读取文件", CompletionCriteria: "文件已读取", CompletionEvidence: []string{}}
	for _, tt := range []struct {
		name   string
		change func(*StepExecution)
		want   string
	}{
		{"blank ID", func(s *StepExecution) { s.StepId = " \t" }, "planner step 1 requires"},
		{"blank intent", func(s *StepExecution) { s.Intent = " \n" }, "planner step 1 requires"},
		{"empty criteria", func(s *StepExecution) { s.CompletionCriteria = "" }, "requires non-empty CompletionCriteria"},
		{"blank criteria", func(s *StepExecution) { s.CompletionCriteria = " \n\t" }, "requires non-empty CompletionCriteria"},
		{"wrong revision", func(s *StepExecution) { s.Revision = 2 }, "planner step 1 requires"},
		{"running status", func(s *StepExecution) { s.Status = RunningExecutionStatus }, "planner step 1 requires"},
		{"completed status", func(s *StepExecution) { s.Status = CompletedExecutionStatus }, "planner step 1 requires"},
		{"failed status", func(s *StepExecution) { s.Status = FailedExecutionStatus }, "planner step 1 requires"},
		{"missing status", func(s *StepExecution) { s.Status = "" }, "planner step 1 requires"},
		{"premature observation", func(s *StepExecution) { s.Observation = []string{"done"} }, "planner step 1 requires"},
		{"premature evidence", func(s *StepExecution) { s.CompletionEvidence = []string{"done"} }, "planner step 1 requires"},
		// ID 按去除空白后的值判重，防止看似不同的 ID 指向同一步骤。
		{"duplicate ID", func(s *StepExecution) { s.StepId = " inspect " }, `duplicate StepId "inspect"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			step := valid
			tt.change(&step)
			steps := []StepExecution{step}
			if tt.name == "duplicate ID" {
				steps = append([]StepExecution{valid}, step)
			}
			text, err := json.Marshal(PlannerResult{StepExecutions: steps})
			if err != nil {
				t.Fatal(err)
			}
			server := plannerTestServer(t, string(text), false)
			assertPlannerResponseError(t, server.URL, tt.want)
		})
	}
	for _, tt := range []struct {
		name, text, want string
		toolCall         bool
	}{
		{"empty steps", `{"StepExecutions":[]}`, "requires at least one step", false},
		{"missing steps", `{}`, "requires at least one step", false},
		{"malformed JSON", `{`, "parse response output", false},
		{"empty output", "", "response output is empty", false},
		{"tool call", "", "returned unexpected tool calls", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := plannerTestServer(t, tt.text, tt.toolCall)
			assertPlannerResponseError(t, server.URL, tt.want)
		})
	}
}

func assertPlannerResponseError(t *testing.T, baseURL, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, next, err := NewPlannerProcess(ctx).Run(plannerTestConfig(baseURL, config.JsonLLMOutputSchema), &fileContext.DurableState{RunId: "planner-task", Goal: "test"})
	if err == nil || !strings.Contains(err.Error(), "task planner-task: planner") || !strings.Contains(err.Error(), want) || got.StepExecutions != nil || next != nil {
		t.Fatalf("Run() = %+v, %v, %v; want zero result and error containing %q", got, next, err, want)
	}
}

func TestPlannerRunProviderErrors(t *testing.T) {
	t.Run("HTTP error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid planner request","type":"invalid_request_error"}}`))
		}))
		defer server.Close()
		assertPlannerResponseError(t, server.URL, "400 Bad Request")
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, next, err := NewPlannerProcess(ctx).Run(plannerTestConfig("http://unused.invalid", config.JsonLLMOutputSchema), &fileContext.DurableState{RunId: "planner-task", Goal: "test"})
		if !errors.Is(err, context.Canceled) || got.StepExecutions != nil || next != nil {
			t.Fatalf("Run() = %+v, %v, %v; want context.Canceled", got, next, err)
		}
	})
}

// 仅显式启用时调用真实 LLM，普通 go test 使用上面的本地服务测试。
func TestPlannerRunIntegration(t *testing.T) {
	if os.Getenv("RUN_PLANNER_INTEGRATION") != "1" {
		t.Skip("set RUN_PLANNER_INTEGRATION=1 to enable real LLM requests")
	}
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is required", name)
		}
	}
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cfg := plannerTestConfig(os.Getenv("OPENAI_BASE_URL"), schema)
			cfg.LLMConfig.ApiKey = os.Getenv("OPENAI_API_KEY")
			cfg.LLMConfig.Model = os.Getenv("OPENAI_MODEL")
			state := &fileContext.DurableState{RunId: "planner-integration", Goal: "读取工作目录中的 README.md，将标题改为 Codex Go Agent，保存后重新读取，确认标题已更新且其他内容保持原样。"}
			got, next, err := NewPlannerProcess(ctx).Run(cfg, state)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.StepExecutions) == 0 || !slices.Equal(next, []AgentProcessorType{ReactAgentType}) {
				t.Fatalf("unexpected plan=%+v next=%v", got, next)
			}
			t.Logf("decoded plan: %+v", got)
		})
	}
}
