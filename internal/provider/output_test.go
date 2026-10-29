package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/provider"
)

// 检查实际输出的时间前缀，再由格式测试断言正文，避免依赖固定时钟。
func withoutLogTime(t *testing.T, output string) string {
	t.Helper()
	const layout = "2006/01/02 15:04:05"
	if len(output) <= len(layout) || output[len(layout)] != ' ' {
		t.Fatalf("missing log timestamp: %q", output)
	}
	if _, err := time.ParseInLocation(layout, output[:len(layout)], time.Local); err != nil {
		t.Fatalf("invalid log timestamp: %q: %v", output, err)
	}
	return output[len(layout)+1:]
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestStderrOutputerWriteError(t *testing.T) {
	want := errors.New("stderr unavailable")
	err := provider.NewStderrOutputer(failingWriter{want}).Emit(context.Background(), provider.OutputEvent{
		Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`),
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestStderrOutputerDefaultsToStderr(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	previous := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = previous }()
	if err := provider.NewStderrOutputer(nil).Emit(context.Background(), provider.OutputEvent{
		TaskID: "default", Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := "task default: LLM response text=hello\n"; withoutLogTime(t, string(data)) != want {
		t.Fatalf("stderr = %q, want %q", data, want)
	}
}

func TestStderrOutputerRejectsInvalidEventsWithoutWriting(t *testing.T) {
	for _, event := range []provider.OutputEvent{
		{Kind: "unsupported", Data: json.RawMessage(`{}`)},
		{Kind: provider.OutputText, Data: json.RawMessage(`{"text":`)},
		{Kind: provider.OutputSchema, Data: json.RawMessage(`{`)},
	} {
		var buf bytes.Buffer
		if err := provider.NewStderrOutputer(&buf).Emit(context.Background(), event); err == nil {
			t.Fatalf("expected error for %+v", event)
		}
		if buf.Len() != 0 {
			t.Fatalf("invalid event wrote %q", buf.String())
		}
	}
}

func TestStderrOutputerConcurrentEvents(t *testing.T) {
	var buf bytes.Buffer
	output := provider.NewStderrOutputer(&buf)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if err := output.Emit(context.Background(), provider.OutputEvent{
				TaskID: "parallel", Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`),
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 100 {
		t.Fatalf("got %d events, want 100", len(lines))
	}
	for _, line := range lines {
		if got := withoutLogTime(t, line); got != "task parallel: LLM response text=hello" {
			t.Fatalf("interleaved event: %q", line)
		}
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestStderrOutputerShortWrite(t *testing.T) {
	err := provider.NewStderrOutputer(shortWriter{}).Emit(context.Background(), provider.OutputEvent{
		Kind: provider.OutputText, Data: json.RawMessage(`{"text":"hello"}`),
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want short write", err)
	}
}

func TestStderrOutputerContext(t *testing.T) {
	var buf bytes.Buffer
	output := provider.NewStderrOutputer(&buf)
	if err := output.Emit(context.Background(), provider.OutputEvent{TaskID: "task-1", Kind: provider.OutputContext, Data: json.RawMessage(`{"context":"user: inspect"}`)}); err != nil {
		t.Fatal(err)
	}
	if want := "task task-1: LLM context=\nuser: inspect\n"; withoutLogTime(t, buf.String()) != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}

func TestStderrOutputerTextDelta(t *testing.T) {
	var buf bytes.Buffer
	output := provider.NewStderrOutputer(&buf)
	if err := output.Emit(context.Background(), provider.OutputEvent{TaskID: "task-1", Kind: provider.OutputTextDelta, Data: json.RawMessage(`{"text":"hi"}`)}); err != nil {
		t.Fatal(err)
	}
	if want := "task task-1: LLM response text delta=hi\n"; withoutLogTime(t, buf.String()) != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}

func TestStderrOutputerSchema(t *testing.T) {
	var buf bytes.Buffer
	output := provider.NewStderrOutputer(&buf)
	if err := output.Emit(context.Background(), provider.OutputEvent{TaskID: "task-1", Kind: provider.OutputSchema, Data: json.RawMessage(`{"type":"object"}`)}); err != nil {
		t.Fatal(err)
	}
	if want := "task task-1: JSON schema=\n{\n  \"type\": \"object\"\n}\n"; withoutLogTime(t, buf.String()) != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}

func TestStderrOutputerToolCall(t *testing.T) {
	var buf bytes.Buffer
	output := provider.NewStderrOutputer(&buf)
	if err := output.Emit(context.Background(), provider.OutputEvent{TaskID: "task-1", Kind: provider.OutputToolCall, Data: json.RawMessage(`{"name":"read","call_id":"call-1","arguments":"{}"}`)}); err != nil {
		t.Fatal(err)
	}
	if want := "task task-1: tool calling name=read call_id=call-1 arguments={}\n"; withoutLogTime(t, buf.String()) != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}

func TestStderrOutputerUsage(t *testing.T) {
	var buf bytes.Buffer
	output := provider.NewStderrOutputer(&buf)
	if err := output.Emit(context.Background(), provider.OutputEvent{TaskID: "task-1", Kind: provider.OutputUsage, Data: json.RawMessage(`{"model":"gpt-test","status":"completed","input_tokens":"0"}`)}); err != nil {
		t.Fatal(err)
	}
	if want := "task task-1: LLM usage model=gpt-test status=completed input_tokens=0 output_tokens=unknown cached_input_tokens=unknown uncached_input_tokens=unknown\n"; withoutLogTime(t, buf.String()) != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}

func TestStderrOutputerToolOutcomes(t *testing.T) {
	for _, tc := range []struct {
		kind provider.OutputKind
		want string
	}{
		{provider.OutputToolResult, "task task-1: tool calling result: name=read_file call_id=read result=[]\n"},
		{provider.OutputToolError, "task task-1: tool calling error: name=read_file call_id=read error=missing result=[]\n"},
	} {
		var buf bytes.Buffer
		if err := provider.NewStderrOutputer(&buf).Emit(t.Context(), provider.OutputEvent{TaskID: "task-1", Kind: tc.kind, Data: json.RawMessage(`{"name":"read_file","call_id":"read","result":[],"error":"missing"}`)}); err != nil {
			t.Fatal(err)
		}
		if got := withoutLogTime(t, buf.String()); got != tc.want {
			t.Fatalf("output=%q want=%q", got, tc.want)
		}
	}
}

func TestStderrOutputerText(t *testing.T) {
	var buf bytes.Buffer
	var output provider.Outputer = provider.NewStderrOutputer(&buf)
	err := output.Emit(context.Background(), provider.OutputEvent{
		TaskID: "task-1", Label: "planner", Kind: provider.OutputText,
		Data: json.RawMessage(`{"text":"hello\nworld"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "task task-1: [planner] LLM response text=hello\nworld\n"; withoutLogTime(t, buf.String()) != want {
		t.Fatalf("output = %q, want %q", buf.String(), want)
	}
}
