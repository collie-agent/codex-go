package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/provider"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/testutil"
	"github.com/redis/go-redis/v9"
)

func TestRedisOutputerFiltersAndIsolatesTasks(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	output := provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(client))
	for _, kind := range []provider.OutputKind{provider.OutputSchema, provider.OutputContext, provider.OutputTextDelta, provider.OutputToolCall, provider.OutputUsage} {
		if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "a", Kind: kind, Data: json.RawMessage(`{"text":"event"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "b", Kind: provider.OutputText, Data: json.RawMessage(`{"text":"other"}`)}); err != nil {
		t.Fatal(err)
	}
	a, err := client.XRange(t.Context(), "codex-go-resp-stream-a", "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 || a[0].Values["type"] != "text_delta" || a[1].Values["type"] != "tool_call" || a[2].Values["type"] != "usage" {
		t.Fatalf("events=%v", a)
	}
	b, err := client.XRange(t.Context(), "codex-go-resp-stream-b", "-", "+").Result()
	if err != nil || len(b) != 1 || b[0].Values["delta"] != `{"text":"other"}` {
		t.Fatalf("task b=%v err=%v", b, err)
	}
}

func TestRedisOutputerRejectsInvalidEvents(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	output := provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(client))
	for _, event := range []provider.OutputEvent{
		{Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`)},
		{TaskID: "task", Kind: provider.OutputText, Data: json.RawMessage(`{`)},
		{TaskID: "task", Kind: "unsupported", Data: json.RawMessage(`{}`)},
	} {
		if err := output.Emit(t.Context(), event); err == nil {
			t.Fatalf("accepted invalid event: %+v", event)
		}
	}
	keys, err := client.Keys(t.Context(), "*").Result()
	if err != nil || len(keys) != 0 {
		t.Fatalf("invalid events created keys: %v err=%v", keys, err)
	}
}

func TestRedisOutputerReturnsWriteErrors(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	output := provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(client))
	event := provider.OutputEvent{TaskID: "task", Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := output.Emit(ctx, event); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled emit: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Emit(t.Context(), event); err == nil {
		t.Fatal("closed Redis accepted event")
	}
}

func TestRedisOutputerWritesToolOutcomes(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	output := provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(client))
	for _, kind := range []provider.OutputKind{provider.OutputToolResult, provider.OutputToolError} {
		data, err := json.Marshal(provider.ToolResultOutput{Name: "read_file", CallID: "call-1", Result: json.RawMessage(`[{"type":"text","text":"missing"}]`), Error: "file missing"})
		if err != nil {
			t.Fatal(err)
		}
		if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "task", Kind: kind, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := client.XRange(t.Context(), "codex-go-resp-stream-task", "-", "+").Result()
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	if entries[0].Values["type"] != "tool_result" || entries[1].Values["type"] != "tool_error" {
		t.Fatalf("entries=%v", entries)
	}
	var data provider.ToolResultOutput
	if err := json.Unmarshal([]byte(entries[1].Values["delta"].(string)), &data); err != nil {
		t.Fatal(err)
	}
	if data.CallID != "call-1" || data.Name != "read_file" || data.Error != "file missing" || string(data.Result) != `[{"type":"text","text":"missing"}]` {
		t.Fatalf("tool data=%+v", data)
	}
}

func TestConfiguredRedisOutputUsesSelectedDatabase(t *testing.T) {
	client, addr := testutil.StartRedis(t)
	output, cleanup := provider.NewConfiguredOutputer(&config.RedisConfig{Addr: addr, DB: 2}, config.ErrorLogLevel)
	defer cleanup()
	if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "db-task", Kind: provider.OutputTextDelta, Data: json.RawMessage(`{"text":"partial"}`)}); err != nil {
		t.Fatal(err)
	}
	if count, err := client.Exists(t.Context(), "codex-go-resp-stream-db-task").Result(); err != nil || count != 0 {
		t.Fatalf("unexpected default DB key: %d %v", count, err)
	}
	selected := redis.NewClient(&redis.Options{Addr: addr, DB: 2})
	defer selected.Close()
	entries, err := selected.XRange(t.Context(), "codex-go-resp-stream-db-task", "-", "+").Result()
	if err != nil || len(entries) != 1 || entries[0].Values["type"] != "text_delta" {
		t.Fatalf("selected DB entries=%v err=%v", entries, err)
	}
}

