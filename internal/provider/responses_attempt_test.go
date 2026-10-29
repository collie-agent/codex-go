package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestResponsesUsageEveryHTTPAttempt(t *testing.T) {
	for _, scenario := range []string{"success", "retry success", "exhausted", "http usage", "http usage no retry", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After-Ms", "1")
				if scenario == "malformed" {
					fmt.Fprint(w, `{broken`)
					return
				}
				if scenario == "http usage no retry" {
					w.Header().Set("X-Should-Retry", "false")
				}
				if scenario == "exhausted" || scenario == "http usage" || scenario == "http usage no retry" || (scenario == "retry success" && attempts == 1) {
					w.WriteHeader(500)
				}
				if scenario == "exhausted" {
					fmt.Fprint(w, `{"error":{"message":"unavailable"}}`)
					return
				}
				fmt.Fprintf(w, `{"id":"resp_%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"answer\":\"ok\"}"}]}],"usage":{"input_tokens":%d,"output_tokens":5,"input_tokens_details":{"cached_tokens":2}}}`, attempts, attempts*10)
			}))
			defer server.Close()
			client, err := NewResponsesApiClient[struct {
				Answer string `json:"answer"`
			}](&config.LLMConfig{ApiKey: "secret", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema}, nil)
			if err != nil {
				t.Fatal(err)
			}
			client.SetLogLevel(config.ErrorLogLevel)
			var logs bytes.Buffer
			client.SetOutputer(NewStderrOutputer(&logs))
			result, _, _, err := client.Run(context.Background(), "attempt-task", "prompt", nil)
			wantAttempts := 1
			if scenario == "retry success" {
				wantAttempts = 2
			}
			if scenario == "exhausted" || scenario == "http usage" {
				wantAttempts = 3
			}
			if attempts != wantAttempts {
				t.Fatalf("attempts=%d want=%d", attempts, wantAttempts)
			}
			if scenario == "success" || scenario == "retry success" {
				if err != nil || result.Answer != "ok" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if err == nil {
				t.Fatal("expected original request error")
			}
			if scenario == "http usage" || scenario == "http usage no retry" || scenario == "exhausted" {
				var apiErr *openai.Error
				if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 {
					t.Fatalf("lost HTTP error: %v", err)
				}
			}
			var lines []string
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "LLM usage") {
					lines = append(lines, line)
				}
			}
			if len(lines) != attempts {
				t.Fatalf("logs=%q want %d attempts", lines, attempts)
			}
			for i, line := range lines {
				// SDK 丢弃的重试正文不读取；最终 HTTP 错误正文仍保留实际 usage。
				discarded := (scenario == "retry success" && i == 0) || (scenario == "http usage" && i < attempts-1)
				if scenario == "exhausted" || scenario == "malformed" || discarded {
					status := "http_error_500"
					if scenario == "malformed" {
						status = "unknown"
					}
					if !strings.Contains(line, "status="+status+" input_tokens=unknown output_tokens=unknown cached_input_tokens=unknown uncached_input_tokens=unknown") {
						t.Fatalf("unexpected log %q", line)
					}
				} else if !strings.Contains(line, fmt.Sprintf("response_id=resp_%d status=completed input_tokens=%d output_tokens=5 cached_input_tokens=2 uncached_input_tokens=%d", i+1, (i+1)*10, (i+1)*10-2)) {
					t.Fatalf("unexpected log %q", line)
				}
			}
		})
	}
}

// 传输替身仅控制外部 HTTP 边界，仍经公开 Run 接口观察结果与日志。
type usageTransport func(*http.Request) (*http.Response, error)

func (f usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type usageBrokenBody struct {
	closed bool
	err    error
}

func (b *usageBrokenBody) Read([]byte) (int, error) { return 0, b.err }
func (b *usageBrokenBody) Close() error             { b.closed = true; return nil }

func TestResponsesUsageTransportFailures(t *testing.T) {
	for _, scenario := range []string{"network", "timeout", "cancel", "read failure"} {
		t.Run(scenario, func(t *testing.T) {
			sentinel := errors.New("secret transport detail")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantErr := sentinel
			status := "network_error"
			if scenario == "timeout" {
				wantErr = context.DeadlineExceeded
				status = "timeout"
			}
			if scenario == "cancel" {
				wantErr = context.Canceled
				status = "canceled"
			}
			body := &usageBrokenBody{err: sentinel}
			attempts := 0
			transport := usageTransport(func(r *http.Request) (*http.Response, error) {
				attempts++
				if scenario == "cancel" {
					cancel()
					return nil, r.Context().Err()
				}
				if scenario == "read failure" {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: r}, nil
				}
				return nil, wantErr
			})
			client, err := NewResponsesApiClient[struct {
				Answer string `json:"answer"`
			}](&config.LLMConfig{ApiKey: "secret", BaseUrl: "http://provider.test", Model: "test-model", OutputSchema: config.JsonLLMOutputSchema}, nil)
			if err != nil {
				t.Fatal(err)
			}
			sdkClient := openai.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("http://provider.test"), option.WithHTTPClient(&http.Client{Transport: transport}), option.WithMaxRetries(0))
			client.client = &sdkClient
			client.SetLogLevel(config.ErrorLogLevel)
			var logs bytes.Buffer
			client.SetOutputer(NewStderrOutputer(&logs))
			_, _, _, err = client.Run(ctx, "transport-task", "prompt", nil)
			if !errors.Is(err, wantErr) {
				t.Fatalf("lost error: %v want %v", err, wantErr)
			}
			if attempts != 1 || strings.Count(logs.String(), "LLM usage") != 1 {
				t.Fatalf("attempts=%d logs=%q", attempts, logs.String())
			}
			if !strings.Contains(logs.String(), "status="+status+" input_tokens=unknown output_tokens=unknown cached_input_tokens=unknown uncached_input_tokens=unknown") {
				t.Fatalf("logs=%q", logs.String())
			}
			if strings.Contains(logs.String(), "secret") {
				t.Fatalf("transport details leaked: %q", logs.String())
			}
			if scenario == "read failure" && !body.closed {
				t.Fatal("SDK did not close original response body")
			}
		})
	}
}

