package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	mcpSdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReactObserveAsync(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan string, 1)
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var request struct {
			Input json.RawMessage `json:"input"`
		}
		if err := decodeModelRequest(req.Body, &request); err != nil {
			t.Error(err)
			return
		}
		started <- string(request.Input)
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":["patch call_patch: file updated"],"ReportedErrors":["patch call_patch: partial result"]}`)})
	}))
	defer server.Close()
	r := &ReactProcess[ReactResult]{BaseReactProcess: BaseReactProcess[ReactResult]{ctx: ctx}}
	agentConfig := &config.AgentConfig{LLMConfig: &config.LLMConfig{
		BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema,
	}}
	rc := fileContext.NewReactContext(&fileContext.DurableState{RunId: "react-task", Goal: "update file"})
	rc.AppendConfirmedFacts("existing fact")
	done := make(chan struct{})
	go func() {
		r.updateContext(&mcpSdk.CallToolResult{Content: []mcpSdk.Content{&mcpSdk.TextContent{Text: "updated"}}},
			&mcpSdk.CallToolParams{Name: "apply_patch", Meta: mcpSdk.Meta{toolCallIDMetaKey: "call_patch"}}, rc, nil, agentConfig)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("context update blocked on observation")
	}
	select {
	case input := <-started:
		for _, want := range []string{"update file", "existing fact", "apply_patch", "call_patch", "updated"} {
			if !strings.Contains(input, want) {
				t.Errorf("observation input missing %q: %s", want, input)
			}
		}
	case <-ctx.Done():
		t.Fatal("observation did not start")
	}
	// Formatting and new facts must remain available while the request waits.
	rc.AppendConfirmedFacts("fact added during observation")
	_, results := rc.FormatForModel()
	if len(results) != 1 || !strings.Contains(results[0].Result, "updated") {
		t.Fatal("raw result missing while observation is pending")
	}
	release <- struct{}{}
	waitForReactObservation(t, ctx, rc, "patch call_patch: partial result")
	for _, want := range []string{"existing fact", "fact added during observation", "patch call_patch: file updated"} {
		if !strings.Contains(rc.Format(), want) {
			t.Errorf("context missing %q", want)
		}
	}
}

func TestReactExecObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var request struct {
			Input string `json:"input"`
		}
		if err := decodeModelRequest(req.Body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, want := range []string{"verify tests", "exec", "call_exec", `{"args":["test"],"command":"npm"}`, "FAIL: TestExample", "exit_code", "影响后续决策"} {
			if !strings.Contains(request.Input, want) {
				t.Errorf("exec observation input missing %q: %s", want, request.Input)
			}
		}
		writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":["exec call_exec: TestExample failed"],"ReportedErrors":["exec call_exec: tests exited with code 1"]}`)})
	}))
	defer server.Close()
	r := &ReactProcess[ReactResult]{BaseReactProcess: BaseReactProcess[ReactResult]{ctx: ctx}}
	rc := fileContext.NewReactContext(&fileContext.DurableState{RunId: "exec-task", Goal: "verify tests"})
	r.updateContext(&mcpSdk.CallToolResult{Content: []mcpSdk.Content{&mcpSdk.TextContent{Text: `{"stdout":"FAIL: TestExample","exit_code":1}`}}},
		&mcpSdk.CallToolParams{Name: "exec", Meta: mcpSdk.Meta{toolCallIDMetaKey: "call_exec"}, Arguments: map[string]any{"command": "npm", "args": []string{"test"}}},
		rc, nil, &config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}})
	waitForReactObservation(t, ctx, rc, "exec call_exec: tests exited with code 1")
	if !strings.Contains(rc.Format(), "exec call_exec: TestExample failed") {
		t.Fatal("exec observation facts were not merged into ReAct context")
	}
}

