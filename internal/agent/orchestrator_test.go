package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	if o.rootAgentType != ReactAgentType {
		t.Fatalf("rootAgentType = %q; want ReAct", o.rootAgentType)
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
	if err == nil || !strings.Contains(err.Error(), "connect to MCP command") || len(got) != 0 {
		t.Fatalf("Run() = %+v, %v; want MCP command connection error", got, err)
	}
}

func TestOrchestratorRun(t *testing.T) {
	for _, tt := range []struct {
		name     string
		answer   string
		wantErr  string
		criteria []string
		root     AgentProcessorType
	}{
		{"react failure", `{"status":"failed","answer":"blocked"}`, `processor "react"`, []string{"old criterion"}, ReactAgentType},
		{"react ends branch", `{"status":"completed","answer":"done"}`, "", []string{"old criterion"}, ReactAgentType},
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
				if err := decodeModelRequest(r.Body, &req); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- req
				text := tt.answer
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_test", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
						map[string]any{"type": "output_text", "text": text},
					}}},
				})
			}))
			defer server.Close()

			// ReAct 使用 base 配置，验证编排器直接运行 ReAct。
			path := filepath.Join(t.TempDir(), "agents.yaml")
			yaml := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: react-model\n  sys_prompt: react-prompt\n"
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
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || len(got) != 0 {
					t.Fatalf("Run() = %+v, %v; want zero result and error containing %q", got, err, tt.wantErr)
				}
			} else if err != nil || !reflect.DeepEqual(got, []ReactResult{{Status: CompletedReactStatus, Answer: "done"}}) {
				t.Fatalf("Run() = %+v, %v", got, err)
			}
			if !slices.Equal(state.CompletionCriteria, tt.criteria) || state.Goal != "verify file" || state.RunId != "orchestrator-task" {
				t.Fatalf("unexpected durable state: %+v", state)
			}
			if len(requests) != 1 {
				t.Fatalf("got %d requests, want only ReAct", len(requests))
			}
			first := <-requests
			if first.Model != "react-model" || !strings.Contains(first.Input, "react-prompt") {
				t.Fatalf("unexpected react request: %+v", first)
			}
		})
	}
}

func TestOrchestratorRunReactConstructionFailure(t *testing.T) {
	o := &ProcessorOrchestrator{
		ctx: context.Background(), rootAgentType: ReactAgentType,
		toolConfig: filemcp.Config{Workspace: filepath.Join(t.TempDir(), "missing")},
	}
	got, err := o.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{}}, &fileContext.DurableState{Goal: "test"})
	if err == nil || !strings.Contains(err.Error(), `create processor "react"`) || len(got) != 0 {
		t.Fatalf("Run() = %+v, %v; want ReAct construction error", got, err)
	}
}

func TestPlanExecutorPreservesFailedStepIndex(t *testing.T) {
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{Workspace: filepath.Join(t.TempDir(), "missing")})
	o.executionMode = PlanAgentType
	state := &fileContext.DurableState{
		Goal: "test", CurrentStepIndex: 1,
		StepExecutions: []StepExecution{{Status: CompletedExecutionStatus}, {Status: PendingExecutionStatus}},
	}
	output, err := o.runProcessor(ReactAgentType, &config.AgentConfig{LLMConfig: &config.LLMConfig{}}, state)
	if err == nil || output.state == nil || output.state.CurrentStepIndex != 1 || output.state.StepExecutions[1].Status != FailedExecutionStatus || output.state.StepExecutions[0].Status != CompletedExecutionStatus {
		t.Fatalf("runProcessor() state=%+v err=%v", output.state, err)
	}
}

