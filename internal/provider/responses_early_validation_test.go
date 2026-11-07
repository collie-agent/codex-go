package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/provider"
)

type earlyValidationOutput struct{ events []provider.OutputEvent }

func (o *earlyValidationOutput) Emit(_ context.Context, event provider.OutputEvent) error {
	o.events = append(o.events, event)
	return nil
}

func TestInvalidAnswerCancelsStreamBeforeCompletion(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			for _, invalid := range []string{"\n", `\q`, `\uZ`, `\uD83D\uZ`} {
				t.Run(fmt.Sprintf("invalid=%q", invalid), func(t *testing.T) {
					prefix := "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"你好"
					if schema == config.JsonLLMOutputSchema {
						prefix = `{"status":"completed","answer":"你好`
					}
					disconnected := make(chan struct{})
					release := make(chan struct{})
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, text := range []string{prefix, invalid} {
							data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": text})
							fmt.Fprintf(w, "data: %s\n\n", data)
							w.(http.Flusher).Flush()
						}
						// Never send completion: cancellation must be triggered by the invalid delta.
						select {
						case <-r.Context().Done():
							close(disconnected)
						case <-release:
						}
					}))
					defer server.Close()
					defer close(release)
					client, err := provider.NewResponsesApiClient[struct {
						Status string `json:"status" lineprotocol:"STATUS"`
						Answer string `json:"answer" lineprotocol:"ANSWER"`
					}](&config.LLMConfig{Stream: true, BaseUrl: server.URL, Model: "test", OutputSchema: schema}, nil)
					if err != nil {
						t.Fatal(err)
					}
					var output earlyValidationOutput
					client.SetOutputer(&output)
					parent, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					_, _, _, err = client.Run(parent, "early-format", "answer", nil)
					var parseErr *provider.OutputParseError
					if !errors.As(err, &parseErr) {
						t.Fatalf("want retryable output format error before completion, got %v", err)
					}
					if parent.Err() != nil {
						t.Fatalf("format failure waited for or canceled parent: %v", parent.Err())
					}
					if parseErr.Output != prefix+invalid {
						t.Fatalf("invalid response=%q", parseErr.Output)
					}
					select {
					case <-disconnected:
					case <-time.After(time.Second):
						t.Fatal("LLM request remains open")
					}
					var reset *provider.OutputResetOutput
					for _, event := range output.events {
						if event.Kind == provider.OutputTextDelta {
							var delta provider.TextOutput
							if err := json.Unmarshal(event.Data, &delta); err != nil {
								t.Fatal(err)
							}
							if delta.Text != prefix {
								t.Errorf("invalid delta was published before cancellation: %q", delta.Text)
							}
						}
						if event.Kind == provider.OutputReset {
							if reset != nil {
								t.Fatal("duplicate reset")
							}
							reset = new(provider.OutputResetOutput)
							if err := json.Unmarshal(event.Data, reset); err != nil {
								t.Fatal(err)
							}
						}
					}
					if reset == nil || reset.AnswerBytes != 6 || reset.TextBytes != len(prefix) {
						t.Fatalf("missing retraction of provisional Chinese answer: %+v", reset)
					}
				})
			}
		})
	}
}

func TestValidAnswerEscapesWaitAcrossChunks(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			document := `{"answer":"hello\n\"x\"\\\u4f60\u597d\ud83d\ude00"}`
			if schema == config.LineProtocolLLMOutputSchema {
				document = `BEGIN RESULT ANSWER="hello\n\"x\"\\\u4f60\u597d\ud83d\ude00" END RESULT`
			}
			// This public helper emits one byte per chunk and requires answer deltas
			// before completion, so incomplete escapes must wait without cancellation.
			checkStreamingAnswer(t, schema, document, "hello\n\"x\"\\你好😀")
		})
	}
}
