package provider

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
)

// TestResponsesRunIntegration performs real requests only when explicitly enabled.
func TestResponsesRunIntegration(t *testing.T) {
	if os.Getenv("RUN_RESPONSES_INTEGRATION") != "1" {
		t.Skip("set RUN_RESPONSES_INTEGRATION=1 to enable real API requests")
	}
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL"} {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is required", name)
		}
	}
	type result struct {
		Message string `json:"message"`
		Count   int    `json:"count"`
	}
	for _, tc := range []struct {
		name   string
		schema config.LLMOutputSchema
		input  string
	}{
		{"json", config.JsonLLMOutputSchema, `Return exactly this JSON object, without Markdown or other text: {"message":"hello","count":2}`},
		{"line_protocol", config.LineProtocolLLMOutputSchema, "Return exactly these four lines, without Markdown or other text:\nBEGIN RESULT\nMESSAGE=\"hello\"\nCOUNT=2\nEND RESULT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewResponsesApiClient[result](&config.LLMConfig{
				ApiKey: os.Getenv("OPENAI_API_KEY"), BaseUrl: os.Getenv("OPENAI_BASE_URL"),
				Model: os.Getenv("OPENAI_MODEL"), OutputSchema: tc.schema,
			})
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			got, err := client.Run(ctx, "integration-"+tc.name, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if want := (result{Message: "hello", Count: 2}); got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			t.Logf("decoded result: %+v", got)
		})
	}
}
