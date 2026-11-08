package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactStoresExecSummaryInsteadOfRawOutput(t *testing.T) {
	durable := &state.DurableState{RunId: "summary", Goal: "run the check", CompletionCriteria: []string{"checked"}}
	decisions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		inputItems, _ := body["input"].([]any)
		system, _ := inputItems[0].(map[string]any)
		instructions, _ := system["content"].(string)
		if strings.Contains(instructions, "# Exec output summary") {
			writeObservationResponse(w, []any{observationMessage(`{"Summary":"Check completed; diagnostic retained."}`)})
			return
		}
		// The existing observer is independent of the decision loop.
		if strings.Contains(instructions, "工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
			return
		}
		decisions++
		if decisions == 1 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_exec", "call_id": "exec_check", "name": "exec", "arguments": `{"command":"python3","args":["-c","print('RAW_SENTINEL'*4000)"]}`}})
			return
		}
		input, _ := json.Marshal(body["input"])
		if !strings.Contains(string(input), "Check completed; diagnostic retained.") || strings.Contains(string(input), strings.Repeat("RAW_SENTINEL", 10)) {
			t.Error("decision did not receive only the exec summary")
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	cfg := execSummaryConfig(t, server.URL)
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	saved := ""
	process.SetCheckpoint(func() error {
		for _, call := range durable.ToolExecutions {
			if call.Status != "" {
				saved = call.Result
			}
		}
		return nil
	})
	if _, _, err := process.Run(cfg, durable); err != nil {
		t.Fatal(err)
	}
	if len(durable.ToolExecutions) != 1 || !strings.Contains(saved, "Check completed; diagnostic retained.") || strings.Contains(saved, "RAW_SENTINEL") {
		t.Fatal("checkpoint did not retain only the summarized exec result")
	}
}

func execSummaryConfig(t *testing.T, url string) *config.AgentConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	data := "name: react\nlog_level: error\nbase:\n  allow_tools: [exec]\n  llm:\n    model: test\n    api_key: test\n    base_url: " + url + "\n    output_schema: line_protocol_llm_output\nagents:\n  compact:\n    llm:\n      model: test\n      api_key: test\n      base_url: " + url + "\n      output_schema: json_llm_output\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestReactSummarizesExecOutputInBoundedChunksAndPreservesExitCode(t *testing.T) {
	durable := &state.DurableState{RunId: "chunks", Goal: "check", CompletionCriteria: []string{"checked"}}
	var supplied strings.Builder
	summaries, decisions := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input []struct{ Role, Content string }
			Tools []any
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		system := body.Input[0].Content
		if strings.Contains(system, "# Exec output summary") {
			var payload struct{ Output, PreviousSummary string }
			if err := json.Unmarshal([]byte(body.Input[1].Content), &payload); err != nil {
				t.Error(err)
			}
			if len([]rune(payload.Output)) > 32768 {
				t.Error("summary request contains an unbounded output chunk")
			}
			if summaries > 0 && payload.PreviousSummary != "Earlier failures retained." {
				t.Error("previous chunk evidence missing")
			}
			if len(body.Tools) != 0 {
				t.Error("summarizer exposes tools")
			}
			summaries++
			supplied.WriteString(payload.Output)
			writeObservationResponse(w, []any{observationMessage(`{"Summary":"Earlier failures retained."}`)})
			return
		}
		if strings.Contains(system, "工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
			return
		}
		decisions++
		if decisions == 1 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "chunks", "name": "exec", "arguments": `{"command":"python3","args":["-c","import sys; sys.stdout.write('界'*40000); sys.stderr.write('FINAL_DIAGNOSTIC'); sys.exit(7)"]}`}})
			return
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := process.Run(execSummaryConfig(t, server.URL), durable); err != nil {
		t.Fatal(err)
	}
	if summaries < 2 || strings.Count(supplied.String(), "界") != 40000 || !strings.Contains(supplied.String(), "FINAL_DIAGNOSTIC") {
		t.Fatalf("chunked summary lost output: requests=%d", summaries)
	}
	var content []struct{ Text string }
	if err := json.Unmarshal([]byte(durable.ToolExecutions[0].Result), &content); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Summary    string
		ExitCode   int `json:"exit_code"`
		Summarized bool
	}
	if err := json.Unmarshal([]byte(content[0].Text), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || !result.Summarized || result.Summary != "Earlier failures retained." {
		t.Fatalf("lost command status or summary: %+v", result)
	}
}