func TestRedisOutputerConcurrentEvents(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	output := provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(client))
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "parallel", Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`)}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	entries, err := client.XRange(t.Context(), "codex-go-resp-stream-parallel", "-", "+").Result()
	if err != nil || len(entries) != 40 {
		t.Fatalf("concurrent entries=%d err=%v", len(entries), err)
	}
	ids := make(map[string]bool)
	for _, entry := range entries {
		if ids[entry.ID] || entry.Values["type"] != "text" || entry.Values["delta"] != `{"text":"hello"}` {
			t.Fatalf("duplicate or corrupted entry: %+v", entry)
		}
		ids[entry.ID] = true
	}
}

func TestResponsesRedisReportsRejectedMixedToolOutput(t *testing.T) {
	redisClient, _ := testutil.StartRedis(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"function_call","name":"read_file","call_id":"mixed","arguments":"{}"},{"type":"message","content":[{"type":"output_text","text":"{\"answer\":\"done\"}"}]}]}`)
	}))
	defer server.Close()
	client, err := provider.NewResponsesApiClient[struct {
		Answer string `json:"answer"`
	}](&config.LLMConfig{BaseUrl: server.URL, Model: "test", ApiKey: "test", OutputSchema: config.JsonLLMOutputSchema}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetLogLevel(config.ErrorLogLevel)
	client.SetOutputer(provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(redisClient)))
	_, calls, _, err := client.RunWithToolMessages(t.Context(), "mixed-task", "inspect", nil, nil)
	var parseError *provider.OutputParseError
	if !errors.As(err, &parseError) || len(calls) != 0 {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
	entries, err := redisClient.XRange(t.Context(), "codex-go-resp-stream-mixed-task", "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Values["type"] != "tool_error" {
			continue
		}
		var data provider.ToolResultOutput
		if err := json.Unmarshal([]byte(entry.Values["delta"].(string)), &data); err != nil {
			t.Fatal(err)
		}
		if data.CallID != "mixed" || data.Name != "read_file" || data.Error == "" {
			t.Fatalf("error=%+v", data)
		}
		return
	}
	t.Fatalf("missing mixed batch rejection: %v", entries)
}

func TestRedisOutputerWritesTextStream(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	output := provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(client))
	err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "task-1", Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := client.XRange(t.Context(), "codex-go-resp-stream-task-1", "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%v", entries)
	}
	entry := entries[0]
	if !regexp.MustCompile(`^\d+-\d+$`).MatchString(entry.ID) || entry.Values["type"] != "text" || entry.Values["delta"] != `{"text":"hello"}` {
		t.Fatalf("unexpected stream entry: %+v", entry)
	}
}

