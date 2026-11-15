package agent_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactRepeatedVerificationEntersBoundedRecovery(t *testing.T) {
	turns, recoveries, summaries := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		if strings.Contains(string(body), "Execution budget closing:") {
			summaries++
			criteriaResponse(w, `{"status":"failed","answer":"verification repeated without changes"}`)
			return
		}
		turns++
		if strings.Contains(string(body), "investigation recovery") {
			recoveries++
		}
		if turns > 8 {
			criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
			return
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": "exec", "arguments": `{"command":"python3","args":["-c","print('verified')"]}`})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2})
	durable := &state.DurableState{RunId: "repeat-command", Goal: "verify", CompletionCriteria: []string{"finish verification"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err == nil || result.Status != agent.FailedReactStatus || turns != 5 || recoveries != 2 || summaries != 1 {
		t.Fatalf("result=%+v err=%v turns=%d recoveries=%d summaries=%d", result, err, turns, recoveries, summaries)
	}
}

func TestReactSessionTimeoutEntersBoundedRecovery(t *testing.T) {
	for _, output := range []bool{false, true} {
		t.Run(fmt.Sprint(output), func(t *testing.T) {
			turns, summaries, recoveries := 0, 0, 0
			sessionID := ""
			cursor := float64(0)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(string(body), "Execution budget closing:") {
					summaries++
					criteriaResponse(w, `{"status":"failed","answer":"session exceeded deadline"}`)
					return
				}
				turns++
				if strings.Contains(string(body), "investigation recovery") {
					recoveries++
				}
				if turns > 18 {
					criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
					return
				}
				name, args := "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", "import time; time.sleep(60)"}}
				if output {
					args["args"] = []string{"-u", "-c", "import time\nwhile True: print('tick'); time.sleep(.03)"}
				}
				if turns > 1 {
					var req struct {
						Input []struct {
							Type, Output string
							CallID       string `json:"call_id"`
						}
					}
					_ = json.Unmarshal(body, &req)
					for _, item := range req.Input {
						if item.Type == "function_call_output" && item.CallID == fmt.Sprintf("call_%d", turns-1) {
							var blocks []struct{ Text string }
							_ = json.Unmarshal([]byte(item.Output), &blocks)
							if len(blocks) > 0 {
								var out map[string]any
								_ = json.Unmarshal([]byte(blocks[0].Text), &out)
								if id, ok := out["session_id"].(string); ok {
									sessionID = id
								}
								if c, ok := out["next_cursor"].(float64); ok {
									cursor = c
								}
							}
						}
					}
					name, args = "exec_poll", map[string]any{"session_id": sessionID, "cursor": cursor, "wait_ms": 100}
				}
				// Simulate a real model's decision latency even for a continuously writing process.
				time.Sleep(40 * time.Millisecond)
				encoded, _ := json.Marshal(args)
				criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": name, "arguments": string(encoded)})
			}))
			defer server.Close()
			process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir(), Timeout: 250 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2})
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecStartMCPToolName: {}, config.ExecPollMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
			result, _, err := process.Run(cfg, &state.DurableState{RunId: "wait", Goal: "wait for service", CompletionCriteria: []string{"service ready"}})
			if err == nil || result.Status != agent.FailedReactStatus || summaries != 1 || recoveries == 0 || turns > 15 {
				t.Fatalf("result=%+v err=%v turns=%d recovery=%d summaries=%d", result, err, turns, recoveries, summaries)
			}
		})
	}
}

func TestReactVerificationRecoveryCanRepairAndRetest(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	turns, stage := 0, 0
	recovered := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		turns++
		name, args := "exec", `{"command":"python3","args":["-c","from pathlib import Path; print(Path('value.txt').read_text())"]}`
		if stage == 0 && strings.Contains(string(body), "investigation recovery") {
			recovered = true
			stage = 1
			name, args = "apply_patch", `{"patch":"--- value.txt\n+++ value.txt\n@@ -1 +1 @@\n-broken\n+fixed\n"}`
		} else if stage == 1 {
			stage = 2
		} else if stage == 2 {
			var req struct {
				Input []struct {
					Type, Output string
					CallID       string `json:"call_id"`
				}
			}
			_ = json.Unmarshal(body, &req)
			verified := false
			for _, item := range req.Input {
				if item.Type == "function_call_output" && item.CallID == fmt.Sprintf("call_%d", turns-1) && strings.Contains(item.Output, "fixed") {
					verified = true
				}
			}
			if !verified {
				t.Error("retest did not expose repaired value through native tool response")
			}

			if strings.Contains(string(body), "Execution budget closing:") {
				t.Error("repair and retest incorrectly exhausted recovery")
			}
			criteriaResponse(w, `{"status":"completed","answer":"repaired and verified"}`)
			return
		}
		if turns > 8 {
			criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
			return
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": name, "arguments": args})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 1})
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, &state.DurableState{RunId: "repair-retest", Goal: "repair and verify", CompletionCriteria: []string{"repair verified"}})
	if err != nil || result.Status != agent.CompletedReactStatus || !recovered || turns != 5 {
		t.Fatalf("result=%+v err=%v recovered=%v turns=%d", result, err, recovered, turns)
	}
}

