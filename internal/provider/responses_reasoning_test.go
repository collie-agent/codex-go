package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/provider"
)

func TestResponsesReasoningEffort(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh"} {
			t.Run(fmt.Sprintf("stream=%v/effort=%s", stream, effort), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Reasoning struct {
							Effort string `json:"effort"`
						} `json:"reasoning"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					want := effort
					if want == "" {
						want = "none"
					}
					if body.Reasoning.Effort != want {
						t.Errorf("reasoning.effort = %q, want %q", body.Reasoning.Effort, want)
					}
					response := `{"id":"resp_reasoning","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"answer\":\"ok\"}"}]}]}`
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, response)
					}
				}))
				defer server.Close()
				client, err := provider.NewResponsesApiClient[struct {
					Answer string `json:"answer"`
				}](&config.LLMConfig{ApiKey: "test", BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema, Stream: stream, ReasoningEffort: effort}, nil)
				if err != nil {
					t.Fatal(err)
				}
				result, _, _, err := client.Run(context.Background(), "reasoning-task", "input", nil)
				if err != nil || result.Answer != "ok" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			})
		}
	}
}
