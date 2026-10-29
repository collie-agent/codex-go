package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

type recordedOutput struct{ events []OutputEvent }

func (o *recordedOutput) Emit(_ context.Context, event OutputEvent) error {
	o.events = append(o.events, event)
	return nil
}

func (o *recordedOutput) ofKind(kind OutputKind) []OutputEvent {
	var events []OutputEvent
	for _, event := range o.events {
		if event.Kind == kind {
			events = append(events, event)
		}
	}
	return events
}

type outputAnswer struct {
	Answer string `json:"answer"`
}

const outputResponse = `{"id":"resp_output","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"answer\":\"ok\"}"}]}],"usage":{"input_tokens":20,"output_tokens":5,"input_tokens_details":{"cached_tokens":4}}}`

func newOutputTestClient(t *testing.T, body string, streaming bool) *responsesApiClient[outputAnswer] {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, body)
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewResponsesApiClient[outputAnswer](&config.LLMConfig{
		BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema, Stream: streaming,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetLogLevel(config.ErrorLogLevel)
	client.SetContextLogLabel("observe")
	return client
}

func TestResponsesEmitsSchemaAtInfoLevel(t *testing.T) {
	for _, level := range []config.LogLevel{config.InfoLogLevel, config.DebugLogLevel, config.ErrorLogLevel} {
		t.Run(string(level), func(t *testing.T) {
			client := newOutputTestClient(t, outputResponse, false)
			client.SetLogLevel(level)
			var output recordedOutput
			client.SetOutputer(&output)
			if _, _, _, err := client.RunWithToolMessages(context.Background(), "output-task", "inspect", nil, nil); err != nil {
				t.Fatal(err)
			}
			events := output.ofKind(OutputSchema)
			if level == config.ErrorLogLevel {
				if len(events) != 0 {
					t.Fatalf("schema events at error level: %v", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("schema events=%v", events)
			}
			var schema struct {
				Type       string                     `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(events[0].Data, &schema); err != nil {
				t.Fatal(err)
			}
			if schema.Type != "object" || schema.Properties["answer"] == nil || events[0].TaskID != "output-task" || events[0].Label != "observe" {
				t.Fatalf("schema event=%+v", events[0])
			}
		})
	}
}

func TestResponsesEmitsContextAtInfoLevel(t *testing.T) {
	for _, level := range []config.LogLevel{config.InfoLogLevel, config.DebugLogLevel, config.ErrorLogLevel} {
		t.Run(string(level), func(t *testing.T) {
			client := newOutputTestClient(t, outputResponse, false)
			client.SetLogLevel(level)
			var output recordedOutput
			client.SetOutputer(&output)
			if _, _, _, err := client.RunWithToolMessages(context.Background(), "output-task", "inspect", nil, nil); err != nil {
				t.Fatal(err)
			}
			events := output.ofKind(OutputContext)
			if level == config.ErrorLogLevel {
				if len(events) != 0 {
					t.Fatalf("context events at error level: %v", events)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("context events=%v", events)
			}
			var data ContextOutput
			if err := json.Unmarshal(events[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(data.Context, "input: inspect") || events[0].TaskID != "output-task" || events[0].Label != "observe" {
				t.Fatalf("context event=%+v", events[0])
			}
		})
	}
}

func TestResponsesEmitsToolCall(t *testing.T) {
	client := newOutputTestClient(t, `{"status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"inspect","arguments":"{\"path\":\"a.go\"}"}]}`, false)
	var output recordedOutput
	client.SetOutputer(&output)
	_, calls, _, err := client.RunWithToolMessages(context.Background(), "output-task", "inspect", nil, nil)
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
	events := output.ofKind(OutputToolCall)
	if len(events) != 1 {
		t.Fatalf("tool events=%v", events)
	}
	var data ToolCallOutput
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Name != "inspect" || data.CallID != "call-1" || data.Arguments != `{"path":"a.go"}` || events[0].TaskID != "output-task" || events[0].Label != "observe" {
		t.Fatalf("tool event=%+v", events[0])
	}
}

func TestResponsesEmitsUsage(t *testing.T) {
	client := newOutputTestClient(t, outputResponse, false)
	var output recordedOutput
	client.SetOutputer(&output)
	if _, _, _, err := client.RunWithToolMessages(context.Background(), "output-task", "inspect", nil, nil); err != nil {
		t.Fatal(err)
	}
	events := output.ofKind(OutputUsage)
	if len(events) != 1 {
		t.Fatalf("usage events=%v", events)
	}
	var data UsageOutput
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	want := UsageOutput{Model: "test-model", ResponseID: "resp_output", Status: "completed", InputTokens: "20", OutputTokens: "5", CachedInputTokens: "4", UncachedInputTokens: "16"}
	if data != want || events[0].TaskID != "output-task" || events[0].Label != "observe" {
		t.Fatalf("usage event=%+v data=%+v", events[0], data)
	}
	if len(output.events) != 2 || output.events[0].Kind != OutputUsage || output.events[1].Kind != OutputText {
		t.Fatalf("event order=%v", output.events)
	}
}

func TestResponsesEmitsStreamingDeltaToCustomOutputAtEveryLevel(t *testing.T) {
	for _, level := range []config.LogLevel{config.DebugLogLevel, config.InfoLogLevel, config.ErrorLogLevel} {
		t.Run(string(level), func(t *testing.T) {
			client := newOutputTestClient(t, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.completed\",\"response\":"+outputResponse+"}\n\n", true)
			client.SetLogLevel(level)
			var output recordedOutput
			client.SetOutputer(&output)
			result, _, _, err := client.RunWithToolMessages(context.Background(), "output-task", "inspect", nil, nil)
			if err != nil || result.Answer != "ok" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			events := output.ofKind(OutputTextDelta)
			if len(events) != 1 {
				t.Fatalf("delta events=%v", events)
			}
			var data TextOutput
			if err := json.Unmarshal(events[0].Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.Text != "partial" || events[0].TaskID != "output-task" || events[0].Label != "observe" {
				t.Fatalf("delta event=%+v", events[0])
			}
			if len(output.ofKind(OutputText)) != 1 || len(output.ofKind(OutputUsage)) != 1 {
				t.Fatalf("terminal events=%v", output.events)
			}
		})
	}
}

type failingOutput struct{}

func (failingOutput) Emit(context.Context, OutputEvent) error {
	return errors.New("output unavailable")
}

func TestResponsesOutputFailurePreservesResultWithoutRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, outputResponse)
	}))
	defer server.Close()
	client, err := NewResponsesApiClient[outputAnswer](&config.LLMConfig{BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetLogLevel(config.DebugLogLevel)
	client.SetOutputer(failingOutput{})
	var diagnostics bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&diagnostics)
	defer log.SetOutput(previous)
	result, _, _, err := client.RunWithToolMessages(context.Background(), "output-task", "inspect", nil, nil)
	if err != nil || result.Answer != "ok" || requests.Load() != 1 {
		t.Fatalf("result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
	for _, kind := range []OutputKind{OutputSchema, OutputContext, OutputUsage, OutputText} {
		if !strings.Contains(diagnostics.String(), "emit output kind="+string(kind)+": output unavailable") {
			t.Fatalf("missing %s diagnostic: %s", kind, diagnostics.String())
		}
	}
}

func TestResponsesDefaultOutputUsesStderr(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprint(reset), func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			previous := os.Stderr
			os.Stderr = file
			defer func() { os.Stderr = previous }()
			client := newOutputTestClient(t, outputResponse, false)
			var output recordedOutput
			if reset {
				client.SetOutputer(&output)
				client.SetOutputer(nil)
			}
			if _, _, _, err := client.RunWithToolMessages(context.Background(), "output-task", "inspect", nil, nil); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(file.Name())
			if err != nil {
				t.Fatal(err)
			}
			if len(output.events) != 0 || strings.Count(string(data), "LLM usage") != 1 || strings.Count(string(data), "LLM response text=") != 1 {
				t.Fatalf("default stderr=%s replaced events=%v", data, output.events)
			}
		})
	}
}

func TestResponsesEmitsText(t *testing.T) {
	client := newOutputTestClient(t, outputResponse, false)
	var output recordedOutput
	client.SetOutputer(&output)
	result, calls, _, err := client.RunWithToolMessages(context.Background(), "output-task", "inspect", nil, nil)
	if err != nil || result.Answer != "ok" || len(calls) != 0 {
		t.Fatalf("result=%+v calls=%v err=%v", result, calls, err)
	}
	events := output.ofKind(OutputText)
	if len(events) != 1 {
		t.Fatalf("text events=%v", events)
	}
	var data TextOutput
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if events[0].TaskID != "output-task" || events[0].Label != "observe" || data.Text != `{"answer":"ok"}` {
		t.Fatalf("event=%+v data=%+v", events[0], data)
	}
}
