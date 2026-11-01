package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReactResumeStopsAtUnknownToolOutcome(t *testing.T) {
	r, err := NewReactProcess[ReactResult](context.Background(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "original", Goal: "continue", ExecutionID: "instance", ToolExecutions: []*state.ToolResult{{ToolCallId: "call_write", ToolName: "write_file", Arguments: `{"path":"a"}`}}}
	_, _, err = r.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{}}, durable)
	if err == nil {
		t.Fatal("unknown outcome allowed execution")
	}
	for _, want := range []string{"call_write", "write_file", `{"path":"a"}`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestReactResumeReturnsSavedInstanceResult(t *testing.T) {
	r, err := NewReactProcess[ReactResult](context.Background(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "original", Goal: "continue", ExecutionID: "instance", ExecutionResult: []byte(`{"status":"completed","answer":"already done"}`)}
	result, _, err := r.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{}}, durable)
	if err != nil || result.Answer != "already done" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestReactPersistsFinalResultBeforeReturning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"done"}`)})
	}))
	defer server.Close()
	r, err := NewReactProcess[ReactResult](context.Background(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "original", Goal: "finish"}
	saved := false
	r.SetCheckpoint(func() error {
		if len(durable.ExecutionResult) > 0 {
			saved = true
		}
		return nil
	})
	_, _, err = r.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err != nil || !saved || durable.ExecutionID == "" {
		t.Fatalf("saved=%v id=%s error=%v", saved, durable.ExecutionID, err)
	}
}

func TestReactDoesNotExposeUncommittedFinalResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"done"}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "original", Goal: "finish", ExecutionID: "original"}
	process.SetCheckpoint(func() error {
		if len(durable.ExecutionResult) > 0 {
			return errors.New("final checkpoint unavailable")
		}
		return nil
	})
	_, _, err = process.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil || len(durable.ExecutionResult) != 0 {
		t.Fatalf("error=%v uncommitted result=%s", err, durable.ExecutionResult)
	}
}

func TestPlanStepDoesNotReuseStandaloneExecutionResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"step done","Observation":["observed"],"CompletionEvidence":["verified"]}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "original", Goal: "finish", ExecutionID: "standalone", ExecutionResult: []byte(`{"status":"completed","answer":"old answer","Observation":["old"],"CompletionEvidence":["old"]}`), StepExecutions: []state.StepExecution{{StepId: "step", Status: state.RunningExecutionStatus}}}
	result, _, err := process.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err != nil || result.Answer != "step done" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

// Ordered completions exercise the two possible races without relying on timing.
func TestUnknownToolOutcomeDoesNotDiscardConfirmedSiblings(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {1, 0, 2}, {1, 2, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			durable := &state.DurableState{RunId: "run", ExecutionID: "instance"}
			for _, id := range []string{"unknown", "first", "second"} {
				durable.ToolExecutions = append(durable.ToolExecutions, &state.ToolResult{ToolCallId: id, ToolName: "read_file"})
			}
			r := &ReactProcess[ReactResult]{}
			rc := state.NewReactContext(durable)
			var saved *state.DurableState
			r.SetCheckpoint(func() error { saved = durable.Clone(); return nil })
			for _, index := range order {
				id := durable.ToolExecutions[index].ToolCallId
				var result *sdk.CallToolResult
				if index != 0 {
					result = &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: id}}}
				}
				r.updateContext(result, &sdk.CallToolParams{Name: "read_file", Meta: sdk.Meta{toolCallIDMetaKey: id}}, rc, nil, nil)
			}
			if r.executionErr == nil || !strings.Contains(r.executionErr.Error(), "unknown") || durable.ToolExecutions[0].Status != "" {
				t.Fatalf("unknown outcome not retained: error=%v calls=%+v", r.executionErr, durable.ToolExecutions)
			}
			if saved == nil || saved.ToolExecutions[0].Status != "" || saved.ToolExecutions[1].Status != "success" || saved.ToolExecutions[2].Status != "success" {
				t.Fatalf("confirmed siblings not saved: %+v", saved)
			}
		})
	}
}

func TestToolCheckpointFailureStopsFurtherWrites(t *testing.T) {
	durable := &state.DurableState{ExecutionID: "instance", ToolExecutions: []*state.ToolResult{{ToolCallId: "a"}, {ToolCallId: "b"}}}
	r := &ReactProcess[ReactResult]{}
	writes := 0
	failure := errors.New("checkpoint lost")
	r.SetCheckpoint(func() error { writes++; return failure })
	for _, id := range []string{"a", "b"} {
		r.updateContext(&sdk.CallToolResult{}, &sdk.CallToolParams{Name: "read_file", Meta: sdk.Meta{toolCallIDMetaKey: id}}, state.NewReactContext(durable), nil, nil)
	}
	if writes != 1 || !errors.Is(r.executionErr, failure) {
		t.Fatalf("writes=%d error=%v", writes, r.executionErr)
	}
}
