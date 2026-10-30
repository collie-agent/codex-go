package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
)

func TestResponsesStreamReturnsCompleteText(t *testing.T) {
	for _, level := range []config.LogLevel{config.DebugLogLevel, config.InfoLogLevel, config.ErrorLogLevel} {
		t.Run(string(level), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request["stream"] != true {
					t.Errorf("stream = %v, want true", request["stream"])
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
				w.(http.Flusher).Flush()
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"answer\\\":\\\"complete\\\"}\"}]}]}}\n\n")
			}))
			defer server.Close()
			client, err := NewResponsesApiClient[struct {
				Answer string `json:"answer"`
			}](&config.LLMConfig{
				Stream: true, BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			client.SetLogLevel(level)
			file, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			previous := os.Stderr
			os.Stderr = file
			client.SetOutputer(nil)
			defer func() { os.Stderr = previous }()
			result, _, _, err := client.Run(context.Background(), "stream-test", "question", nil)
			if err != nil || result.Answer != "complete" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			data, err := os.ReadFile(file.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "LLM response text=") {
				t.Fatalf("unexpected response log: %s", data)
			}

			if got := strings.Count(string(data), "LLM response text delta=partial"); (level == config.DebugLogLevel && got != 1) || (level != config.DebugLogLevel && got != 0) {
				t.Fatalf("level=%s: unexpected delta logs: %s", level, data)
			}
		})
	}
}

func TestResponsesStreamLogsTerminalUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":4,\"input_tokens_details\":{\"cached_tokens\":2}},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"answer\\\":\\\"ok\\\"}\"}]}]}}\n\n")
	}))
	defer server.Close()
	client, err := NewResponsesApiClient[struct {
		Answer string `json:"answer"`
	}](&config.LLMConfig{Stream: true, BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	client.SetOutputer(NewStderrOutputer(&logs))
	_, _, _, err = client.Run(context.Background(), "stream-usage", "question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "LLM usage") != 1 || !strings.Contains(logs.String(), "response_id=resp_stream status=completed input_tokens=12 output_tokens=4 cached_input_tokens=2 uncached_input_tokens=10") {
		t.Fatalf("usage logs: %s", logs.String())
	}
}

// Exercise both transport strategies through the same public boundary and fixtures.
func TestResponsesTransportParity(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct {
			name, status, output, wantError string
			wantCalls                       int
		}{
			{"tool", "completed", `[{"type":"function_call","name":"read_file","call_id":"call_1","arguments":"{\"path\":\"README.md\"}"}]`, "", 1},
			{"failed", "failed", "[]", "response status", 0},
			{"incomplete", "incomplete", "[]", "response status", 0},
			{"empty", "completed", "[]", "response output is empty", 0},
			{"invalid output", "completed", `[{"type":"message","content":[{"type":"output_text","text":"invalid JSON"}]}]`, "parse response output", 0},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, streaming), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body := fmt.Sprintf(`{"status":%q,"output":%s}`, tc.status, tc.output)
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: {\"type\":\"response.%s\",\"response\":%s}\n\n", tc.status, body)
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, body)
					}
				}))
				defer server.Close()
				client, err := NewResponsesApiClient[struct {
					Answer string `json:"answer"`
				}](&config.LLMConfig{Stream: streaming, BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema}, nil)
				if err != nil {
					t.Fatal(err)
				}
				_, calls, _, err := client.Run(context.Background(), "parity", "question", nil)
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("error=%v, want %s", err, tc.wantError)
					}
				} else if err != nil || len(calls) != tc.wantCalls || calls[0].Arguments != `{"path":"README.md"}` {
					t.Fatalf("calls=%v err=%v", calls, err)
				}
			})
		}
	}
}

func TestResponsesStreamRejectsInterruptedOrErrorEvents(t *testing.T) {
	for _, tc := range []struct{ name, events, want string }{
		{"truncated", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", "without a terminal response"},
		{"error", "event: error\ndata: {\"type\":\"error\",\"message\":\"provider unavailable\"}\n\n", "provider unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, tc.events)
			}))
			defer server.Close()
			client, err := NewResponsesApiClient[struct {
				Answer string `json:"answer"`
			}](&config.LLMConfig{Stream: true, BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = client.Run(context.Background(), "broken-stream", "question", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %s", err, tc.want)
			}
		})
	}
}

func TestResponsesStreamUsageBeforeHTTPBodyEOF(t *testing.T) {
	// Keep HTTP open after [DONE]: the SDK terminates without reading EOF.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_done\",\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":4,\"input_tokens_details\":{\"cached_tokens\":2}},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"answer\\\":\\\"ok\\\"}\"}]}]}}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)
	client, err := NewResponsesApiClient[struct {
		Answer string `json:"answer"`
	}](&config.LLMConfig{Stream: true, BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	client.SetOutputer(NewStderrOutputer(&logs))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _, err = client.Run(ctx, "done-usage", "question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "LLM usage") != 1 || !strings.Contains(logs.String(), "response_id=resp_done status=completed input_tokens=12 output_tokens=4 cached_input_tokens=2 uncached_input_tokens=10") {
		t.Fatalf("terminal usage lost: %s", logs.String())
	}
}
