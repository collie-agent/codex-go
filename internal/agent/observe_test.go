package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestObserveRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema config.LLMOutputSchema
		output string
		want   ObserveResult
	}{
		{"json", config.JsonLLMOutputSchema, `{"ConfirmedFacts":["read_file call_read: found notes.txt","read_file call_read: content is hello"],"ReportedErrors":["read_file call_read: permission denied"]}`, ObserveResult{ConfirmedFacts: []string{"read_file call_read: found notes.txt", "read_file call_read: content is hello"}, ReportedErrors: []string{"read_file call_read: permission denied"}}},
		{"line protocol", config.LineProtocolLLMOutputSchema, "BEGIN OBSERVATION\nCONFIRMEDFACTS=\"read_file call_read: found notes.txt\"\nCONFIRMEDFACTS=\"read_file call_read: content is hello\"\nREPORTEDERRORS=\"read_file call_read: permission denied\"\nEND OBSERVATION", ObserveResult{ConfirmedFacts: []string{"read_file call_read: found notes.txt", "read_file call_read: content is hello"}, ReportedErrors: []string{"read_file call_read: permission denied"}}},
		{"empty json", config.JsonLLMOutputSchema, `{"ConfirmedFacts":[],"ReportedErrors":[]}`, ObserveResult{ConfirmedFacts: []string{}, ReportedErrors: []string{}}},
		{"empty line protocol", config.LineProtocolLLMOutputSchema, "BEGIN OBSERVATION\nEND OBSERVATION", ObserveResult{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input string            `json:"input"`
					Tools []json.RawMessage `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(request.Tools) != 0 {
					t.Errorf("observation exposed tools: %s", request.Tools)
				}
				for _, want := range []string{"工具结果观察器", "inspect notes", "notes inspected", "previous fact", "previous error", "call_read", "read_file", `{"path":"notes.txt"}`, "hello", "permission denied"} {
					if !strings.Contains(request.Input, want) {
						t.Errorf("observation input missing %q: %s", want, request.Input)
					}
				}
				if strings.Contains(request.Input, "单轮决策器") {
					t.Error("observation contains worker instructions")
				}
				writeObservationResponse(w, []any{observationMessage(tc.output)})
			}))
			defer server.Close()
			rc := fileContext.NewObservationContext(
				&fileContext.DurableState{RunId: "observe-task", Goal: "inspect notes", CompletionCriteria: []string{"notes inspected"}},
				&fileContext.Observation{ConfirmedFacts: []string{"previous fact"}, ReportedErrors: []string{"previous error"}},
				&fileContext.ToolResult{ToolCallId: "call_read", ToolName: "read_file", Status: filemcp.ToolCallFailedStatus, Arguments: `{"path":"notes.txt"}`, Result: "partial content: hello", Error: "permission denied"},
			)
			state := rc.DurableState
			rc.DurableState = &fileContext.DurableState{RunId: "old-task", Goal: "old goal"}
			before := rc.Format()
			agentConfig := &config.AgentConfig{LLMConfig: &config.LLMConfig{
				ApiKey: "test", BaseUrl: server.URL, Model: "test-model", OutputSchema: tc.schema,
			}}
			var observer AgentProcessor[ObserveResult] = NewObserveProcess[ObserveResult](context.Background(), rc)
			got, _, err := observer.Run(agentConfig, state)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if rc.Format() != before {
				t.Fatal("observer changed the input context")
			}
		})
	}
}

func TestObserveRunResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output []any
		want   string
	}{
		{"invalid output", []any{observationMessage("invalid JSON")}, "parse response output"},
		{"empty output", nil, "response output is empty"},
		{"tool call", []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read_file", "arguments": `{}`}}, "unexpected tool calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeObservationResponse(w, tc.output)
			}))
			defer server.Close()
			agentConfig := &config.AgentConfig{LLMConfig: &config.LLMConfig{
				ApiKey: "test", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema,
			}}
			state := &fileContext.DurableState{RunId: "observe-task"}
			observer := NewObserveProcess[ObserveResult](context.Background(), fileContext.NewObservationContext(state, nil, nil))
			got, _, err := observer.Run(agentConfig, state)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "observe-task") {
				t.Fatalf("got error %v, want task ID and %q", err, tc.want)
			}
			if !reflect.DeepEqual(got, ObserveResult{}) {
				t.Fatalf("failed observation returned a result: %+v", got)
			}
		})
	}
}

func TestObserveRunInvalidInput(t *testing.T) {
	state := &fileContext.DurableState{RunId: "observe-task"}
	validContext := fileContext.NewObservationContext(state, nil, nil)
	validConfig := &config.AgentConfig{LLMConfig: &config.LLMConfig{}}
	for _, tc := range []struct {
		name     string
		observer *ObserveProcess[ObserveResult]
		config   *config.AgentConfig
		state    *fileContext.DurableState
	}{
		{"nil observer", nil, validConfig, state},
		{"nil config", NewObserveProcess[ObserveResult](context.Background(), validContext), nil, state},
		{"nil context", NewObserveProcess[ObserveResult](nil, validContext), validConfig, state},
		{"nil observation context", NewObserveProcess[ObserveResult](context.Background(), nil), validConfig, state},
		{"nil durable state", NewObserveProcess[ObserveResult](context.Background(), validContext), validConfig, nil},
		{"nil LLM config", NewObserveProcess[ObserveResult](context.Background(), validContext), &config.AgentConfig{}, state},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := tc.observer.Run(tc.config, tc.state); err == nil {
				t.Fatal("expected an input error")
			}
		})
	}
}

func observationMessage(output string) map[string]any {
	return map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": []any{
		map[string]any{"type": "output_text", "text": output, "annotations": []any{}},
	}}
}

func writeObservationResponse(w http.ResponseWriter, output []any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_observe", "status": "completed", "output": output})
}
