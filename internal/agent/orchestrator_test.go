package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestOrchestratorWithCommand(t *testing.T) {
	command := filepath.Join(t.TempDir(), "missing-mcp")
	args := []string{"--workspace", t.TempDir(), "--timeout", "30s"}
	o := NewProcessorOrchestratorWithCommand(context.Background(), command, args...)
	if o.rootAgentType != TaskAnalyzerAgentType {
		t.Fatalf("rootAgentType = %q; want task analyzer", o.rootAgentType)
	}
	wantArgs := append([]string{command}, args...)
	args[0] = "changed"
	first, second := o.mcpCommand(), o.mcpCommand()
	if first == second || !slices.Equal(first.Args, wantArgs) || !slices.Equal(second.Args, wantArgs) {
		t.Fatalf("MCP commands must be independent and preserve constructor arguments")
	}
	// 直接执行 ReAct 节点，验证外部命令的连接错误会经由编排器返回。
	o.rootAgentType = ReactAgentType
	got, err := o.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{}}, &fileContext.DurableState{Goal: "test"})
	if err == nil || !strings.Contains(err.Error(), "connect to MCP command") || got != (ReactResult{}) {
		t.Fatalf("Run() = %+v, %v; want MCP command connection error", got, err)
	}
}

func TestOrchestratorRun(t *testing.T) {
	for _, tt := range []struct {
		name     string
		analysis string
		answer   string
		wantErr  string
		criteria []string
		root     AgentProcessorType
	}{
		{"success", `{"NeedsTools":true,"ExpectedSteps":2,"ReasoningSummary":"inspect and verify","CompletionCriteria":["file verified"]}`, `{"status":"completed","answer":"done"}`, "", []string{"file verified"}, TaskAnalyzerAgentType},
		{"clear stale criteria", `{"NeedsTools":false,"ExpectedSteps":1,"ReasoningSummary":"direct answer","CompletionCriteria":[]}`, `{"status":"completed","answer":"done"}`, "", []string{}, TaskAnalyzerAgentType},
		{"analyzer failure", `{"NeedsTools":true,"ExpectedSteps":0,"ReasoningSummary":"invalid","CompletionCriteria":[]}`, "", `processor "task_analyzer"`, []string{"old criterion"}, TaskAnalyzerAgentType},
		{"react failure", `{"NeedsTools":true,"ExpectedSteps":2,"ReasoningSummary":"inspect and verify","CompletionCriteria":["file verified"]}`, `{"status":"failed","answer":"blocked"}`, `processor "react"`, []string{"old criterion"}, ReactAgentType},
		{"react selects unimplemented plan", "", `{"status":"completed","answer":"done"}`, `unsupported processor type "plan"`, []string{"old criterion"}, ReactAgentType},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type request struct {
				Model string `json:"model"`
				Input string `json:"input"`
			}
			requests := make(chan request, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- req
				text := tt.analysis
				if req.Model == "react-model" {
					text = tt.answer
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_test", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
						map[string]any{"type": "output_text", "text": text},
					}}},
				})
			}))
			defer server.Close()

			// 为两个处理器设置不同模型及提示词，验证编排器按代理类型解析配置。
			path := filepath.Join(t.TempDir(), "agents.yaml")
			yaml := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: react-model\n  sys_prompt: react-prompt\nagents:\n  task_analyzer:\n    llm:\n      base_url: " + server.URL + "\n      api_key: test\n      model: analyzer-model\n    sys_prompt: analyzer-prompt\n"
			if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadAgentConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			state := &fileContext.DurableState{RunId: "orchestrator-task", Goal: "verify file", CompletionCriteria: []string{"old criterion"}}
			o := NewProcessorOrchestrator(ctx, filemcp.Config{Workspace: t.TempDir()})
			// 从指定根类型执行，严格遵循处理器返回的后续列表。
			o.rootAgentType = tt.root
			got, err := o.Run(cfg, state)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != (ReactResult{}) {
					t.Fatalf("Run() = %+v, %v; want zero result and error containing %q", got, err, tt.wantErr)
				}
			} else if err != nil || got != (ReactResult{}) {
				t.Fatalf("Run() = %+v, %v", got, err)
			}
			if !slices.Equal(state.CompletionCriteria, tt.criteria) || state.Goal != "verify file" || state.RunId != "orchestrator-task" {
				t.Fatalf("unexpected durable state: %+v", state)
			}
			if len(requests) != 1 {
				t.Fatalf("got %d requests, want 1", len(requests))
			}
			first := <-requests
			if tt.root == TaskAnalyzerAgentType {
				if first.Model != "analyzer-model" || !strings.Contains(first.Input, "analyzer-prompt") || strings.Contains(first.Input, "react-prompt") {
					t.Fatalf("unexpected analyzer request: %+v", first)
				}
			} else if first.Model != "react-model" || !strings.Contains(first.Input, "react-prompt") || strings.Contains(first.Input, "analyzer-prompt") {
				t.Fatalf("unexpected react request: %+v", first)
			}
		})
	}
}