func TestReactSummaryFailureRecordsKnownExecOutcomeWithoutRawOutput(t *testing.T) {
	durable := &state.DurableState{RunId: "summary-failure", Goal: "check", CompletionCriteria: []string{"checked"}}
	decisions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input []struct{ Role, Content string }
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(body.Input[0].Content, "# Exec output summary") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"summarizer unavailable"}}`))
			return
		}
		decisions++
		writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "failed-summary", "name": "exec", "arguments": `{"command":"python3","args":["-c","print('DISCARD_ME'*5000)"]}`}})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var saved *state.DurableState
	process.SetCheckpoint(func() error { saved = durable.Clone(); return nil })
	_, _, err = process.Run(execSummaryConfig(t, server.URL), durable)
	if err == nil || !strings.Contains(err.Error(), "summarize exec output") || decisions != 1 {
		t.Fatalf("summary failure did not stop scheduling: %v", err)
	}
	call := saved.ToolExecutions[0]
	var content []struct{ Text string }
	if err := json.Unmarshal([]byte(call.Result), &content); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Summarized   bool
		SummaryError string `json:"summary_error"`
		ExitCode     int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(content[0].Text), &result); err != nil {
		t.Fatal(err)
	}
	if call.Status != "success" || call.Error != "" || result.ExitCode != 0 || result.Summarized || result.SummaryError == "" || strings.Contains(call.Result, "DISCARD_ME") {
		t.Fatalf("summary failure lost known execution status or retained raw output: %+v", result)
	}
}

func TestReactSummarizesEscapedExecOutputWithinRequestBudget(t *testing.T) {
	durable := &state.DurableState{RunId: "escaped", Goal: "check", CompletionCriteria: []string{"checked"}}
	decisions := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input []struct{ Role, Content string }
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(body.Input[0].Content, "# Exec output summary") {
			if len(body.Input[1].Content) > 32768 {
				t.Error("escaped summary payload exceeded request budget")
			}
			writeObservationResponse(w, []any{observationMessage(`{"Summary":"Backslash output checked."}`)})
			return
		}
		if strings.Contains(body.Input[0].Content, "工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
			return
		}
		decisions++
		if decisions == 1 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "escaped", "name": "exec", "arguments": `{"command":"python3","args":["-c","print(chr(92)*40000)"]}`}})
			return
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := process.Run(execSummaryConfig(t, server.URL), durable); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(durable.ToolExecutions[0].Result, "Backslash output checked.") {
		t.Fatal("escaped output was not summarized")
	}
}

// Checkpoint bytes are the public recovery boundary: resume must use the stored
// summary as the original call's result, including read_tool_result requests.
func TestReactResumesAndReadsStoredExecSummaryWithoutRerunningCommand(t *testing.T) {
	durable := &state.DurableState{RunId: "resume-summary", Goal: "check", CompletionCriteria: []string{"checked"}}
	phase, decisions, summaries := 1, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input []struct {
				Role, Content, Output string
				CallID                string `json:"call_id"`
			}
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		system := body.Input[0].Content
		if strings.Contains(system, "# Exec output summary") {
			summaries++
			writeObservationResponse(w, []any{observationMessage(`{"Summary":"Stored check summary."}`)})
			return
		}
		if strings.Contains(system, "工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
			return
		}
		decisions++
		if phase == 1 && decisions == 1 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_exec", "call_id": "original", "name": "exec", "arguments": `{"command":"python3","args":["-c","print('RAW_RESUME'*5000)"]}`}})
			return
		}
		if phase == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"decision temporarily unavailable"}}`))
			return
		}
		input, _ := json.Marshal(body.Input)
		if !strings.Contains(string(input), "Stored check summary.") || strings.Contains(string(input), strings.Repeat("RAW_RESUME", 10)) {
			t.Error("resume lost summary or restored raw output")
		}
		if decisions == 1 {
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_read", "call_id": "read-summary", "name": "read_tool_result", "arguments": `{"call_id":"original","offset":0,"limit":4000}`}})
			return
		}
		if !strings.Contains(string(input), "read-summary") {
			t.Error("stored summary was not available through read_tool_result")
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	cfg := execSummaryConfig(t, server.URL)
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint []byte
	process.SetCheckpoint(func() error { var err error; checkpoint, err = json.Marshal(durable); return err })
	if _, _, err := process.Run(cfg, durable); err == nil {
		t.Fatal("expected decision interruption")
	}
	if summaries == 0 || strings.Contains(string(checkpoint), strings.Repeat("RAW_RESUME", 10)) {
		t.Fatal("checkpoint retained raw output")
	}
	var restored state.DurableState
	if err := json.Unmarshal(checkpoint, &restored); err != nil {
		t.Fatal(err)
	}
	phase, decisions = 2, 0
	before := summaries
	resumed, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resumed.Run(cfg, &restored); err != nil {
		t.Fatal(err)
	}
	if summaries != before || len(restored.ToolExecutions) != 2 || restored.ToolExecutions[0].ToolCallId != "original" || restored.ToolExecutions[1].ToolName != "read_tool_result" {
		t.Fatal("resume summarized or executed the original command again")
	}
}

