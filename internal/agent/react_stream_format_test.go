package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
)

type streamCorrectionOutput struct {
	events []provider.OutputEvent
	reset  chan struct{}
}

func (o *streamCorrectionOutput) Emit(_ context.Context, e provider.OutputEvent) error {
	o.events = append(o.events, e)
	if e.Kind == provider.OutputReset {
		select {
		case <-o.reset:
		default:
			close(o.reset)
		}
	}
	return nil
}

func TestReactRetriesImmediatelyCanceledInvalidAnswer(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			prefix, good := `{"status":"completed","answer":"你好`, `{"status":"completed","answer":"你好，完成"}`
			if schema == config.LineProtocolLLMOutputSchema {
				prefix = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"你好"
				good = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"你好，完成\"\nEND RESULT"
			}
			var requests atomic.Int32
			output := &streamCorrectionOutput{reset: make(chan struct{})}
			disconnected, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
					return
				}
				call := requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if call == 1 {
					for _, text := range []string{prefix, "\n"} {
						data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
						fmt.Fprintf(w, "data: %s\n\n", data)
						w.(http.Flusher).Flush()
					}
					select {
					case <-req.Context().Done():
						close(disconnected)
					case <-release:
					}
					return
				}
				if call != 2 {
					t.Errorf("unexpected request %d", call)
				}
				for _, want := range []string{"LLM output format error", "invalid JSON literal for ANSWER"} {
					if !strings.Contains(string(body), want) {
						t.Errorf("correction missing %q", want)
					}
				}
				select {
				case <-output.reset:
				default:
					t.Error("retry started before retraction")
				}
				data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": good})
				fmt.Fprintf(w, "data: %s\n\n", data)
				completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": good}}}}}})
				fmt.Fprintf(w, "data: %s\n\n", completed)
			}))
			defer server.Close()
			defer close(release)
			parent, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			ctx := provider.WithOutputer(parent, output)
			process := &ReactProcess[ReactResult]{BaseReactProcess: BaseReactProcess[ReactResult]{ctx: ctx, maxTurn: 1}}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, Model: "test", Stream: true, OutputSchema: schema}}
			result, next, err := process.Run(cfg, &fileContext.DurableState{RunId: "early-retry", Goal: "answer", CompletionCriteria: []string{"answer verified"}})
			if err != nil || result.Answer != "你好，完成" || len(next) != 1 || next[0] != EndAgentType {
				t.Fatalf("result=%+v next=%v err=%v", result, next, err)
			}
			if parent.Err() != nil {
				t.Fatalf("parent canceled: %v", parent.Err())
			}
			if requests.Load() != 2 {
				t.Fatalf("requests=%d", requests.Load())
			}
			select {
			case <-disconnected:
			case <-time.After(time.Second):
				t.Fatal("first request was not disconnected")
			}
			answer, resets := "", 0
			for _, event := range output.events {
				switch event.Kind {
				case provider.OutputAnswerDelta:
					var data provider.TextOutput
					if err := json.Unmarshal(event.Data, &data); err != nil {
						t.Fatal(err)
					}
					answer += data.Text
				case provider.OutputReset:
					resets++
					var data provider.OutputResetOutput
					if err := json.Unmarshal(event.Data, &data); err != nil {
						t.Fatal(err)
					}
					if data.AnswerBytes != 6 || data.AnswerBytes > len(answer) {
						t.Fatalf("reset=%+v answer=%q", data, answer)
					}
					answer = answer[:len(answer)-data.AnswerBytes]
				}
			}
			if resets != 1 || answer != "你好，完成" {
				t.Fatalf("resets=%d displayed answer=%q", resets, answer)
			}
		})
	}
}
