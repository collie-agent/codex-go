package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

// 服务端默认预算不足时返回与线上相同的截断错误，显式预算才允许完成。
func TestResponsesMaxOutputTokens(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, limit := range []int64{0, 32768} {
			t.Run(fmt.Sprintf("stream=%v/limit=%d", stream, limit), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					got, present := body["max_output_tokens"]
					if (limit == 0 && present) || (limit > 0 && got != float64(limit)) {
						t.Errorf("max_output_tokens=%v, want %d (zero omitted)", got, limit)
					}
					status := "incomplete"
					output := `[]`
					if got == float64(32768) {
						status = "completed"
						output = `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"answer\":\"ok\"}"}]}]`
					}
					response := fmt.Sprintf(`{"id":"resp_limit","status":%q,"output":%s,"incomplete_details":{"reason":"max_output_tokens"}}`, status, output)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "event: response.%s\ndata: {\"type\":\"response.%s\",\"response\":%s}\n\n", status, status, response)
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, response)
					}
				}))
				defer server.Close()
				client, err := NewResponsesApiClient[struct {
					Answer string `json:"answer"`
				}](&config.LLMConfig{ApiKey: "test", BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema, Stream: stream, MaxOutputTokens: limit}, nil)
				if err != nil {
					t.Fatal(err)
				}
				result, calls, _, err := client.Run(context.Background(), "limit-task", "input", nil)
				if limit == 0 {
					if err == nil || len(calls) != 0 {
						t.Fatalf("truncated response: calls=%v err=%v", calls, err)
					}
				} else if err != nil || result.Answer != "ok" {
					t.Fatalf("answer=%q err=%v", result.Answer, err)
				}
			})
		}
	}
}
