package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

func TestResponsesLogsFormatErrorsAtErrorLevel(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
			t.Run(fmt.Sprintf("%s/stream=%v", schema, streaming), func(t *testing.T) {
				const invalid = "broken\noutput"
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					text, _ := json.Marshal(invalid)
					body := fmt.Sprintf(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":%s}]}]}`, text)
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", body)
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, body)
					}
				}))
				defer server.Close()
				client, err := NewResponsesApiClient[struct {
					Answer string `json:"answer"`
				}](&config.LLMConfig{BaseUrl: server.URL, Model: "test", Stream: streaming, OutputSchema: schema}, nil)
				if err != nil {
					t.Fatal(err)
				}
				client.SetLogLevel(config.ErrorLogLevel)
				client.SetContextLogLabel("react")
				var logs bytes.Buffer
				writer := log.Writer()
				log.SetOutput(&logs)
				t.Cleanup(func() { log.SetOutput(writer) })
				_, _, _, err = client.Run(t.Context(), "format-task", "question", nil)
				var parseErr *OutputParseError
				if !errors.As(err, &parseErr) || parseErr.Output != invalid {
					t.Fatalf("expected original parse error, got %v", err)
				}
				for _, want := range []string{"task format-task: [react] LLM output format error:", parseErr.Err.Error(), fmt.Sprintf("output=%q", invalid)} {
					if !strings.Contains(logs.String(), want) {
						t.Errorf("missing %q in logs: %s", want, logs.String())
					}
				}
				if strings.Count(logs.String(), "LLM output format error:") != 1 {
					t.Fatalf("expected one format error log: %s", logs.String())
				}
			})
		}
	}
}