func TestReactExecSummaryPreservesShortOutputAndTimeoutMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, script        string
		summarized, timeout bool
	}{
		{"short", "print('short diagnostic')", false, false},
		{"line protocol summary", "print('payload'*6000)", true, false},
		{"timeout", "import sys,time; print('payload'*6000,flush=True); time.sleep(20)", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			durable := &state.DurableState{RunId: "metadata", Goal: "check", CompletionCriteria: []string{"checked"}}
			decisions, summaries := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Input []struct{ Role, Content string }
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if strings.Contains(body.Input[0].Content, "# Exec output summary") {
					summaries++
					writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSUMMARY=\"Relevant exec diagnostics.\"\nEND RESULT")})
					return
				}
				if strings.Contains(body.Input[0].Content, "工具结果观察器") {
					writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
					return
				}
				decisions++
				if decisions == 1 {
					args, _ := json.Marshal(map[string]any{"command": "python3", "args": []string{"-c", tc.script}})
					writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "metadata", "name": "exec", "arguments": string(args)}})
					return
				}
				writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
			}))
			defer server.Close()
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{Model: "test", ApiKey: "test", BaseUrl: server.URL, OutputSchema: config.LineProtocolLLMOutputSchema}}
			toolCfg := filemcp.Config{Workspace: t.TempDir()}
			if tc.timeout {
				toolCfg.Timeout = 300 * time.Millisecond
			}
			process, err := NewReactProcess[ReactResult](t.Context(), toolCfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := process.Run(cfg, durable); err != nil {
				t.Fatal(err)
			}
			call := durable.ToolExecutions[0]
			if (summaries > 0) != tc.summarized {
				t.Fatalf("unexpected summarization requests: %d", summaries)
			}
			if !tc.summarized && (!strings.Contains(call.Result, "short diagnostic") || strings.Contains(call.Result, "summarized")) {
				t.Fatal("short output was changed")
			}
			if tc.summarized && !strings.Contains(call.Result, "Relevant exec diagnostics.") {
				t.Fatal("line protocol summary missing")
			}
			if tc.timeout && (call.Status != "failed" || !strings.Contains(call.Result, "timeout") || call.Error == "") {
				t.Fatal("timeout metadata or tool failure status lost")
			}
		})
	}
}

func TestReactExecSummaryRetriesFormatErrorsWithTrailingUserCorrections(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			durable := &state.DurableState{RunId: "summary-format", Goal: "check", CompletionCriteria: []string{"checked"}}
			decisions, attempts := 0, 0
			chunk := ""
			invalid := `{"Summary":`
			valid := `{"Summary":"Corrected exec evidence."}`
			if schema == config.LineProtocolLLMOutputSchema {
				invalid = "BEGIN RESULT\nSUMMARY=unquoted\nEND RESULT"
				valid = "BEGIN RESULT\nSUMMARY=\"Corrected exec evidence.\"\nEND RESULT"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Input []struct{ Role, Content string }
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if strings.Contains(body.Input[0].Content, "# Exec output summary") {
					if chunk == "" {
						chunk = body.Input[1].Content
					}
					if body.Input[1].Content == chunk {
						attempts++
						if len(body.Input) != attempts+1 {
							t.Errorf("attempt %d: missing cumulative user corrections", attempts)
						}
						for _, message := range body.Input[2:] {
							if message.Role != "user" || !strings.Contains(message.Content, strconv.Quote(invalid)) || !strings.Contains(message.Content, "format") || !strings.Contains(message.Content, "Retry") {
								t.Errorf("correction did not report invalid format as trailing user message: %+v", message)
							}
						}
						if attempts <= 3 {
							writeObservationResponse(w, []any{observationMessage(invalid)})
							return
						}
					} else if len(body.Input) != 2 {
						t.Error("corrections leaked into the next output chunk")
					}
					writeObservationResponse(w, []any{observationMessage(valid)})
					return
				}
				if strings.Contains(body.Input[0].Content, "工具结果观察器") {
					writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
					return
				}
				decisions++
				if decisions == 1 {
					writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "format", "name": "exec", "arguments": `{"command":"python3","args":["-c","print('evidence'*6000)"]}`}})
					return
				}
				writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent.yaml")
			data := "name: react\nlog_level: error\nbase:\n  allow_tools: [exec]\n  llm:\n    model: test\n    api_key: test\n    base_url: " + server.URL + "\n    output_schema: line_protocol_llm_output\nagents:\n  compact:\n    llm:\n      model: test\n      api_key: test\n      base_url: " + server.URL + "\n      output_schema: " + string(schema) + "\n"
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadAgentConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := process.Run(cfg, durable); err != nil {
				t.Fatal(err)
			}
			if attempts != 4 || !strings.Contains(durable.ToolExecutions[0].Result, "Corrected exec evidence.") {
				t.Fatalf("format corrections did not recover: attempts=%d", attempts)
			}
		})
	}
}

