package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/provider"
	"github.com/collie-agent/codex-go/internal/testutil"
)

func TestReactPublishesToolOutcomesToRedis(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			redisClient, addr := testutil.StartRedis(t)
			workspace := t.TempDir()
			if !failed {
				if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			turns := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				turns++
				w.Header().Set("Content-Type", "application/json")
				if turns == 1 {
					fmt.Fprint(w, `{"status":"completed","output":[{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"a.txt\",\"offset\":1,\"limit\":200}"}]}`)
				} else {
					fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\",\"Observation\":[],\"CompletionEvidence\":[]}"}]}]}`)
				}
			}))
			defer server.Close()
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, Global: &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}, AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, Model: "test", ApiKey: "test", OutputSchema: config.JsonLLMOutputSchema}}
			result, _, err := process.Run(cfg, &fileContext.DurableState{RunId: "tool-task", Goal: "inspect", CompletionCriteria: []string{"inspect file"}})
			if err != nil || result.Answer != "done" || turns != 2 {
				t.Fatalf("result=%+v turns=%d err=%v", result, turns, err)
			}
			entries, err := redisClient.XRange(t.Context(), "codex-go-resp-stream-tool-task", "-", "+").Result()
			if err != nil {
				t.Fatal(err)
			}
			wantKind := "tool_result"
			if failed {
				wantKind = "tool_error"
			}
			var callSeen, outcomeSeen bool
			for _, entry := range entries {
				if entry.Values["type"] == "tool_call" {
					callSeen = true
				}
				if entry.Values["type"] != wantKind {
					continue
				}
				if !callSeen || outcomeSeen {
					t.Fatalf("tool event order/duplication: %v", entries)
				}
				outcomeSeen = true
				var data provider.ToolResultOutput
				if err := json.Unmarshal([]byte(entry.Values["delta"].(string)), &data); err != nil {
					t.Fatal(err)
				}
				if data.Name != "read_file" || data.CallID != "call-read" || (failed && data.Error == "") || (!failed && !strings.Contains(string(data.Result), "evidence")) {
					t.Fatalf("tool outcome=%+v", data)
				}
			}
			if !outcomeSeen {
				t.Fatalf("missing %s: %v", wantKind, entries)
			}
		})
	}
}

func TestPlannerPublishesRedisOutput(t *testing.T) {
	redisClient, addr := testutil.StartRedis(t)
	server := plannerTestServer(t, `{"StepExecutions":[{"StepId":"step-1","Intent":"inspect","Revision":1,"Status":"pending","CompletionCriteria":"verified","Observation":[],"CompletionEvidence":[]}]}`, false)
	cfg := plannerTestConfig(server.URL, config.JsonLLMOutputSchema)
	cfg.LogLevel = config.ErrorLogLevel
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	if _, _, err := NewPlannerProcess(t.Context()).Run(cfg, &fileContext.DurableState{RunId: "plan-task", Goal: "inspect"}); err != nil {
		t.Fatal(err)
	}
	entries, err := redisClient.XRange(t.Context(), "codex-go-resp-stream-plan-task", "-", "+").Result()
	if err != nil || len(entries) != 2 || entries[0].Values["type"] != "usage" || entries[1].Values["type"] != "text" {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestObserverPrintsWithoutPublishingRedisOutput(t *testing.T) {
	redisClient, addr := testutil.StartRedis(t)
	server := plannerTestServer(t, `{"ConfirmedFacts":["found evidence"],"ReportedErrors":[]}`, false)
	cfg := plannerTestConfig(server.URL, config.JsonLLMOutputSchema)
	cfg.LogLevel = config.ErrorLogLevel
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	durable := &fileContext.DurableState{RunId: "observe-task", Goal: "inspect"}
	observation := fileContext.NewObservationContext(durable, &fileContext.Observation{}, &fileContext.ToolResult{ToolName: "read_file", ToolCallId: "read", Result: "hello"})
	output, err := os.CreateTemp(t.TempDir(), "observation-log")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	stderr := os.Stderr
	os.Stderr = output
	defer func() { os.Stderr = stderr }()
	if _, _, err := NewObserveProcess[ObserveResult](t.Context(), observation).Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
	logs, err := os.ReadFile(output.Name())
	if err != nil || !strings.Contains(string(logs), `found evidence`) {
		t.Fatalf("observation missing from stderr: %s, err=%v", logs, err)
	}
	entries, err := redisClient.XRange(t.Context(), "codex-go-resp-stream-observe-task", "-", "+").Result()
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}

func TestWorkerPublishesRejectedToolBatchErrors(t *testing.T) {
	redisClient, addr := testutil.StartRedis(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeObservationResponse(w, []any{
			map[string]any{"type": "function_call", "call_id": "good", "name": "read_file", "arguments": `{"path":"a"}`},
			map[string]any{"type": "function_call", "call_id": "bad", "name": "read_file", "arguments": `null`},
		})
	}))
	defer server.Close()
	cfg := plannerTestConfig(server.URL, config.JsonLLMOutputSchema)
	cfg.LogLevel = config.ErrorLogLevel
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	cfg.AllowTools = map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}}
	worker := NewWorker[ReactResult](t.Context(), cfg, nil)
	_, _, calls, err := worker.Run(workerTestContext{request: fileContext.ModelRequest{RunID: "rejected", AllowTools: true}})
	if err == nil || len(calls) != 0 {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
	entries, readErr := redisClient.XRange(t.Context(), "codex-go-resp-stream-rejected", "-", "+").Result()
	if readErr != nil {
		t.Fatal(readErr)
	}
	var errors []provider.ToolResultOutput
	for _, entry := range entries {
		if entry.Values["type"] != "tool_error" {
			continue
		}
		var data provider.ToolResultOutput
		if err := json.Unmarshal([]byte(entry.Values["delta"].(string)), &data); err != nil {
			t.Fatal(err)
		}
		errors = append(errors, data)
	}
	if len(errors) != 2 || errors[0].CallID != "good" || errors[1].CallID != "bad" || !strings.Contains(errors[0].Error, "not executed") || !strings.Contains(errors[1].Error, "null") {
		t.Fatalf("errors=%+v entries=%v", errors, entries)
	}
}

func TestReactPublishesControlToolResult(t *testing.T) {
	redisClient, addr := testutil.StartRedis(t)
	turns := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turns++
		if turns == 1 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "call_id": "criteria", "name": "update_completion_criteria", "arguments": `{"completion_criteria":["verified"]}`}})
		} else {
			writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"done","Observation":[],"CompletionEvidence":[]}`)})
		}
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := plannerTestConfig(server.URL, config.JsonLLMOutputSchema)
	cfg.LogLevel = config.ErrorLogLevel
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	if _, _, err := process.Run(cfg, &fileContext.DurableState{RunId: "control", Goal: "inspect"}); err != nil {
		t.Fatal(err)
	}
	entries, err := redisClient.XRange(t.Context(), "codex-go-resp-stream-control", "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.Values["type"] == "tool_result" {
			count++
			var data provider.ToolResultOutput
			if err := json.Unmarshal([]byte(entry.Values["delta"].(string)), &data); err != nil {
				t.Fatal(err)
			}
			if data.CallID != "criteria" || data.Name != "update_completion_criteria" || !strings.Contains(string(data.Result), "verified") {
				t.Fatalf("outcome=%+v", data)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected one control result: %v", entries)
	}
}