func TestReactRepeatedFailedVerificationEntersBoundedRecovery(t *testing.T) {
	turns, recoveries, summaries := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		if strings.Contains(string(body), "Execution budget closing:") {
			summaries++
			criteriaResponse(w, `{"status":"failed","answer":"verification repeated without changes"}`)
			return
		}
		turns++
		if strings.Contains(string(body), "investigation recovery") {
			recoveries++
		}
		if turns > 8 {
			criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
			return
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": "exec", "arguments": `{"command":"python3","args":["-c","import sys; sys.exit(1)"]}`})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2})
	durable := &state.DurableState{RunId: "repeat-command", Goal: "verify", CompletionCriteria: []string{"finish verification"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err == nil || result.Status != agent.FailedReactStatus || turns != 5 || recoveries != 2 || summaries != 1 {
		t.Fatalf("result=%+v err=%v turns=%d recoveries=%d summaries=%d", result, err, turns, recoveries, summaries)
	}
}

func TestReactSessionAllowsEmptyWaitsAndCursorProgress(t *testing.T) {
	turns := 0
	sessionID := ""
	cursor := float64(0)
	runningEmpty, advanced := 0, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "investigation recovery") || strings.Contains(string(body), "Execution budget closing:") {
			t.Error("reasonable session waiting triggered recovery")
		}
		turns++
		name, args := "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", "import time; time.sleep(.5); print('one'); time.sleep(.15); print('two')"}}
		if turns > 1 {
			var req struct {
				Input []struct {
					Type, Output string
					CallID       string `json:"call_id"`
				}
			}
			_ = json.Unmarshal(body, &req)
			for _, item := range req.Input {
				if item.Type == "function_call_output" && item.CallID == fmt.Sprintf("call_%d", turns-1) {
					var blocks []struct{ Text string }
					_ = json.Unmarshal([]byte(item.Output), &blocks)
					if len(blocks) > 0 {
						var out map[string]any
						_ = json.Unmarshal([]byte(blocks[0].Text), &out)
						if id, ok := out["session_id"].(string); ok {
							sessionID = id
						}
						if c, ok := out["next_cursor"].(float64); ok {
							if c > cursor {
								advanced = true
							}
							cursor = c
						}
						if out["state"] == "running" && out["stdout"] == "" {
							runningEmpty++
						}
						if out["state"] == "exited" {
							if out["exit_code"] != float64(0) {
								t.Errorf("unexpected exit: %v", out)
							}
							criteriaResponse(w, `{"status":"completed","answer":"both output chunks observed"}`)
							return
						}
					}
				}
			}
			name, args = "exec_poll", map[string]any{"session_id": sessionID, "cursor": cursor, "wait_ms": 100}
		}
		if turns > 20 {
			criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
			return
		}
		encoded, _ := json.Marshal(args)
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": name, "arguments": string(encoded)})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir(), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 1})
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecStartMCPToolName: {}, config.ExecPollMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, &state.DurableState{RunId: "reasonable-wait", Goal: "await output", CompletionCriteria: []string{"observe output"}})
	if err != nil || result.Status != agent.CompletedReactStatus || runningEmpty < 3 || !advanced {
		t.Fatalf("result=%+v err=%v empty=%d advanced=%v", result, err, runningEmpty, advanced)
	}
}

func TestReactResumeRetainsCommandEvidence(t *testing.T) {
	workspace := t.TempDir()
	durable := &state.DurableState{RunId: "resume-command", ExecutionID: "same-instance", Goal: "verify", CompletionCriteria: []string{"verification finished"}}
	turns := 0
	recovered := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		turns++
		if strings.Contains(string(body), "investigation recovery") {
			recovered = true
			criteriaResponse(w, `{"status":"completed","answer":"prior verification is sufficient"}`)
			return
		}
		if turns == 3 {
			criteriaResponse(w, `{"status":"pending","answer":"interrupted decision"}`)
			return
		}
		if turns > 5 {
			criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
			return
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": "exec", "arguments": `{"command":"python3","args":["-c","print('verified')"]}`})
	}))
	defer server.Close()
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	for attempt := 0; attempt < 2; attempt++ {
		process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		process.SetCheckpoint(func() error { return nil })
		process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2})
		_, _, err = process.Run(cfg, durable)
		if (attempt == 0 && err == nil) || (attempt == 1 && err != nil) {
			t.Fatalf("attempt=%d error=%v", attempt, err)
		}
		encoded, _ := json.Marshal(durable)
		durable = &state.DurableState{}
		if err := json.Unmarshal(encoded, durable); err != nil {
			t.Fatal(err)
		}
	}
	if !recovered || turns != 5 {
		t.Fatalf("resume lost command evidence: recovery=%v turns=%d", recovered, turns)
	}
}

func TestReactExpiredSessionEntersBoundedRecovery(t *testing.T) {
	turns, recoveries, summaries := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "工具结果观察器") {
			criteriaResponse(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		if strings.Contains(string(body), "Execution budget closing:") {
			summaries++
			criteriaResponse(w, `{"status":"failed","answer":"verification repeated without changes"}`)
			return
		}
		turns++
		if strings.Contains(string(body), "investigation recovery") {
			recoveries++
		}
		if turns > 8 {
			criteriaResponse(w, `{"status":"failed","answer":"test deadline"}`)
			return
		}
		criteriaResponse(w, "", map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turns), "call_id": fmt.Sprintf("call_%d", turns), "name": "exec_poll", "arguments": `{"session_id":"expired-before-resume","wait_ms":100}`})
	}))
	defer server.Close()
	process, err := agent.NewReactProcess[agent.ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	process.SetFileProgressPolicy(agent.FileProgressPolicy{StalledBatches: 2, RecoveryWindow: 2})
	durable := &state.DurableState{RunId: "repeat-command", Goal: "verify", CompletionCriteria: []string{"finish verification"}}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecPollMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	result, _, err := process.Run(cfg, durable)
	if err == nil || result.Status != agent.FailedReactStatus || turns != 5 || recoveries != 2 || summaries != 1 {
		t.Fatalf("result=%+v err=%v turns=%d recoveries=%d summaries=%d", result, err, turns, recoveries, summaries)
	}
}