func TestPlanExecutorRejectsInvalidStepIndex(t *testing.T) {
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{})
	o.executionMode = PlanAgentType
	for _, index := range []int{-1, 0, 2} {
		state := &fileContext.DurableState{
			Goal: "test", CurrentStepIndex: index,
			StepExecutions: []StepExecution{{Status: CompletedExecutionStatus}, {Status: PendingExecutionStatus}},
		}
		_, err := o.runProcessor(ReactAgentType, &config.AgentConfig{LLMConfig: &config.LLMConfig{}}, state)
		if err == nil || !strings.Contains(err.Error(), "no pending step at index") || state.StepExecutions[1].Status != PendingExecutionStatus {
			t.Fatalf("runProcessor() index=%d err=%v state=%+v", index, err, state)
		}
	}
}

func TestOrchestratorConcurrentBatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := NewProcessorOrchestrator(ctx, filemcp.Config{})
	o.rootAgentType = PlanAgentType
	state := &fileContext.DurableState{Goal: "test"}
	var plannerCalls int
	var started, finished atomic.Int32
	bothStarted := make(chan struct{})
	run := func(kind AgentProcessorType, cfg *config.AgentConfig, snapshot *fileContext.DurableState) (processorResult, error) {
		if kind == PlanAgentType {
			plannerCalls++
			if plannerCalls == 1 {
				return processorResult{
					state: &fileContext.DurableState{Goal: snapshot.Goal, CompletionCriteria: []string{"criterion"}},
					next:  []AgentProcessorType{ReactAgentType, EndAgentType, ReactAgentType},
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
			output.next = []AgentProcessorType{PlanAgentType}
		}
		return output, nil
	}
	got, err := o.runBatches(&config.AgentConfig{}, state, run)
	if err != nil || !reflect.DeepEqual(got, []ReactResult{{Status: CompletedReactStatus, Answer: "done"}, {Status: CompletedReactStatus, Answer: "done"}}) || started.Load() != 2 || plannerCalls != 2 {
		t.Fatalf("got %+v, %v; started=%d plannerCalls=%d", got, err, started.Load(), plannerCalls)
	}
	if !slices.Equal(state.CompletionCriteria, []string{"criterion"}) {
		t.Fatalf("processor mutated shared state: %+v", state)
	}
}

func TestOrchestratorBatchFailureWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := NewProcessorOrchestrator(ctx, filemcp.Config{})
	o.rootAgentType = PlanAgentType
	failure := errors.New("processor failed")
	failed := make(chan struct{})
	var finished atomic.Bool
	run := func(kind AgentProcessorType, _ *config.AgentConfig, _ *fileContext.DurableState) (processorResult, error) {
		if kind == PlanAgentType {
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
	if !errors.Is(err, failure) || len(got) != 0 || strings.Count(err.Error(), "processor failed") != 2 {
		t.Fatalf("Run() = %+v, %v; want both batch errors", got, err)
	}
}

func TestOrchestratorEndRoot(t *testing.T) {
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{})
	o.rootAgentType = EndAgentType
	got, err := o.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{}}, &fileContext.DurableState{Goal: "test"})
	if err != nil || len(got) != 0 {
		t.Fatalf("Run() = %+v, %v", got, err)
	}
}

