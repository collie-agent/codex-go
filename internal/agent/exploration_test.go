package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactExplorationThresholdBlocksFurtherTools(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 4 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": fmt.Sprint("fc", calls), "call_id": fmt.Sprint("call", calls), "name": "list_dir", "arguments": fmt.Sprintf(`{"path":".","limit":%d}`, calls)}})
			return
		}
		writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"stopped"}`)})
	}))
	defer server.Close()
	durable := &state.DurableState{Goal: "modify source", CompletionCriteria: []string{"change source"}}
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = process.Run(&config.AgentConfig{ExplorationBudget: config.ExplorationBudget{InitialTurns: 1, DecisionRetries: 1}, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil {
		t.Fatal("expected bounded failure")
	}
	if len(durable.ToolExecutions) != 1 {
		t.Fatalf("threshold allowed %d tools", len(durable.ToolExecutions))
	}
}

func TestReactAnalysisClassificationContinuesPastThreshold(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"analysis","basis":"User only requests an explanation"}}`)})
		case 2:
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "list_dir", "arguments": `{"path":"."}`}})
		default:
			writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"explained"}`)})
		}
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{Goal: "explain source", CompletionCriteria: []string{"explanation"}}
	result, _, err := process.Run(&config.AgentConfig{ExplorationBudget: config.ExplorationBudget{InitialTurns: 1, DecisionRetries: 1}, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err != nil || result.Answer != "explained" || len(durable.ToolExecutions) != 1 {
		t.Fatalf("result=%+v err=%v tools=%d", result, err, len(durable.ToolExecutions))
	}
}

func TestReactEarlyHandoffDoesNotSpendExploration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"implement","target":"main.go: fix return value","evidence":"read main.go","next_operation":"apply patch"}}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{Goal: "fix source", CompletionCriteria: []string{"fixed"}}
	_, _, err = process.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil {
		t.Fatal("handoff is not task completion")
	}
	var saved struct {
		Exploration struct {
			Used    int
			Phase   string
			Handoff *state.ExplorationDecision
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.Used != 0 || saved.Exploration.Phase != "implement" || saved.Exploration.Handoff == nil {
		t.Fatalf("lost handoff: %s", durable.ExecutionProgress)
	}
}

func TestReactExplorationCheckpointFailureBlocksTools(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"unexpected"}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{ExecutionID: "instance", Goal: "fix source", CompletionCriteria: []string{"fixed"}}
	process.SetCheckpoint(func() error { return fmt.Errorf("storage unavailable") })
	_, _, err = process.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}, durable)
	if err == nil || calls != 0 || len(durable.ToolExecutions) != 0 {
		t.Fatalf("err=%v requests=%d", err, calls)
	}
}

func TestReactResumeRetainsExplorationPolicy(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "call", "name": "list_dir", "arguments": `{"path":"."}`}})
			return
		}
		if requests == 2 {
			http.Error(w, "interrupted", http.StatusBadRequest)
			return
		}
		writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"unable","reason":"source unavailable","execution_state":"only listed directory"}}`)})
	}))
	defer server.Close()
	durable := &state.DurableState{Goal: "fix source", CompletionCriteria: []string{"fixed"}}
	cfg := &config.AgentConfig{ExplorationBudget: config.ExplorationBudget{InitialTurns: 1, DecisionRetries: 0}, AllowTools: map[config.McpToolName]struct{}{config.ListDirMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	workspace := t.TempDir()
	for attempt := 0; attempt < 2; attempt++ {
		process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = process.Run(cfg, durable)
		if err == nil {
			t.Fatal("expected incomplete execution")
		}
		data, err := json.Marshal(durable)
		if err != nil {
			t.Fatal(err)
		}
		durable = &state.DurableState{}
		if err = json.Unmarshal(data, durable); err != nil {
			t.Fatal(err)
		}
		cfg.ExplorationBudget.InitialTurns = 100
	}
	var saved struct {
		Exploration struct {
			Policy config.ExplorationBudget
			Used   int
			Phase  string
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if requests != 4 || saved.Exploration.Policy.InitialTurns != 1 || saved.Exploration.Used != 1 || saved.Exploration.Phase != "unable" || len(durable.ToolExecutions) != 1 {
		t.Fatalf("requests=%d progress=%s", requests, durable.ExecutionProgress)
	}
}

func TestReactLineProtocolPhaseDecision(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nBEGIN EXPLORATION\nTASK_TYPE=\"modification\"\nBASIS=\"requested fix\"\nCHOICE=\"unable\"\nREASON=\"source missing\"\nEXECUTION_STATE=\"no changes\"\nEND EXPLORATION\nEND RESULT")})
			return
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"failed\"\nANSWER=\"source missing; no changes\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{Goal: "fix source", CompletionCriteria: []string{"fixed"}}
	result, _, err := process.Run(&config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.LineProtocolLLMOutputSchema}}, durable)
	if err == nil || result.Status != FailedReactStatus || requests != 2 || len(durable.ToolExecutions) != 0 {
		t.Fatalf("result=%+v err=%v requests=%d", result, err, requests)
	}
}

func TestReactAnalysisImplementationChargesInvestigation(t *testing.T) {
	workspace := t.TempDir()
	policy := config.DefaultExplorationBudget()
	policy.InitialTurns = 1
	durable := &state.DurableState{ExecutionID: "analysis-implementation", Goal: "create main.go", CompletionCriteria: []string{"changed"}}
	err := runModificationScript(t, workspace, durable, policy, [][]any{
		{observationMessage(`{"exploration":{"task_type":"analysis","basis":"initial analysis","choice":"implement","target_files":["main.go"],"target":"create main.go","evidence":"missing file","next_operation":"apply patch"}}`)},
		modificationTool("edit", "apply_patch", map[string]string{"patch": "--- /dev/null\n+++ main.go\n@@ -0,0 +1 @@\n+package main\n"}),
		modificationTool("read", "read_file", map[string]string{"path": "main.go"}),
		modificationTool("search", "grep", map[string]string{"pattern": "package", "path": "main.go"}),
		{observationMessage(`{"status":"completed","answer":"done"}`)},
	})
	if err == nil {
		t.Fatal("analysis implementation bypassed exploration threshold")
	}
	var saved struct {
		Exploration struct {
			TaskType string
			Used     int
		}
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Exploration.TaskType != "modification" || saved.Exploration.Used != 1 {
		t.Fatalf("progress=%s", durable.ExecutionProgress)
	}
}
