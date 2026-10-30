package provider_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/provider"
)

type answerSink struct{ chunks chan string }

func (s answerSink) Emit(_ context.Context, e provider.OutputEvent) error {
	if e.Kind == "answer_delta" {
		var v struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(e.Data, &v); err != nil {
			return err
		}
		s.chunks <- v.Text
	}
	return nil
}

func checkStreamingAnswer(t *testing.T, schema config.LLMOutputSchema, document, want string) {
	t.Helper()
	chunks := make(chan string, len(document)+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Deliberately split every byte, including field names and escapes.
		for i := range len(document) {
			data, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": document[i : i+1]})
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		var got strings.Builder
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for got.String() != want {
			select {
			case part := <-chunks:
				got.WriteString(part)
			case <-timer.C:
				t.Errorf("before completion: answer=%q, want %q", got.String(), want)
				return
			}
		}
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": document}}}}}})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer server.Close()
	client, err := provider.NewResponsesApiClient[struct {
		Answer string `json:"answer" lineprotocol:"ANSWER"`
		Status string `json:"status" lineprotocol:"STATUS"`
		Result *struct {
			Answer string `json:"answer" lineprotocol:"ANSWER"`
		} `json:"result" lineprotocol:"RESULT"`
	}](&config.LLMConfig{Stream: true, BaseUrl: server.URL, Model: "test", OutputSchema: schema}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetOutputer(answerSink{chunks})
	result, _, _, err := client.Run(t.Context(), "answer-test", "question", nil)
	if result.Result != nil {
		result.Answer = result.Result.Answer
	}
	if err != nil || result.Answer != want {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
func TestStreamsAnswerBeforeCompletion(t *testing.T) {
	checkStreamingAnswer(t, config.JsonLLMOutputSchema, `{"answer":"hello"}`, "hello")
}

func TestStreamsEscapedAnswer(t *testing.T) {
	checkStreamingAnswer(t, config.JsonLLMOutputSchema, `{"note":{"answer":"wrong"},"answer":"hello\n\"x\"\\\u4f60\u597d\ud83d\ude00"}`, "hello\n\"x\"\\你好😀")
}

func TestStreamsLineProtocolAnswer(t *testing.T) {
	checkStreamingAnswer(t, config.LineProtocolLLMOutputSchema, "BEGIN RESULT\nANSWER=\"hello\\n\\u4f60\\u597d\"\nEND RESULT", "hello\n你好")
}

func TestPrintsAnswerDelta(t *testing.T) {
	var output strings.Builder
	err := provider.NewStderrOutputer(&output).Emit(t.Context(), provider.OutputEvent{TaskID: "answer", Kind: provider.OutputAnswerDelta, Data: json.RawMessage(`{"text":"你好"}`)})
	if err != nil || !strings.Contains(output.String(), "LLM answer delta=你好") {
		t.Fatalf("output=%q err=%v", output.String(), err)
	}
}

func TestStreamsNestedResultAnswer(t *testing.T) {
	checkStreamingAnswer(t, config.JsonLLMOutputSchema, `{"result":{"answer":"nested"}}`, "nested")
}

func TestStreamsNestedLineResultAnswer(t *testing.T) {
	checkStreamingAnswer(t, config.LineProtocolLLMOutputSchema, "begin_result\r\nbegin_result\r\nanswer=\"nested\"\r\nend_result\r\nend_result", "nested")
}

func TestStreamsSingleLineProtocolAnswer(t *testing.T) {
	checkStreamingAnswer(t, config.LineProtocolLLMOutputSchema, `BEGIN RESULT STATUS="completed" ANSWER="XXX" END RESULT`, "XXX")
}

func TestSingleLineProtocolPreservesQuotedContent(t *testing.T) {
	checkStreamingAnswer(t, config.LineProtocolLLMOutputSchema, `BEGIN RESULT  STATUS="completed"   ANSWER="a b END RESULT ANSWER=\"fake\"\n\u4f60\ud83d\ude00"  END RESULT`, "a b END RESULT ANSWER=\"fake\"\n你😀")
}

func TestStreamsSingleLineNestedResult(t *testing.T) {
	checkStreamingAnswer(t, config.LineProtocolLLMOutputSchema, `BEGIN_RESULT BEGIN_RESULT ANSWER="nested answer" END_RESULT END_RESULT`, "nested answer")
}
