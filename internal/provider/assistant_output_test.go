package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/provider"
)

type visibleOutput struct{ events []provider.OutputEvent }

func (o *visibleOutput) Emit(_ context.Context, e provider.OutputEvent) error {
	o.events = append(o.events, e)
	return nil
}

func TestExplanationAndAnswerUseSeparateEvents(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			turn := 0
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turn++
				text := "继续检查上下文预算。"
				output := []any{map[string]any{"type": "function_call", "call_id": "inspect-1", "name": "inspect", "arguments": "{}"}}
				if turn == 2 {
					text = `{"answer":"检查完成。"}`
					output = nil
				}
				output = append(output, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
				response := map[string]any{"status": "completed", "output": output}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, delta := range []string{text[:3], text[3:]} {
						data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": delta})
						fmt.Fprintf(w, "data: %s\n\n", data)
					}
					data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
					fmt.Fprintf(w, "data: %s\n\n", data)
				} else {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(response)
				}
			}))
			defer model.Close()
			client, err := provider.NewResponsesApiClient[struct {
				Answer string `json:"answer"`
			}](&config.LLMConfig{BaseUrl: model.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema, Stream: stream}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var output visibleOutput
			client.SetOutputer(&output)
			for range 2 {
				if _, _, _, err := client.Run(t.Context(), "run", "inspect", nil); err != nil {
					t.Fatal(err)
				}
			}
			var assistant, answer strings.Builder
			for _, e := range output.events {
				var body provider.TextOutput
				if e.Kind != "assistant_delta" && e.Kind != "answer_delta" {
					continue
				}
				if err := json.Unmarshal(e.Data, &body); err != nil {
					t.Fatal(err)
				}
				if e.Kind == "assistant_delta" {
					assistant.WriteString(body.Text)
				} else {
					answer.WriteString(body.Text)
				}
			}
			if assistant.String() != "继续检查上下文预算。" || answer.String() != "检查完成。" {
				t.Fatalf("assistant=%q answer=%q", assistant.String(), answer.String())
			}
		})
	}
}