func TestReactExecSummaryIncludesFewShotForConfiguredFormat(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		t.Run(string(schema), func(t *testing.T) {
			durable := &state.DurableState{RunId: "few-shot", Goal: "check", CompletionCriteria: []string{"checked"}}
			decisions, summaries := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Input []struct{ Role, Content string }
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				system := body.Input[0].Content
				if strings.Contains(system, "# Exec output summary") {
					summaries++
					for _, example := range []string{"Example 1", "Example 2", "TestCache", "cache_test.go:42", "PreviousSummary", "TestTimeout"} {
						if !strings.Contains(system, example) {
							t.Errorf("summary instructions missing few-shot evidence %q", example)
						}
					}
					if schema == config.JsonLLMOutputSchema {
						if strings.Count(system, `{"Summary":`) < 2 {
							t.Error("missing JSON summary examples")
						}
						writeObservationResponse(w, []any{observationMessage(`{"Summary":"Evidence summarized."}`)})
					} else {
						if strings.Count(system, "BEGIN RESULT\nSUMMARY=") < 2 {
							t.Error("missing Line Protocol summary examples")
						}
						writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSUMMARY=\"Evidence summarized.\"\nEND RESULT")})
					}
					return
				}
				if strings.Contains(system, "工具结果观察器") {
					if schema == config.JsonLLMOutputSchema {
						writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
					} else {
						writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
					}
					return
				}
				decisions++
				if decisions == 1 {
					writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "examples", "name": "exec", "arguments": `{"command":"python3","args":["-c","print('payload'*6000)"]}`}})
					return
				}
				if schema == config.JsonLLMOutputSchema {
					writeObservationResponse(w, []any{observationMessage(`{"Status":"completed","Answer":"done"}`)})
				} else {
					writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
				}
			}))
			defer server.Close()
			cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{config.ExecMCPToolName: {}}, LLMConfig: &config.LLMConfig{Model: "test", ApiKey: "test", BaseUrl: server.URL, OutputSchema: schema}}
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := process.Run(cfg, durable); err != nil {
				t.Fatal(err)
			}
			if summaries == 0 {
				t.Fatal("summary prompt was never exercised")
			}
		})
	}
}

func TestReactExecSummaryStopsAfterThreeFormatRetries(t *testing.T) {
	for _, tc := range []struct {
		name, output, problem string
		toolCall              bool
	}{
		{"malformed JSON", `{"Summary":`, "unexpected end", false},
		{"empty summary", `{"Summary":""}`, "nonempty", false},
		{"oversized summary", `{"Summary":"` + strings.Repeat("x", 5000) + `"}`, "token budget", false},
		{"unexpected tool call", "", "tool calls", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			durable := &state.DurableState{RunId: "format-limit", Goal: "check", CompletionCriteria: []string{"checked"}}
			attempts, decisions := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body struct {
					Input []struct{ Role, Content string }
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if strings.Contains(body.Input[0].Content, "# Exec output summary") {
					attempts++
					if attempts > 1 {
						last := body.Input[len(body.Input)-1]
						if len(body.Input) != attempts+1 || last.Role != "user" || !strings.Contains(last.Content, tc.problem) {
							t.Errorf("missing specific format correction: %+v", last)
						}
					}
					if tc.toolCall {
						writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_fake", "call_id": "fake", "name": "exec", "arguments": `{"command":"go","args":["test"]}`}})
					} else {
						writeObservationResponse(w, []any{observationMessage(tc.output)})
					}
					return
				}
				decisions++
				writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc", "call_id": "limit", "name": "exec", "arguments": `{"command":"python3","args":["-c","print('DISCARD_OUTPUT'*4000)"]}`}})
			}))
			defer server.Close()
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = process.Run(execSummaryConfig(t, server.URL), durable)
			if err == nil || !strings.Contains(err.Error(), "output format errors exceeded limit (3)") || attempts != 4 || decisions != 1 {
				t.Fatalf("expected initial request plus three corrections, then stop: requests=%d decisions=%d err=%v", attempts, decisions, err)
			}
			call := durable.ToolExecutions[0]
			if call.Status != "success" || !strings.Contains(call.Result, "summary_error") || strings.Contains(call.Result, "DISCARD_OUTPUT") {
				t.Fatal("format exhaustion lost known exec outcome or retained original output")
			}
		})
	}
}
