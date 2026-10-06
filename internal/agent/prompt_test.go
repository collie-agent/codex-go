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
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			return
		}
		inputReceived <- request.Input
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_final","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\"}"}]}]}`))
	}))
	defer server.Close()

	process, err := NewReactProcess[ReactResult](ctx, &fileContext.DurableState{RunId: "prompt-test", Goal: "完成任务"}, &config.AgentConfig{
		LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
		SysPrompt: customPrompt,
	}, filemcp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.Run(); err != nil {
		t.Fatal(err)
	}

	select {
	case input := <-inputReceived:
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