func TestReactRetriesOutputParseErrors(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, recover := range []bool{true, false} {
			name := string(schema) + "/limit"
			if recover {
				name = string(schema) + "/recover"
			}
			t.Run(name, func(t *testing.T) {
				bad, good := `{"answer":"broken`, `{"status":"completed","answer":"done"}`
				wantError := "unexpected end of JSON input"
				if schema == config.LineProtocolLLMOutputSchema {
					bad = "BEGIN RESULT\nANSWER=\"broken\nanswer\"\nEND RESULT"
					good = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
					wantError = "invalid JSON literal for ANSWER"
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					calls++
					var request struct {
						Input []struct {
							Role    string `json:"role"`
							Content string `json:"content"`
						} `json:"input"`
					}
					if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if len(request.Input) != calls+1 {
						t.Errorf("messages = %d, want %d (system, state, corrections)", len(request.Input), calls+1)
					}
					for index, message := range request.Input {
						if index < 2 {
							if strings.Contains(message.Content, "LLM output format error") || strings.Contains(message.Content, "Previous invalid response") {
								t.Errorf("correction mixed into system/state: %s", message.Content)
							}
							continue
						}
						if message.Role != "user" {
							t.Errorf("correction role = %q, want user", message.Role)
						}
						for _, want := range []string{wantError, "Retry", "broken"} {
							if !strings.Contains(message.Content, want) {
								t.Errorf("correction missing %q: %s", want, message.Content)
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
				// 最后一轮也应允许纠正格式，不消耗工具执行的轮数预算。
				process := &ReactProcess[ReactResult]{BaseReactProcess: BaseReactProcess[ReactResult]{ctx: t.Context(), maxTurn: 1}}
				cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{
					BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema,
				}}
				got, next, err := process.Run(cfg, &fileContext.DurableState{CompletionCriteria: []string{"original task verified"}, RunId: "retry-task", Goal: "answer"})
				if calls != 4 {
					t.Fatalf("requests = %d, want 4; err = %v", calls, err)
				}
				if recover {
					if err != nil || got.Answer != "done" || len(next) != 1 || next[0] != EndAgentType {
						t.Fatalf("Run() = %+v, %v, %v", got, next, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "output format errors exceeded limit (3)") || !strings.Contains(err.Error(), wantError) {
					t.Fatalf("unexpected error: %v", err)
				}
			})
		}
	}
}

func TestReactDoesNotRetryRequestErrors(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()
	process := &ReactProcess[ReactResult]{BaseReactProcess: BaseReactProcess[ReactResult]{ctx: t.Context(), maxTurn: 5}}
	cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{
		BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema,
	}}
	_, _, err := process.Run(cfg, &fileContext.DurableState{CompletionCriteria: []string{"original task verified"}, RunId: "request-error"})
	if err == nil || !strings.Contains(err.Error(), "create response") || calls != 1 {
		t.Fatalf("requests = %d, err = %v", calls, err)
	}
}

func TestReactObserveFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		writeObservationResponse(w, []any{observationMessage("invalid JSON")})
	}))
	defer server.Close()
	r := &ReactProcess[ReactResult]{BaseReactProcess: BaseReactProcess[ReactResult]{ctx: ctx}}
	agentConfig := &config.AgentConfig{LLMConfig: &config.LLMConfig{
		BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema,
	}}
	rc := fileContext.NewReactContext(&fileContext.DurableState{RunId: "react-task"})
	r.updateContext(nil, &mcpSdk.CallToolParams{Name: "apply_patch", Meta: mcpSdk.Meta{toolCallIDMetaKey: "call_patch"}}, rc, errors.New("tool failed"), agentConfig)
	waitForReactObservation(t, ctx, rc, "observation failed")
	formatted := rc.Format()
	for _, want := range []string{"call_patch", "parse response output"} {
		if !strings.Contains(formatted, want) {
			t.Errorf("context missing %q: %s", want, formatted)
		}
	}
	_, results := rc.FormatForModel()
	if len(results) != 1 || !strings.Contains(results[0].Error, "tool failed") {
		t.Fatal("tool error missing from tool messages")
	}
}

func TestObservationSnapshotIndependent(t *testing.T) {
	state := &fileContext.DurableState{Goal: "original goal", CompletionCriteria: []string{"original criterion"}}
	rc := fileContext.NewReactContext(state)
	rc.AppendConfirmedFacts("original fact")
	rc.AppendReportedErrors("original error")
	tr := &fileContext.ToolResult{Result: "original result"}
	snapshot := rc.SnapshotObservationContext(tr)
	state.Goal = "changed goal"
	state.CompletionCriteria[0] = "changed criterion"
	rc.Observation.ConfirmedFacts[0] = "changed fact"
	rc.Observation.ReportedErrors[0] = "changed error"
	tr.Result = "changed result"
	formatted := snapshot.Format()
	for _, want := range []string{"original goal", "original criterion", "original fact", "original error", "original result"} {
		if !strings.Contains(formatted, want) {
			t.Errorf("snapshot missing %q: %s", want, formatted)
		}
	}
}

func waitForReactObservation(t *testing.T, ctx context.Context, rc *fileContext.ReactContext, want string) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if strings.Contains(rc.Format(), want) {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("observation missing %q: %s", want, rc.Format())
		}
	}
}