func TestOrchestratorFinalBranchResults(t *testing.T) {
	for _, tt := range []struct {
		name  string
		nodes []processorResult
		want  []ReactResult
	}{
		{
			name: "continuation replaces intermediate result",
			nodes: []processorResult{
				{react: &ReactResult{Answer: "intermediate"}, next: []AgentProcessorType{ReactAgentType}},
				{react: &ReactResult{Answer: "final"}, next: []AgentProcessorType{EndAgentType}},
			},
			want: []ReactResult{{Answer: "final"}},
		},
		{
			name: "nested branches preserve declaration order despite different depths",
			nodes: []processorResult{
				{react: &ReactResult{Answer: "root"}, next: []AgentProcessorType{ReactAgentType, EndAgentType}},
				{react: &ReactResult{Answer: "middle"}, next: []AgentProcessorType{EndAgentType, PlanAgentType}},
				{next: []AgentProcessorType{ReactAgentType}},
				{react: &ReactResult{Answer: "deep"}},
			},
			want: []ReactResult{{Answer: "middle"}, {Answer: "deep"}, {Answer: "root"}},
		},
		{
			name: "non-react continuation inherits result",
			nodes: []processorResult{
				{react: &ReactResult{Answer: "final"}, next: []AgentProcessorType{PlanAgentType}},
				{},
			},
			want: []ReactResult{{Answer: "final"}},
		},
		{
			name: "branches without react have no result",
			nodes: []processorResult{
				{next: []AgentProcessorType{EndAgentType, PlanAgentType}},
				{},
			},
		},
		{
			name: "duplicate end branches retain separate results",
			nodes: []processorResult{
				{react: &ReactResult{Answer: "shared"}, next: []AgentProcessorType{EndAgentType, EndAgentType}},
			},
			want: []ReactResult{{Answer: "shared"}, {Answer: "shared"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := NewProcessorOrchestrator(context.Background(), filemcp.Config{})
			var calls int
			// 每批最多一个执行节点，其余为 end，保证脚本调用顺序确定。
			run := func(_ AgentProcessorType, _ *config.AgentConfig, _ *fileContext.DurableState) (processorResult, error) {
				if calls >= len(tt.nodes) {
					return processorResult{}, errors.New("unexpected processor call")
				}
				output := tt.nodes[calls]
				calls++
				return output, nil
			}
			got, err := o.runBatches(&config.AgentConfig{}, &fileContext.DurableState{Goal: "test"}, run)
			if err != nil || !reflect.DeepEqual(got, tt.want) || calls != len(tt.nodes) {
				t.Fatalf("runBatches() = %+v, %v; want %+v; calls=%d", got, err, tt.want, calls)
			}
		})
	}
}

func TestOrchestratorFailureDiscardsFinishedBranches(t *testing.T) {
	o := NewProcessorOrchestrator(context.Background(), filemcp.Config{})
	failure := errors.New("continuation failed")
	calls := 0
	run := func(_ AgentProcessorType, _ *config.AgentConfig, _ *fileContext.DurableState) (processorResult, error) {
		calls++
		if calls == 1 {
			return processorResult{react: &ReactResult{Answer: "done"}, next: []AgentProcessorType{EndAgentType, ReactAgentType}}, nil
		}
		return processorResult{}, failure
	}
	got, err := o.runBatches(&config.AgentConfig{}, &fileContext.DurableState{Goal: "test"}, run)
	if got != nil || !errors.Is(err, failure) {
		t.Fatalf("runBatches() = %+v, %v; want nil and failure", got, err)
	}
}

func TestOrchestratorBranchOrderIgnoresCompletionOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := NewProcessorOrchestrator(ctx, filemcp.Config{})
	o.rootAgentType = PlanAgentType
	secondFinished := make(chan struct{})
	var plannerCalls atomic.Int32
	run := func(kind AgentProcessorType, _ *config.AgentConfig, _ *fileContext.DurableState) (processorResult, error) {
		if kind == PlanAgentType {
			if plannerCalls.Add(1) == 1 {
				return processorResult{next: []AgentProcessorType{ReactAgentType, PlanAgentType}}, nil
			}
			close(secondFinished)
			return processorResult{react: &ReactResult{Answer: "second"}}, nil
		}
		// 首个分支必须等第二个分支产生结果，验证并发完成顺序不影响返回顺序。
		select {
		case <-secondFinished:
		case <-ctx.Done():
			return processorResult{}, ctx.Err()
		}
		return processorResult{react: &ReactResult{Answer: "first"}}, nil
	}
	got, err := o.runBatches(&config.AgentConfig{}, &fileContext.DurableState{Goal: "test"}, run)
	want := []ReactResult{{Answer: "first"}, {Answer: "second"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("runBatches() = %+v, %v; want %+v", got, err, want)
	}
}
