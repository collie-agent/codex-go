package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/openai/openai-go/v3"
)

func TestResponsesLogsTokenUsage(t *testing.T) {
	testResponseUsage(t, config.ErrorLogLevel, `"usage":{"input_tokens":1000,"output_tokens":120,"input_tokens_details":{"cached_tokens":600,"cache_write_tokens":200}}`,
		"1000", "120", "600", "400")
}

func TestResponsesUsageKnownAndUnknownValues(t *testing.T) {
	cases := []struct{ name, usage, input, output, cached, uncached string }{
		{"full hit", `"usage":{"input_tokens":1000,"output_tokens":120,"input_tokens_details":{"cached_tokens":1000}}`, "1000", "120", "1000", "0"},
		{"no hit", `"usage":{"input_tokens":1000,"output_tokens":120,"input_tokens_details":{"cached_tokens":0}}`, "1000", "120", "0", "1000"},
		{"zero", `"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}`, "0", "0", "0", "0"},
		{"missing usage", `"object":"response"`, "unknown", "unknown", "unknown", "unknown"},
		{"null usage", `"usage":null`, "unknown", "unknown", "unknown", "unknown"},
		{"missing details", `"usage":{"input_tokens":1000,"output_tokens":120}`, "1000", "120", "unknown", "unknown"},
		{"null details", `"usage":{"input_tokens":1000,"output_tokens":120,"input_tokens_details":null}`, "1000", "120", "unknown", "unknown"},
		{"missing cached", `"usage":{"input_tokens":1000,"output_tokens":120,"input_tokens_details":{}}`, "1000", "120", "unknown", "unknown"},
		{"null cached", `"usage":{"input_tokens":1000,"output_tokens":120,"input_tokens_details":{"cached_tokens":null}}`, "1000", "120", "unknown", "unknown"},
		{"missing totals", `"usage":{"input_tokens_details":{"cached_tokens":10}}`, "unknown", "unknown", "10", "unknown"},
		{"null totals", `"usage":{"input_tokens":null,"output_tokens":null,"input_tokens_details":{"cached_tokens":10}}`, "unknown", "unknown", "10", "unknown"},
	}
	for _, level := range []config.LogLevel{config.InfoLogLevel, config.ErrorLogLevel} {
		for _, tc := range cases {
			t.Run(string(level)+"/"+tc.name, func(t *testing.T) {
				testResponseUsage(t, level, tc.usage, tc.input, tc.output, tc.cached, tc.uncached)
			})
		}
	}
}

// Exercise the public request boundary against a controlled provider response.
func testResponseUsage(t *testing.T, level config.LogLevel, usage, input, output, cached, uncached string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_usage","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"answer\":\"ok\"}"}]}],%s}`, usage)
	}))
	defer server.Close()
	client, err := NewResponsesApiClient[struct {
		Answer string `json:"answer"`
	}](&config.LLMConfig{
		ApiKey: "secret-key", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetLogLevel(level)
	client.SetContextLogLabel("observe")
	var logs bytes.Buffer
	client.SetOutputer(NewStderrOutputer(&logs))
	result, calls, _, err := client.Run(context.Background(), "usage-task", "private prompt", nil)
	if err != nil || result.Answer != "ok" || len(calls) != 0 {
		t.Fatalf("result=%+v calls=%v err=%v", result, calls, err)
	}
	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "LLM usage") {
			lines = append(lines, line)
		}
	}
	want := fmt.Sprintf("task usage-task: [observe] LLM usage model=test-model response_id=resp_usage status=completed input_tokens=%s output_tokens=%s cached_input_tokens=%s uncached_input_tokens=%s", input, output, cached, uncached)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], want) {
		t.Fatalf("usage logs=%q, want one line ending %q", lines, want)
	}
}

func TestResponsesUsageLoggedBeforeOutputHandling(t *testing.T) {
	cases := []struct {
		name, status, output, wantError string
		wantTools                       int
	}{
		{"incomplete", "incomplete", "[]", "response status", 0},
		{"failed", "failed", "[]", "response status", 0},
		{"empty", "completed", "[]", "response output is empty", 0},
		{"invalid output", "completed", `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"invalid-json"}]}]`, "parse response output", 0},
		{"tool call", "completed", `[{"type":"function_call","call_id":"call_1","name":"inspect","arguments":"{}"}]`, "", 1},
	}
	for _, level := range []config.LogLevel{config.InfoLogLevel, config.ErrorLogLevel} {
		for _, tc := range cases {
			t.Run(string(level)+"/"+tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"status":%q,"output":%s,"usage":{"input_tokens":20,"output_tokens":5,"input_tokens_details":{"cached_tokens":4}}}`, tc.status, tc.output)
				}))
				defer server.Close()
				client, err := NewResponsesApiClient[struct {
					Answer string `json:"answer"`
				}](&config.LLMConfig{ApiKey: "secret-key", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema}, nil)
				if err != nil {
					t.Fatal(err)
				}
				client.SetLogLevel(level)
				var logs bytes.Buffer
				client.SetOutputer(NewStderrOutputer(&logs))
				history := []ToolMessage{{ChatCompletionToolMessageParam: *openai.ToolMessage("result", "previous_call").OfTool, Name: "inspect", Arguments: "{}"}}
				for range 2 {
					_, calls, _, err := client.RunWithToolMessages(context.Background(), "usage-task", "private prompt", nil, history)
					if tc.wantError == "" {
						if err != nil {
							t.Fatal(err)
						}
					} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("error=%v, want %q", err, tc.wantError)
					}
					if tc.name == "invalid output" {
						var parseErr *OutputParseError
						if !errors.As(err, &parseErr) || parseErr.Output != "invalid-json" {
							t.Fatalf("lost parse error: %v", err)
						}
					}
					if len(calls) != tc.wantTools {
						t.Fatalf("tool calls=%v", calls)
					}
				}
				var lines []string
				for _, line := range strings.Split(logs.String(), "\n") {
					if strings.Contains(line, "LLM usage") {
						lines = append(lines, line)
					}
				}
				if len(lines) != 2 {
					t.Fatalf("usage logs=%q, want one per response", lines)
				}
				want := "task usage-task: LLM usage model=test-model status=" + tc.status + " input_tokens=20 output_tokens=5 cached_input_tokens=4 uncached_input_tokens=16"
				for _, line := range lines {
					if !strings.HasSuffix(line, want) {
						t.Errorf("usage log=%q, want suffix %q", line, want)
					}
				}
			})
		}
	}
}
