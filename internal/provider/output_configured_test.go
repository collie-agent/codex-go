package provider

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
)

// 正文增量只在 debug 级别回显到 stderr，但无论级别都必须写入 Stream。
func TestConfiguredOutputDeltaLogLevel(t *testing.T) {
	for _, kind := range []OutputKind{OutputTextDelta, OutputAnswerDelta, OutputReset} {
		for _, level := range []config.LogLevel{config.DebugLogLevel, config.InfoLogLevel, config.ErrorLogLevel} {
			t.Run(string(kind)+"/"+string(level), func(t *testing.T) {
				var stderr bytes.Buffer
				stream := &recordedOutput{}
				output := &configuredOutputer{stderr: NewStderrOutputer(&stderr), stream: stream, level: level}
				if err := output.Emit(t.Context(), OutputEvent{TaskID: "delta", Kind: kind, Data: json.RawMessage(`{"text":"partial"}`)}); err != nil {
					t.Fatal(err)
				}
				if want := kind == OutputReset || level == config.DebugLogLevel; (stderr.Len() > 0) != want {
					t.Fatalf("stderr=%q, want visible=%v", stderr.String(), want)
				}
				if len(stream.ofKind(kind)) != 1 {
					t.Fatalf("stream events=%+v", stream.events)
				}
			})
		}
	}
}

// 撤回属于失败诊断，默认 stderr 路径在 error 级别也必须打印。
func TestDefaultOutputResetLogsAtEveryLevel(t *testing.T) {
	for _, level := range []config.LogLevel{config.DebugLogLevel, config.InfoLogLevel, config.ErrorLogLevel} {
		t.Run(string(level), func(t *testing.T) {
			var stderr bytes.Buffer
			client := &baseResponsesApiClient[outputAnswer]{logLevel: level, stderr: NewStderrOutputer(&stderr)}
			client.emitOutput(t.Context(), "reset-task", OutputReset, OutputResetOutput{TextBytes: 42, AnswerBytes: 6})
			if !strings.Contains(stderr.String(), "LLM output reset text_bytes=42 answer_bytes=6") {
				t.Fatalf("missing reset diagnostic: %q", stderr.String())
			}
		})
	}
}