func TestUpdateContext(t *testing.T) {
	r := &ReactProcess[ReactResult]{}
	rc := fileContext.NewReactContext(nil)
	retained := &fileContext.ToolResult{ToolName: "read_file", Result: "original"}
	rc.AppendToolResult(retained)
	r.updateContext(&mcpSdk.CallToolResult{
		Content:           []mcpSdk.Content{&mcpSdk.TextContent{Text: "permission denied"}},
		StructuredContent: map[string]any{"path": "a.go"},
		IsError:           true,
	}, &mcpSdk.CallToolParams{Meta: mcpSdk.Meta{toolCallIDMetaKey: "call_123"}, Name: "grep", Arguments: map[string]any{"pattern": "test"}}, rc, errors.New("transport error"), nil)
	if len(rc.ToolResults) != 2 || rc.ToolResults[0] != retained {
		t.Fatalf("unexpected history: %+v", rc.ToolResults)
	}
	got := rc.ToolResults[1]
	if got.Status != filemcp.ToolCallFailedStatus {
		t.Fatalf("unexpected status: %q", got.Status)
	}
	if got.ToolCallId != "call_123" || got.ToolName != "grep" || got.Arguments != `{"pattern":"test"}` {
		t.Fatalf("unexpected tool parameters: %+v", got)
	}
	if !strings.Contains(got.Result, "permission denied") || strings.Contains(got.Result, "a.go") || strings.Contains(got.Result, "structuredContent") || !strings.Contains(got.Error, `tool "grep" (call_id="call_123"): MCP call failed: transport error`) || !strings.Contains(got.Error, "tool execution failed (MCP isError=true): permission denied") || !strings.Contains(got.Error, `Tool arguments: {"pattern":"test"}`) {
		t.Fatalf("unexpected result: %+v", got)
	}
	if formatted := rc.Format(); strings.Contains(formatted, "transport error") || strings.Contains(formatted, "permission denied") {
		t.Fatal("raw tool error duplicated in ReAct context")
	}
	r.updateContext(nil, &mcpSdk.CallToolParams{Name: "read_file"}, rc, errors.New("read failed"), nil)
	if len(rc.ToolResults) != 3 || !strings.Contains(rc.ToolResults[2].Error, "MCP call failed: read failed") {
		t.Fatal("nil result lost the tool error")
	}
}

func TestUpdateContextUsesOnlyContent(t *testing.T) {
	r := &ReactProcess[ReactResult]{}
	rc := fileContext.NewReactContext(nil)
	response := &mcpSdk.CallToolResult{
		Content: []mcpSdk.Content{&mcpSdk.TextContent{Text: "file contents"}},
		// An unusable structured payload must not affect content recording.
		StructuredContent: make(chan int),
	}
	r.updateContext(response, &mcpSdk.CallToolParams{Name: "read_file"}, rc, nil, nil)
	got := rc.ToolResults[0]
	if got.Result != `[{"type":"text","text":"file contents"}]` || got.Status != filemcp.ToolCallSuceessStatus || got.Error != "" {
		t.Fatalf("unexpected content result: %+v", got)
	}
	if response.StructuredContent == nil {
		t.Fatal("original MCP response was mutated")
	}
}

func TestUpdateContextConcurrent(t *testing.T) {
	r := &ReactProcess[ReactResult]{}
	rc := fileContext.NewReactContext(nil)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Go(func() {
			r.updateContext(nil, &mcpSdk.CallToolParams{Name: "read_file"}, rc, nil, nil)
			rc.Format()
		})
	}
	wg.Wait()
	if len(rc.ToolResults) != 32 {
		t.Fatalf("concurrent updates lost results: got %d", len(rc.ToolResults))
	}
}

func TestUpdateContextErrorDetails(t *testing.T) {
	tests := []struct {
		name      string
		arguments any
		result    *mcpSdk.CallToolResult
		err       error
		want      string
	}{
		{name: "arguments encoding", arguments: make(chan int), want: "failed to encode arguments as JSON (type chan int)"},
		{name: "result encoding", result: &mcpSdk.CallToolResult{Content: []mcpSdk.Content{&mcpSdk.TextContent{Meta: mcpSdk.Meta{"invalid": make(chan int)}}}}, want: "failed to encode MCP result as JSON; result cannot be added to context"},
		{name: "empty tool error", result: &mcpSdk.CallToolResult{IsError: true}, want: "MCP returned isError=true without a non-empty text error message"},
		{name: "transport error", err: errors.New("connection closed"), want: "MCP call failed: connection closed"},
		{name: "tool execution error", result: &mcpSdk.CallToolResult{IsError: true, Content: []mcpSdk.Content{&mcpSdk.TextContent{Text: "file does not exist"}}}, want: "tool execution failed (MCP isError=true): file does not exist"},
		{name: "arguments validation", err: errors.New("invalid tool arguments: path is required"), want: "invalid tool arguments: path is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			writer := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(writer)
			rc := fileContext.NewReactContext(nil)
			r := &ReactProcess[ReactResult]{}
			r.updateContext(tt.result, &mcpSdk.CallToolParams{Meta: mcpSdk.Meta{toolCallIDMetaKey: "call_failure"}, Name: "read_file", Arguments: tt.arguments}, rc, tt.err, &config.AgentConfig{LogLevel: config.ErrorLogLevel})
			got := rc.ToolResults[0]
			if got.Status != filemcp.ToolCallFailedStatus {
				t.Fatalf("unexpected status: %q", got.Status)
			}
			if got.ToolCallId != "call_failure" || !strings.Contains(got.Error, `tool "read_file" (call_id="call_failure")`) || !strings.Contains(got.Error, tt.want) {
				t.Fatalf("missing error details: %+v", got)
			}
			for _, want := range []string{"[react] tool calling error:", `tool "read_file"`, `call_id="call_failure"`, tt.want} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("error log missing %q: %s", want, logs.String())
				}
			}
			if strings.Contains(logs.String(), "tool calling result:") {
				t.Errorf("error level printed result details: %s", logs.String())
			}
		})
	}
}