func TestWorkerRedisFailureDoesNotRetryModel(t *testing.T) {
	redisClient, addr := testutil.StartRedis(t)
	_ = redisClient.Shutdown(t.Context()).Err()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"done","Observation":[],"CompletionEvidence":[]}`)})
	}))
	defer server.Close()
	cfg := plannerTestConfig(server.URL, config.JsonLLMOutputSchema)
	cfg.LogLevel = config.ErrorLogLevel
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	worker := NewWorker[ReactResult](t.Context(), cfg, nil)
	result, _, _, err := worker.Run(workerTestContext{request: fileContext.ModelRequest{RunID: "unavailable"}})
	if err != nil || result.Answer != "done" || requests.Load() != 1 {
		t.Fatalf("result=%+v requests=%d err=%v", result, requests.Load(), err)
	}
}

func TestWorkerStreamsRedisAtErrorLogLevel(t *testing.T) {
	redisClient, addr := testutil.StartRedis(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":12,\"output_tokens\":4},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"{\\\"status\\\":\\\"completed\\\",\\\"answer\\\":\\\"done\\\",\\\"Observation\\\":[],\\\"CompletionEvidence\\\":[]}\"}]}]}}\n\n")
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	yaml := fmt.Sprintf("name: react\nlog_level: error\nredis:\n  addr: %q\nbase:\n  llm:\n    model: test\n    api_key: test\n    base_url: %q\n    stream: true\n", addr, server.URL)
	if err := os.WriteFile(configPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker[ReactResult](t.Context(), cfg.ForAgent("react"), nil)
	result, _, _, err := worker.Run(workerTestContext{request: fileContext.ModelRequest{RunID: "stream-task", LogLabel: "react"}})
	if err != nil || result.Answer != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	entries, err := redisClient.XRange(t.Context(), "codex-go-resp-stream-stream-task", "-", "+").Result()
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	for i, kind := range []string{"text_delta", "usage"} {
		if entries[i].Values["type"] != kind {
			t.Fatalf("entry %d=%+v", i, entries[i])
		}
		if !json.Valid([]byte(entries[i].Values["delta"].(string))) {
			t.Fatalf("invalid event data: %+v", entries[i])
		}
	}
	if entries[0].Values["delta"] != `{"text":"partial"}` {
		t.Fatalf("delta=%+v", entries[0])
	}
}
