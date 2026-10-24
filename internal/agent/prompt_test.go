package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactRunSendsBuiltInPromptBeforeCustomPrompt(t *testing.T) {
	const customPrompt = "始终先核对完成标准，再报告任务结果。"
	const builtInMarker = "你是 ReAct agent 的单轮决策器。"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inputReceived := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			return
		}
		if len(request.Input) != 2 || request.Input[0].Role != "system" || request.Input[1].Role != "user" {
			t.Errorf("unexpected message roles: %+v", request.Input)
		} else {
			if strings.Contains(request.Input[1].Content, builtInMarker) || strings.Contains(request.Input[1].Content, customPrompt) {
				t.Error("system instructions duplicated in user message")
			}
			if strings.Contains(request.Input[0].Content, "本次运行目标") {
				t.Error("task goal included in system message")
			}
		}
		var combined string
		for _, item := range request.Input {
			combined += item.Content + "\n"
		}
		inputReceived <- combined
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_final","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\"}"}]}]}`))
	}))
	defer server.Close()

	process, err := NewReactProcess[ReactResult](ctx, filemcp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runConfig := &config.AgentConfig{
		LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
		SysPrompt: customPrompt,
	}
	state := &fileContext.DurableState{RunId: "run-task", Goal: "本次运行目标"}
	var processor AgentProcessor[ReactResult] = process
	if _, _, err := processor.Run(runConfig, state); err != nil {
		t.Fatal(err)
	}

	select {
	case input := <-inputReceived:
		if !strings.Contains(input, state.Goal) {
			t.Fatalf("model request did not use Run config and state: %q", input)
		}
		builtInAt := strings.Index(input, builtInMarker)
		customAt := strings.Index(input, customPrompt)
		if builtInAt < 0 || customAt < 0 {
			t.Fatalf("model request omitted prompt content: %q", input)
		}
		if builtInAt >= customAt {
			t.Fatalf("custom prompt must follow built-in ReAct prompt: %q", input)
		}
	case <-ctx.Done():
		t.Fatal("model request was not captured")
	}
}