func TestOrchestratorRunValidation(t *testing.T) {
	cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{}}
	state := &fileContext.DurableState{Goal: "test"}
	for _, tt := range []struct {
		name  string
		o     *ProcessorOrchestrator
		cfg   *config.AgentConfig
		state *fileContext.DurableState
		want  string
	}{
		{"nil orchestrator", nil, cfg, state, "context is required"},
		{"nil context", &ProcessorOrchestrator{}, cfg, state, "context is required"},
		{"nil config", NewProcessorOrchestrator(context.Background(), filemcp.Config{}), nil, state, "agent config"},
		{"nil LLM config", NewProcessorOrchestrator(context.Background(), filemcp.Config{}), &config.AgentConfig{}, state, "agent config"},
		{"nil state", NewProcessorOrchestrator(context.Background(), filemcp.Config{}), cfg, nil, "durable state"},
		{"blank goal", NewProcessorOrchestrator(context.Background(), filemcp.Config{}), cfg, &fileContext.DurableState{Goal: "  "}, "durable state"},
		{"unsupported direct", &ProcessorOrchestrator{ctx: context.Background(), rootAgentType: DirectAgentType}, cfg, state, `unsupported processor type "direct"`},
		{"unsupported plan before side effects", &ProcessorOrchestrator{ctx: context.Background(), rootAgentType: PlanAgentType}, cfg, state, `unsupported processor type "plan"`},
		{"unknown processor", &ProcessorOrchestrator{ctx: context.Background(), rootAgentType: "unknown"}, cfg, state, `unsupported processor type "unknown"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.o.Run(tt.cfg, tt.state)
			if err == nil || !strings.Contains(err.Error(), tt.want) || got != (ReactResult{}) {
				t.Fatalf("Run() = %+v, %v; want error containing %q", got, err, tt.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewProcessorOrchestrator(ctx, filemcp.Config{}).Run(cfg, state); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestOrchestratorRunReactConstructionFailure(t *testing.T) {
	o := &ProcessorOrchestrator{
		ctx: context.Background(), rootAgentType: ReactAgentType,
		toolConfig: filemcp.Config{Workspace: filepath.Join(t.TempDir(), "missing")},
	}
	got, err := o.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{}}, &fileContext.DurableState{Goal: "test"})
	if err == nil || !strings.Contains(err.Error(), `create processor "react"`) || got != (ReactResult{}) {
		t.Fatalf("Run() = %+v, %v; want ReAct construction error", got, err)
	}
}

func TestOrchestratorConcurrentBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := NewProcessorOrchestrator(ctx, filemcp.Config{})
	state := &fileContext.DurableState{Goal: "test"}
	var analyzerCalls int
	var started, finished atomic.Int32
	bothStarted := make(chan struct{})
	run := func(kind AgentProcessorType, cfg *config.AgentConfig, snapshot *fileContext.DurableState) (processorResult, error) {
		if kind == TaskAnalyzerAgentType {
			analyzerCalls++
			if analyzerCalls == 1 {
				return processorResult{
					analysis: &TaskAnalyzerResult{CompletionCriteria: []string{"criterion"}},
					next:     []AgentProcessorType{ReactAgentType, EndAgentType, ReactAgentType},
				}, nil
			}
			if finished.Load() != 2 {
				t.Error("next batch started before both processors finished")
			}
			return processorResult{next: []AgentProcessorType{EndAgentType}}, nil
		}
		if !slices.Equal(snapshot.CompletionCriteria, []string{"criterion"}) {
			t.Errorf("unexpected snapshot: %+v", snapshot)
		}
		// 每个并发节点都修改自己的配置和状态，race 检查可发现快照是否仍然共享。
		snapshot.CompletionCriteria[0] = "local"
		cfg.SysPrompt = "local"
		if started.Add(1) == 2 {
			close(bothStarted)
		}
		select {
		case <-bothStarted:
		case <-ctx.Done():
			return processorResult{}, ctx.Err()
		}
		output := processorResult{react: &ReactResult{Status: CompletedReactStatus, Answer: "done"}}
		if finished.Add(1) == 1 {
			output.next = []AgentProcessorType{TaskAnalyzerAgentType}
		}
		return output, nil
	}
	got, err := o.runBatches(&config.AgentConfig{}, state, run)
	if err != nil || got.Answer != "done" || started.Load() != 2 || analyzerCalls != 2 {
		t.Fatalf("got %+v, %v; started=%d analyzerCalls=%d", got, err, started.Load(), analyzerCalls)
	}
	if !slices.Equal(state.CompletionCriteria, []string{"criterion"}) {
		t.Fatalf("processor mutated shared state: %+v", state)
	}
}

func TestOrchestratorBatchFailureWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := NewProcessorOrchestrator(ctx, filemcp.Config{})
	failure := errors.New("processor failed")
	failed := make(chan struct{})
	var finished atomic.Bool
	run := func(kind AgentProcessorType, _ *config.AgentConfig, _ *fileContext.DurableState) (processorResult, error) {
		if kind == TaskAnalyzerAgentType {
			return processorResult{next: []AgentProcessorType{ReactAgentType, ReactAgentType}}, nil
		}
		if finished.CompareAndSwap(false, true) {
			close(failed)
			return processorResult{}, failure
		}
		select {
		case <-failed:
		case <-ctx.Done():
			return processorResult{}, ctx.Err()
		}
		return processorResult{next: []AgentProcessorType{PlanAgentType}}, failure
	}
	got, err := o.runBatches(&config.AgentConfig{}, &fileContext.DurableState{Goal: "test"}, run)
	if !errors.Is(err, failure) || got != (ReactResult{}) || strings.Count(err.Error(), "processor failed") != 2 {
		t.Fatalf("Run() = %+v, %v; want both batch errors", got, err)
	}
}

func TestOrchestratorEndRoot(t *testing.T) {
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{})
	o.rootAgentType = EndAgentType
	got, err := o.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{}}, &fileContext.DurableState{Goal: "test"})
	if err != nil || got != (ReactResult{}) {
		t.Fatalf("Run() = %+v, %v", got, err)
	}
}
