package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactCheckpointsDecisionBudgetBeforeModelRequest(t *testing.T) {
	durable := &state.DurableState{RunId: "budget", ExecutionID: "budget", Goal: "finish"}
	var checkpoint map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if len(checkpoint["ExecutionProgress"]) == 0 || string(checkpoint["ExecutionProgress"]) == "null" {
			t.Error("model decision started without durable budget reservation")
		}
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"done"}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetCheckpoint(func() error {
		data, err := json.Marshal(durable)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, &checkpoint)
	})
	_, _, err = process.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReactResumeCannotRefreshDecisionBudget(t *testing.T) {
	durable := &state.DurableState{RunId: "resume-budget", ExecutionID: "instance", Goal: "finish", CompletionCriteria: []string{"verified"}}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests < 4 {
			writeObservationResponse(w, []any{observationMessage(`{"status":"pending","answer":"not finished"}`)})
			return
		}
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"unsupported success"}`)})
	}))
	defer server.Close()
	var last ReactResult
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		process.maxTurn = 4
		process.SetCheckpoint(func() error { return nil })
		last, _, lastErr = process.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
		data, _ := json.Marshal(durable)
		durable = &state.DurableState{}
		if err := json.Unmarshal(data, durable); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 4 || lastErr == nil || last.Status != FailedReactStatus || len(durable.ExecutionResult) == 0 {
		t.Fatalf("requests=%d result=%+v err=%v saved=%s", requests, last, lastErr, durable.ExecutionResult)
	}
}

func TestReactResumeRetainsStallRecoveryAndNewExecutionIsIndependent(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "resume-stall", ExecutionID: "first", Goal: "inspect", CompletionCriteria: []string{"inspect value"}}
	requests, recovered := 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		requests++
		if strings.Contains(string(body), "File investigation recovery") {
			recovered = true
			writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"inspected"}`)})
			return
		}
		if requests == 3 {
			writeObservationResponse(w, []any{observationMessage(`{"status":"pending","answer":"checkpoint interruption"}`)})
			return
		}
		if requests > 5 {
			t.Error("resume discarded prior stagnation")
			writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"deadline"}`)})
			return
		}
		writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", requests), "call_id": fmt.Sprintf("call_%d", requests), "name": "read_file", "arguments": fmt.Sprintf(`{"path":"value.txt","limit":%d}`, 20+requests)}})
	}))
	defer server.Close()
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	for attempt := 0; attempt < 2; attempt++ {
		process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		process.SetFileProgressPolicy(FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2})
		process.SetCheckpoint(func() error { return nil })
		_, _, err = process.Run(cfg, durable)
		if attempt == 0 && err == nil {
			t.Fatal("expected interrupted first run")
		}
		if attempt == 1 && err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(durable)
		durable = &state.DurableState{}
		if err := json.Unmarshal(data, durable); err != nil {
			t.Fatal(err)
		}
	}
	if !recovered || requests != 5 {
		t.Fatalf("recovered=%v requests=%d", recovered, requests)
	}
	// A new instance must not inherit a finalized/exhausted predecessor's budget.
	durable.ExecutionID = "second"
	durable.ExecutionResult = nil
	durable.ToolExecutions = nil
	finalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"second instance"}`)})
	}))
	defer finalServer.Close()
	cfg.LLMConfig.BaseUrl = finalServer.URL
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.maxTurn = 2
	result, _, err := process.Run(cfg, durable)
	if err != nil || result.Answer != "second instance" {
		t.Fatalf("new instance result=%+v err=%v", result, err)
	}
}

