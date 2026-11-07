package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

// Replay the public output events across a rejected response and its correction.
func TestRejectedResponseDoesNotDuplicateAnswer(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", schema, stream), func(t *testing.T) {
				bad, good := "{\"answer\":\"你好\n世界\"}", `{"answer":"你好\n世界"}`
				if schema == config.LineProtocolLLMOutputSchema {
					bad = "BEGIN RESULT\nANSWER=\"你好\n世界\"\nEND RESULT"
					good = "BEGIN RESULT\nANSWER=\"你好\\n世界\"\nEND RESULT"
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					document := bad
					if calls == 2 {
						document = good
					}
					response := map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": document}}}}}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						// Multiple deltas expose the valid prefix before the invalid newline.
						for _, delta := range strings.SplitAfter(document, "你好") {
							data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": delta})
							fmt.Fprintf(w, "data: %s\n\n", data)
						}
						data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
						fmt.Fprintf(w, "data: %s\n\n", data)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
					}
				}))
				defer server.Close()
				client, err := NewResponsesApiClient[struct {
					Answer string `json:"answer" lineprotocol:"ANSWER"`
				}](&config.LLMConfig{BaseUrl: server.URL, Model: "test", OutputSchema: schema, Stream: stream}, nil)
				if err != nil {
					t.Fatal(err)
				}
				client.SetLogLevel(config.ErrorLogLevel)
				var output recordedOutput
				client.SetOutputer(&output)
				if _, _, _, err := client.Run(t.Context(), "retry", "question", nil); err == nil {
					t.Fatal("malformed response accepted")
				}
				if _, _, _, err := client.Run(t.Context(), "retry", "correct format", nil); err != nil {
					t.Fatal(err)
				}
				// Keep already accepted content from earlier turns intact when rolling back.
				answer, raw := "earlier:", "earlier:"
				resets := 0
				for _, event := range output.events {
					var data struct {
						Text        string `json:"text"`
						TextBytes   int    `json:"text_bytes"`
						AnswerBytes int    `json:"answer_bytes"`
					}
					if err := json.Unmarshal(event.Data, &data); err != nil {
						t.Fatal(err)
					}
					switch event.Kind {
					case OutputAnswerDelta, OutputText:
						answer += data.Text
					case OutputTextDelta:
						raw += data.Text
					case OutputKind("output_reset"):
						resets++
						if data.AnswerBytes > len(answer)-len("earlier:") || data.TextBytes > len(raw)-len("earlier:") {
							t.Fatal("reset removes earlier turn")
						}
						answer = answer[:len(answer)-data.AnswerBytes]
						raw = raw[:len(raw)-data.TextBytes]
					}
				}
				if answer != "earlier:你好\n世界" {
					t.Errorf("visible answer=%q; rejected prefix duplicated", answer)
				}
				if stream && (raw != "earlier:"+good || resets != 1) {
					t.Errorf("raw=%q resets=%d; want only corrected response and one reset", raw, resets)
				}
				if !stream && resets != 0 {
					t.Errorf("nonstreaming rejected output should never be published; resets=%d", resets)
				}
			})
		}
	}
}

func TestStreamFailureRetractsProvisionalOutput(t *testing.T) {
	for _, terminal := range []string{"", `data: {"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n"} {
		t.Run(fmt.Sprintf("terminal=%q", terminal), func(t *testing.T) {
			prefix := `{"answer":"临时`
			delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": prefix})
			client := newOutputTestClient(t, fmt.Sprintf("data: %s\n\n%s", delta, terminal), true)
			var output recordedOutput
			client.SetOutputer(&output)
			if _, _, _, err := client.Run(t.Context(), "failed-stream", "question", nil); err == nil {
				t.Fatal("failed stream accepted")
			}
			resets := output.ofKind(OutputReset)
			if len(resets) != 1 || output.events[len(output.events)-1].Kind != OutputReset {
				t.Fatalf("missing final reset: %+v", output.events)
			}
			var reset OutputResetOutput
			if err := json.Unmarshal(resets[0].Data, &reset); err != nil {
				t.Fatal(err)
			}
			if reset.TextBytes != len(prefix) || reset.AnswerBytes != len("临时") {
				t.Fatalf("reset=%+v", reset)
			}
		})
	}
}
