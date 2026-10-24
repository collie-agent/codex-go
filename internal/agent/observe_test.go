package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/provider"
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
				formatMarker := "Output format: JSON."
				if tc.schema == config.LineProtocolLLMOutputSchema {
					formatMarker = "Final text output format: Line Protocol."
				}
				if strings.Count(request.Input, formatMarker) != 1 {
					t.Error("expected exactly one observation format contract")
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
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.ConfirmedFacts) == 0 && len(got.ReportedErrors) == 0 && string(encoded) != "{}" {
				t.Fatalf("empty observation must omit fields, got %s", encoded)
			}
			if rc.Format() != before {
				t.Fatal("observer changed the input context")
			}
		})
	}
}

func TestObserveExecDecisionRelevance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema config.LLMOutputSchema
		output string
		want   ObserveResult
	}{
		{"facts only", config.JsonLLMOutputSchema, `{"ConfirmedFacts":["exec call_exec: all tests passed"],"ReportedErrors":[]}`, ObserveResult{ConfirmedFacts: []string{"exec call_exec: all tests passed"}, ReportedErrors: []string{}}},
		{"errors only", config.JsonLLMOutputSchema, `{"ConfirmedFacts":[],"ReportedErrors":["exec call_exec: missing dependency"]}`, ObserveResult{ConfirmedFacts: []string{}, ReportedErrors: []string{"exec call_exec: missing dependency"}}},
		{"irrelevant json", config.JsonLLMOutputSchema, `{"ConfirmedFacts":[],"ReportedErrors":[]}`, ObserveResult{ConfirmedFacts: []string{}, ReportedErrors: []string{}}},
		{"irrelevant line protocol", config.LineProtocolLLMOutputSchema, "BEGIN OBSERVATION\nEND OBSERVATION", ObserveResult{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				for _, want := range []string{"exec", "call_exec", `{"command":"npm","args":["test"]}`, "command output", "影响后续决策", "忽略重复信息、无关日志", "空数组"} {
					if !strings.Contains(request.Input, want) {
						t.Errorf("exec observation input missing %q: %s", want, request.Input)
					}
				}
				writeObservationResponse(w, []any{observationMessage(tc.output)})
			}))
			defer server.Close()
			state := &fileContext.DurableState{RunId: "observe-exec", Goal: "verify tests"}
			observer := NewObserveProcess[ObserveResult](t.Context(), fileContext.NewObservationContext(state, nil, &fileContext.ToolResult{
				ToolName: "exec", ToolCallId: "call_exec", Arguments: `{"command":"npm","args":["test"]}`, Result: "command output",
			}))
			got, _, err := observer.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{
				ApiKey: "test", BaseUrl: server.URL, Model: "test", OutputSchema: tc.schema,
			}}, state)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestObserveRunResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output []any
		want   string
		calls  int
	}{
		{"invalid output", []any{observationMessage("invalid JSON")}, "output format errors exceeded limit (3)", 4},
		{"empty output", nil, "response output is empty", 1},
		{"tool call", []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read_file", "arguments": `{}`}}, "unexpected tool calls", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				writeObservationResponse(w, tc.output)
			}))
			defer server.Close()
			agentConfig := &config.AgentConfig{LLMConfig: &config.LLMConfig{
				ApiKey: "test", BaseUrl: server.URL, Model: "test-model", OutputSchema: config.JsonLLMOutputSchema,
			}}
			state := &fileContext.DurableState{RunId: "observe-task"}
			observer := NewObserveProcess[ObserveResult](context.Background(), fileContext.NewObservationContext(state, nil, nil))
			got, _, err := observer.Run(agentConfig, state)
			if calls != tc.calls {
				t.Fatalf("requests = %d, want %d", calls, tc.calls)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "observe-task") {
				t.Fatalf("got error %v, want task ID and %q", err, tc.want)
			}
			if !reflect.DeepEqual(got, ObserveResult{}) {
				t.Fatalf("failed observation returned a result: %+v", got)
			}
		})
	}
}

func TestObserveRetriesOutputParseErrors(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, recover := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/recover=%t", schema, recover), func(t *testing.T) {
				bad, good := `{"ConfirmedFacts":["broken`, `{"ConfirmedFacts":["verified"],"ReportedErrors":[]}`
				wantError := "unexpected end of JSON input"
				if schema == config.LineProtocolLLMOutputSchema {
					bad = "BEGIN OBSERVATION\nCONFIRMEDFACTS=\"broken\nfact\"\nEND OBSERVATION"
					good = "BEGIN OBSERVATION\nCONFIRMEDFACTS=\"verified\"\nEND OBSERVATION"
					wantError = "invalid JSON literal for CONFIRMEDFACTS"
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					calls++
					var request struct {
						Input string `json:"input"`
					}
					if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					for _, want := range []string{"inspect notes", "previous fact", "previous error", "call_read", "hello", "custom observer prompt"} {
						if !strings.Contains(request.Input, want) {
							t.Errorf("input missing %q: %s", want, request.Input)
						}
					}
					// 每次重试保留此前所有纠错信息及被引用的原始输出。
					for attempt := 1; attempt < calls; attempt++ {
						for _, want := range []string{fmt.Sprintf("LLM output format error (%d/3)", attempt), wantError, fmt.Sprintf("%q", bad), "Retry using the required output schema"} {
							if !strings.Contains(request.Input, want) {
								t.Errorf("retry input missing %q: %s", want, request.Input)
							}
						}
					}
					text := bad
					if recover && calls == 4 {
						text = good
					}
					writeObservationResponse(w, []any{observationMessage(text)})
				}))
				defer server.Close()
				state := &fileContext.DurableState{RunId: "observe-retry", Goal: "inspect notes"}
				rc := fileContext.NewObservationContext(state,
					&fileContext.Observation{ConfirmedFacts: []string{"previous fact"}, ReportedErrors: []string{"previous error"}},
					&fileContext.ToolResult{ToolName: "read_file", ToolCallId: "call_read", Result: "hello"})
				before := rc.Format()
				cfg := &config.AgentConfig{SysPrompt: "custom observer prompt", LLMConfig: &config.LLMConfig{
					BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema,
				}}
				got, next, err := NewObserveProcess[ObserveResult](t.Context(), rc).Run(cfg, state)
				if calls != 4 {
					t.Fatalf("requests = %d, want 4; err = %v", calls, err)
				}
				if rc.Format() != before {
					t.Fatal("retry changed the input observation context")
				}
				if recover {
					if err != nil || !reflect.DeepEqual(got.ConfirmedFacts, []string{"verified"}) || len(got.ReportedErrors) != 0 || len(next) != 1 || next[0] != EndAgentType {
						t.Fatalf("Run() = %+v, %v, %v", got, next, err)
					}
				} else {
					var parseErr *provider.OutputParseError
					if err == nil || !strings.Contains(err.Error(), "output format errors exceeded limit (3)") || !errors.As(err, &parseErr) || parseErr.Output != bad || !reflect.DeepEqual(got, ObserveResult{}) || len(next) != 0 {
						t.Fatalf("Run() = %+v, %v, %v", got, next, err)
					}
				}
			})
		}
	}
}

func TestObserveDoesNotRetryRequestErrors(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()
	state := &fileContext.DurableState{RunId: "request-error"}
	cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{
		BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema,
	}}
	_, _, err := NewObserveProcess[ObserveResult](t.Context(), fileContext.NewObservationContext(state, nil, nil)).Run(cfg, state)
	if err == nil || !strings.Contains(err.Error(), "create response") || calls != 1 {
		t.Fatalf("requests = %d, err = %v", calls, err)
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