func TestReactProgressCheckpointFailureStopsBeforeAnotherDecision(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_read", "call_id": "read", "name": "read_file", "arguments": `{"path":"value.txt"}`}})
	}))
	defer server.Close()
	durable := &state.DurableState{RunId: "save-failure", ExecutionID: "save-failure", CompletionCriteria: []string{"inspect value"}}
	failure := errors.New("progress storage unavailable")
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetCheckpoint(func() error {
		var progress struct{ ObservedThroughBatch int }
		if err := json.Unmarshal(durable.ExecutionProgress, &progress); err != nil {
			return err
		}
		if progress.ObservedThroughBatch > 0 {
			return failure
		}
		return nil
	})
	_, _, err = process.Run(&config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if !errors.Is(err, failure) || requests != 1 {
		t.Fatalf("requests=%d error=%v", requests, err)
	}
}

func TestReactResumeReconcilesConfirmedBatchBeforeNextDecision(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "reconcile", ExecutionID: "reconcile", Goal: "inspect", CompletionCriteria: []string{"inspect value"}}
	var saved *state.DurableState
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		body, _ := io.ReadAll(req.Body)
		if requests == 3 {
			if !strings.Contains(string(body), "File investigation recovery") {
				t.Error("confirmed batch was not reconciled before resume decision")
			}
			writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"reconciled"}`)})
			return
		}
		if requests > 3 {
			t.Error("confirmed tool batch replayed")
		}
		writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", requests), "call_id": fmt.Sprintf("call_%d", requests), "name": "read_file", "arguments": fmt.Sprintf(`{"path":"value.txt","limit":%d}`, 20+requests)}})
	}))
	defer server.Close()
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 2})
	failure := errors.New("crashed before progress checkpoint")
	process.SetCheckpoint(func() error {
		var progress struct{ ObservedThroughBatch int }
		if err := json.Unmarshal(durable.ExecutionProgress, &progress); err != nil {
			return err
		}
		if progress.ObservedThroughBatch == 2 {
			return failure
		}
		saved = durable.Clone()
		return nil
	})
	if _, _, err := process.Run(cfg, durable); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if saved == nil || len(saved.ToolExecutions) != 2 || saved.ToolExecutions[1].Status != "success" {
		t.Fatalf("missing confirmed crash-window batch: %+v", saved)
	}
	process, err = NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 2})
	result, _, err := process.Run(cfg, saved)
	if err != nil || result.Answer != "reconciled" || requests != 3 {
		t.Fatalf("requests=%d result=%+v error=%v", requests, result, err)
	}
}

func TestReactClosingFailureKeepsConfirmedEvidence(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if tools := string(body["tools"]); tools != "" && tools != "[]" && tools != "null" {
			t.Errorf("closing request exposed tools: %s", tools)
		}
		// A closing response cannot turn an unfinished execution into success.
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"unsupported success"}`)})
	}))
	defer server.Close()
	durable := &state.DurableState{
		RunId: "closing", ExecutionID: "closing", Goal: "repair parser",
		CompletionCriteria: []string{"parser accepts valid input"},
		ExecutionProgress:  []byte(`{"ExecutionID":"closing","UsedTurns":1,"ObservedThroughBatch":2}`),
		ToolExecutions: []*state.ToolResult{
			{Batch: 1, ToolCallId: "patch", ToolName: "apply_patch", Status: "success", Result: "parser.go modified"},
			{Batch: 2, ToolCallId: "check", ToolName: "exec", Status: "success", Result: `{"stdout":"parser test failed","exit_code":1}`},
		},
	}
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.maxTurn = 2
	result, _, err := process.Run(&config.AgentConfig{LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	var failure *BusinessFailureError
	if !errors.As(err, &failure) || result.Status != FailedReactStatus || requests != 1 || len(durable.ExecutionResult) == 0 {
		t.Fatalf("requests=%d result=%+v error=%v", requests, result, err)
	}
	observations := strings.Join(result.Observation, "\n")
	for _, want := range []string{"patch", "check", "parser test failed", "parser accepts valid input", "maximum decision budget exhausted"} {
		if !strings.Contains(observations, want) {
			t.Errorf("closing result lost %q: %s", want, observations)
		}
	}
}