func TestResponsesUsageNoHTTPAttemptBeforeValidation(t *testing.T) {
	client, err := NewResponsesApiClient[struct {
		Answer string `json:"answer"`
	}](&config.LLMConfig{ApiKey: "secret", BaseUrl: "http://provider.test", Model: "test-model", OutputSchema: config.JsonLLMOutputSchema}, nil)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	sdkClient := openai.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("http://provider.test"), option.WithHTTPClient(&http.Client{Transport: usageTransport(func(*http.Request) (*http.Response, error) { attempts++; return nil, errors.New("unexpected HTTP") })}), option.WithMaxRetries(0))
	client.client = &sdkClient
	var logs bytes.Buffer
	client.SetOutputer(NewStderrOutputer(&logs))
	_, _, _, err = client.RunWithToolMessages(context.Background(), "invalid-task", "prompt", nil, []ToolMessage{{Name: "inspect"}})
	if err == nil || !strings.Contains(err.Error(), "requires call ID") {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts != 0 || strings.Contains(logs.String(), "LLM usage") {
		t.Fatalf("attempts=%d logs=%q", attempts, logs.String())
	}
}

// 正文遵守请求取消；重试路径不应读取它，最终响应则必须在取消后关闭。
type usageWaitingBody struct {
	ctx    context.Context
	reads  int
	closes int
}

func (b *usageWaitingBody) Read([]byte) (int, error) {
	b.reads++
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *usageWaitingBody) Close() error { b.closes++; return nil }

func TestResponsesUsageBodyLifecycle(t *testing.T) {
	for _, scenario := range []string{"retry", "body cancellation", "headers cancellation"} {
		t.Run(scenario, func(t *testing.T) {
			timeout := 100 * time.Millisecond
			if scenario == "retry" {
				timeout = 3 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			body := &usageWaitingBody{ctx: ctx}
			attempts := 0
			transport := usageTransport(func(r *http.Request) (*http.Response, error) {
				attempts++
				header := http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"1"}}
				if attempts == 1 {
					status := http.StatusOK
					if scenario == "retry" {
						status = http.StatusInternalServerError
					}
					if scenario == "headers cancellation" {
						cancel()
					}
					return &http.Response{StatusCode: status, Header: header, Body: body, Request: r}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Header: header, Request: r,
					Body: io.NopCloser(strings.NewReader(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"answer\":\"ok\"}"}]}],"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}`))}, nil
			})
			client, err := NewResponsesApiClient[struct {
				Answer string `json:"answer"`
			}](&config.LLMConfig{ApiKey: "secret", BaseUrl: "http://provider.test", Model: "test-model", OutputSchema: config.JsonLLMOutputSchema}, nil)
			if err != nil {
				t.Fatal(err)
			}
			sdkClient := openai.NewClient(option.WithAPIKey("secret"), option.WithBaseURL("http://provider.test"), option.WithHTTPClient(&http.Client{Transport: transport}), option.WithMaxRetries(1))
			client.client = &sdkClient
			client.SetLogLevel(config.ErrorLogLevel)
			var logs bytes.Buffer
			client.SetOutputer(NewStderrOutputer(&logs))
			result, _, _, err := client.Run(ctx, "lifecycle-task", "prompt", nil)
			if scenario == "retry" {
				if err != nil || result.Answer != "ok" || attempts != 2 {
					t.Errorf("result=%+v attempts=%d err=%v", result, attempts, err)
				}
				if body.reads != 0 {
					t.Errorf("retry body read %d times", body.reads)
				}
				if !strings.Contains(logs.String(), "status=http_error_500 input_tokens=unknown") {
					t.Errorf("missing discarded-attempt log: %s", logs.String())
				}
			} else {
				wantErr := error(context.DeadlineExceeded)
				if scenario == "headers cancellation" {
					wantErr = context.Canceled
					if body.reads != 0 {
						t.Errorf("canceled response body read %d times", body.reads)
					}
				}
				if !errors.Is(err, wantErr) {
					t.Errorf("error=%v want=%v", err, wantErr)
				}
				if attempts != 1 {
					t.Errorf("attempts=%d want=1", attempts)
				}
				status := "timeout"
				if scenario == "headers cancellation" {
					status = "canceled"
				}
				if !strings.Contains(logs.String(), "status="+status+" input_tokens=unknown") {
					t.Errorf("missing cancellation log: %s", logs.String())
				}
			}
			if body.closes != 1 {
				t.Errorf("body closed %d times, want 1", body.closes)
			}
			if got := strings.Count(logs.String(), "LLM usage"); got != attempts {
				t.Errorf("usage logs=%d attempts=%d", got, attempts)
			}
		})
	}
}