// Each handler waits until both calls have started, so sequential execution
// cannot pass. The batch must also finish updating context before returning.
func TestBatchToolCallings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := mcpSdk.NewServer(&mcpSdk.Implementation{Name: "batch-test"}, nil)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	mcpSdk.AddTool(server, &mcpSdk.Tool{Name: "read_file"}, func(ctx context.Context, req *mcpSdk.CallToolRequest, args struct{}) (*mcpSdk.CallToolResult, any, error) {
		started <- struct{}{}
		select {
		case <-release:
			return &mcpSdk.CallToolResult{Content: []mcpSdk.Content{&mcpSdk.TextContent{Text: "done"}}}, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	})
	ct, st := mcpSdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcpSdk.NewClient(&mcpSdk.Implementation{Name: "batch-client"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	r := &ReactProcess[ReactResult]{BaseReactProcess: BaseReactProcess[ReactResult]{ctx: ctx, mcpClientSess: cs}}
	rc := fileContext.NewReactContext(nil)
	done := make(chan struct{})
	go func() {
		r.batchToolCallings([]*mcpSdk.CallToolParams{
			{Name: "read_file", Meta: mcpSdk.Meta{toolCallIDMetaKey: "first"}, Arguments: map[string]any{}},
			{Name: "read_file", Meta: mcpSdk.Meta{toolCallIDMetaKey: "second"}, Arguments: map[string]any{}},
		}, rc, nil)
		close(done)
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("tool calls did not start concurrently")
		}
	}
	select {
	case <-done:
		t.Fatal("batch returned before tools completed")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("batch did not complete")
	}
	if len(rc.ToolResults) != 2 {
		t.Fatalf("got %d results, want 2", len(rc.ToolResults))
	}
	ids := map[string]bool{}
	for _, result := range rc.ToolResults {
		ids[result.ToolCallId] = true
		if result.Status != filemcp.ToolCallSuceessStatus || result.Error != "" || !strings.Contains(result.Result, "done") {
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if !ids["first"] || !ids["second"] {
		t.Fatalf("missing call IDs: %v", ids)
	}
}

func TestReactRetriesInvalidToolCalls(t *testing.T) {
	for _, recover := range []bool{true, false} {
		t.Run(map[bool]string{true: "recover", false: "limit"}[recover], func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				var request struct {
					Input string `json:"input"`
				}
				if err := decodeCorrectionRequest(t, req.Body, &request, calls-1, "LLM tool call error"); err != nil {
					t.Error(err)
				}
				if calls > 1 {
					for _, want := range []string{"tool BEGIN RESULT is not allowed", "Retry", "final text"} {
						if !strings.Contains(request.Input, want) {
							t.Errorf("retry missing %q", want)
						}
					}
				}
				if recover && calls == 2 {
					writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
					return
				}
				writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "BEGIN RESULT", "arguments": `{"status":"completed","answer":"done"}`}})
			}))
			defer server.Close()
			process := &ReactProcess[ReactResult]{BaseReactProcess: BaseReactProcess[ReactResult]{ctx: t.Context(), maxTurn: 1}}
			cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.LineProtocolLLMOutputSchema}}
			got, _, err := process.Run(cfg, &fileContext.DurableState{CompletionCriteria: []string{"original task verified"}, RunId: "retry-tool-task", Goal: "answer"})
			if recover {
				if err != nil || got.Answer != "done" || calls != 2 {
					t.Fatalf("result=%+v calls=%d err=%v", got, calls, err)
				}
			} else if calls != 4 || err == nil || !strings.Contains(err.Error(), "tool call errors exceeded limit (3)") || !strings.Contains(err.Error(), "tool BEGIN RESULT is not allowed") {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