// 输出必须能由共享 rdb reader 读取，避免生产者和 SSE 消费者协议漂移。
func TestRedisOutputerRoundTripsThroughTaskStream(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	manager := rdb.NewRedisManagerWithClient(client)
	output := provider.NewRedisOutputer(manager)
	data := json.RawMessage(`{"text":"你好"}`)
	for _, kind := range []provider.OutputKind{provider.OutputTextDelta, provider.OutputText} {
		if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "roundtrip", Kind: kind, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := manager.ReadTaskStream(t.Context(), "roundtrip", "")
	if err != nil || len(events) != 2 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if events[0].Type != "text_delta" || events[1].Type != "text" || events[0].Delta != string(data) || events[1].Delta != string(data) {
		t.Fatalf("unexpected events: %+v %+v", events[0], events[1])
	}
	resumed, err := manager.ReadTaskStream(t.Context(), "roundtrip", events[0].ID)
	if err != nil || len(resumed) != 1 || resumed[0].ID != events[1].ID {
		t.Fatalf("resumed=%v err=%v", resumed, err)
	}
	ttl, err := client.TTL(t.Context(), rdb.STREAM_NAME+"-roundtrip").Result()
	if err != nil || ttl < 23*time.Hour || ttl > 24*time.Hour {
		t.Fatalf("ttl=%v err=%v", ttl, err)
	}
}

func TestRedisStreamsAnswerDelta(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	output := provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(client))
	if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "answer", Kind: provider.OutputAnswerDelta, Data: json.RawMessage(`{"text":"你好"}`)}); err != nil {
		t.Fatal(err)
	}
	entries, err := client.XRange(t.Context(), "codex-go-resp-stream-answer", "-", "+").Result()
	if err != nil || len(entries) != 1 || entries[0].Values["type"] != "answer_delta" || entries[0].Values["delta"] != `{"text":"你好"}` {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestResponsesRedisNonStreamingTextContainsExtractedAnswer(t *testing.T) {
	redisClient, _ := testutil.StartRedis(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"answer\":\"hello\\n世界\",\"status\":\"completed\"}"}]}]}`)
	}))
	defer server.Close()
	client, err := provider.NewResponsesApiClient[struct {
		Answer string `json:"answer"`
	}](&config.LLMConfig{BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetOutputer(provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(redisClient)))
	if _, _, _, err := client.Run(t.Context(), "answer-text", "question", nil); err != nil {
		t.Fatal(err)
	}
	entries, err := redisClient.XRange(t.Context(), "codex-go-resp-stream-answer-text", "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, entry := range entries {
		switch entry.Values["type"] {
		case "text":
			texts = append(texts, entry.Values["delta"].(string))
		case "text_delta", "answer_delta":
			t.Fatalf("non-streaming delta: %+v", entry)
		}
	}
	if len(texts) != 1 || texts[0] != `{"text":"hello\n世界"}` {
		t.Fatalf("texts=%q", texts)
	}
}

func TestResponsesRedisStreamingNeverEmitsText(t *testing.T) {
	for _, withDelta := range []bool{true, false} {
		t.Run(fmt.Sprint(withDelta), func(t *testing.T) {
			redisClient, _ := testutil.StartRedis(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if withDelta {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"{\\\"answer\\\":\\\"hello\\\"}\"}\n\n")
				}
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"answer\\\":\\\"hello\\\"}\"}]}]}}\n\n")
			}))
			defer server.Close()
			client, err := provider.NewResponsesApiClient[struct {
				Answer string `json:"answer"`
			}](&config.LLMConfig{Stream: true, BaseUrl: server.URL, Model: "test", OutputSchema: config.JsonLLMOutputSchema}, nil)
			if err != nil {
				t.Fatal(err)
			}
			client.SetOutputer(provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(redisClient)))
			result, _, _, err := client.Run(t.Context(), "stream-answer", "question", nil)
			if err != nil || result.Answer != "hello" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			entries, err := redisClient.XRange(t.Context(), "codex-go-resp-stream-stream-answer", "-", "+").Result()
			if err != nil {
				t.Fatal(err)
			}
			var raw, answer int
			for _, entry := range entries {
				switch entry.Values["type"] {
				case "text":
					t.Fatalf("streaming response emitted text: %+v", entry)
				case "text_delta":
					raw++
				case "answer_delta":
					answer++
					if entry.Values["delta"] != `{"text":"hello"}` {
						t.Fatalf("answer=%+v", entry)
					}
				}
			}
			if withDelta && (raw != 1 || answer != 1) {
				t.Fatalf("raw=%d answer=%d", raw, answer)
			}
		})
	}
}

func TestRedisOutputerWritesTaskInfo(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	manager := rdb.NewRedisManagerWithClient(client)
	output := provider.NewRedisOutputer(manager)
	data := json.RawMessage(`{"run_id":"run-1","conv_id":"conv-1"}`)
	if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "run-1", Kind: provider.OutputTaskInfo, Data: data}); err != nil {
		t.Fatal(err)
	}
	entries, err := manager.ReadTaskStream(t.Context(), "run-1", "")
	if err != nil || len(entries) != 1 || entries[0].Type != "task_info" || entries[0].Delta != string(data) {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestRedisOutputPreservesAgentAttribution(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	manager := rdb.NewRedisManagerWithClient(client)
	output := provider.NewRedisOutputer(manager)
	ctx := provider.WithTaskStreamID(t.Context(), "queue-task")
	err := output.Emit(ctx, provider.OutputEvent{TaskID: "run", RunID: "run", AgentInstanceID: "child-2", ParentAgentInstanceID: "child-1", AgentName: "reviewer", Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	events, err := manager.ReadTaskStream(ctx, "queue-task", "")
	if err != nil {
		t.Fatal(err)
	}
	e := events[0]
	if e.RunID != "run" || e.AgentInstanceID != "child-2" || e.ParentAgentInstanceID != "child-1" || e.AgentName != "reviewer" || e.Type != "text" || e.Delta != `{"text":"hello"}` {
		t.Fatalf("event=%+v", e)
	}
}

func TestRedisOutputerWritesReset(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	output := provider.NewRedisOutputer(rdb.NewRedisManagerWithClient(client))
	data := json.RawMessage(`{"text_bytes":42,"answer_bytes":6}`)
	if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "retry", RunID: "run", AgentInstanceID: "agent", Kind: provider.OutputReset, Data: data}); err != nil {
		t.Fatal(err)
	}
	entries, err := client.XRange(t.Context(), "codex-go-resp-stream-retry", "-", "+").Result()
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	fields := entries[0].Values
	if fields["type"] != "output_reset" || fields["delta"] != string(data) || fields["agent_instance_id"] != "agent" || fields["run_id"] != "run" {
		t.Fatalf("reset lost data or attribution: %v", fields)
	}
}
