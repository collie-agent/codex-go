package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactCompactsOlderBatchesAndContinuesReasoning(t *testing.T) {
	durable := compactionHistory(11)
	// 同批并行调用也必须纳入快照摘要，原始执行记录仍保留。
	durable.ToolExecutions = append(durable.ToolExecutions, &state.ToolResult{Batch: 11, Position: 1, ToolCallId: "parallel", ToolName: "read_file", Arguments: `{"path":"other"}`, Status: "success", Result: "parallel output"})
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		encoded, _ := json.Marshal(body["input"])
		input := string(encoded)
		if requests == 1 {
			if tools, _ := body["tools"].([]any); len(tools) != 0 {
				t.Error("compression exposes tools")
			}
			text, _ := body["text"].(map[string]any)
			format, _ := text["format"].(map[string]any)
			if format["type"] != "json_schema" || format["strict"] != true {
				t.Error("compression must use strict structured output")
			}
			for _, want := range []string{"call_1", "call_2", "call_3", "original goal", "verified"} {
				if !strings.Contains(input, want) {
					t.Errorf("compression missing %q", want)
				}
			}
			if !strings.Contains(input, "call_11") || !strings.Contains(input, "parallel output") {
				t.Error("compression omitted snapshot tail")
			}
			writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"old evidence","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
			return
		}
		if strings.Contains(input, "raw-batch-1:") || strings.Contains(input, "raw-batch-3:") {
			t.Error("old raw results still sent to reasoning")
		}
		if !strings.Contains(input, "old evidence") {
			t.Error("summary missing from reasoning")
		}
		for _, want := range []string{"call_10", "call_11", "parallel output"} {
			if strings.Contains(input, want) {
				t.Errorf("reasoning still contains covered record %q", want)
			}
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := runAfterCompaction(t, process, compactionConfig(t, server.URL), durable)
	if err != nil || result.Answer != "done" || requests != 2 {
		t.Fatalf("result=%+v requests=%d error=%v", result, requests, err)
	}
	if len(durable.ToolExecutions) != 12 || !strings.HasPrefix(durable.ToolExecutions[0].Result, "raw-batch-1:") {
		t.Fatal("compression discarded execution records")
	}
}

func compactionHistory(batches int) *state.DurableState {
	durable := &state.DurableState{RunId: "compact-run", ExecutionID: "compact-instance", Goal: "original goal", CompletionCriteria: []string{"verified"}}
	for batch := 1; batch <= batches; batch++ {
		durable.ToolExecutions = append(durable.ToolExecutions, &state.ToolResult{Batch: batch, ToolCallId: fmt.Sprintf("call_%d", batch), ToolName: "read_file", Arguments: fmt.Sprintf(`{"path":"file-%d"}`, batch), Status: "success", Result: fmt.Sprintf("raw-batch-%d:", batch) + strings.Repeat("payload ", 1100)})
	}
	// 历史批次触发压缩，最近八批保持在预算内。
	for i, call := range durable.ToolExecutions {
		if i < 3 {
			call.Result += strings.Repeat("old payload ", 3000)
		}
	}
	return durable
}

// Existing lifecycle scenarios use JSON compaction and line-protocol reasoning.
func compactionConfig(t *testing.T, url string) *config.AgentConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	data := fmt.Sprintf("name: react\nlog_level: error\nbase:\n  llm:\n    model: test\n    api_key: test\n    base_url: %s\n    output_schema: line_protocol_llm_output\nagents:\n  compact:\n    llm:\n      model: test\n      api_key: test\n      base_url: %s\n      output_schema: json_llm_output\n", url, url)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestReactRejectsUnverifiableCompaction(t *testing.T) {
	for _, evidence := range []string{`{"Text":"unattributed","SourceCallIDs":[]}`, `{"Text":"invented","SourceCallIDs":["call_missing"]}`} {
		t.Run(evidence, func(t *testing.T) {
			durable := compactionHistory(11)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests++
				writeObservationResponse(w, []any{observationMessage(`{"Facts":[` + evidence + `],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
			}))
			defer server.Close()
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = runAfterCompaction(t, process, compactionConfig(t, server.URL), durable)
			if err == nil || !strings.Contains(err.Error(), "summary") || requests != 1 || len(durable.ExecutionCompaction) != 0 {
				t.Fatalf("requests=%d compact=%s err=%v", requests, durable.ExecutionCompaction, err)
			}
		})
	}
}

func TestReactReadsOriginalCompactedToolResult(t *testing.T) {
	durable := compactionHistory(11)
	original := "原始输出abcdef"
	durable.ToolExecutions[0].Result = original
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input []struct {
				Type   string `json:"type"`
				Output string `json:"output"`
				CallID string `json:"call_id"`
			} `json:"input"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Text struct {
				Format struct {
					Type string `json:"type"`
				} `json:"format"`
			} `json:"text"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Text.Format.Type == "json_schema" {
			writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"original available","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
			return
		}
		found := false
		for _, tool := range body.Tools {
			if tool.Name == "read_tool_result" {
				found = true
			}
		}
		if !found {
			t.Error("read_tool_result missing from reasoning tools")
		}
		if page > 0 {
			foundResult := false
			for _, item := range body.Input {
				if item.Type != "function_call_output" || item.CallID != fmt.Sprintf("read_original_%d", page) {
					continue
				}
				foundResult = true
				var content []struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal([]byte(item.Output), &content); err != nil || len(content) != 1 {
					t.Errorf("invalid read response %q", item.Output)
					continue
				}
				var result struct {
					CallID     string `json:"call_id"`
					ToolName   string `json:"tool_name"`
					Status     string `json:"status"`
					Arguments  string `json:"arguments"`
					Content    string `json:"content"`
					NextOffset int    `json:"next_offset"`
					HasMore    bool   `json:"has_more"`
				}
				if err := json.Unmarshal([]byte(content[0].Text), &result); err != nil {
					t.Error(err)
				}
				wantContent, wantNext, wantMore := "原始输出", 4, true
				if page == 2 {
					wantContent, wantNext, wantMore = "abcdef", 10, false
				}
				if result.CallID != "call_1" || result.ToolName != "read_file" || result.Status != "success" || result.Arguments != `{"path":"file-1"}` || result.Content != wantContent || result.NextOffset != wantNext || result.HasMore != wantMore {
					t.Errorf("page %d = %+v", page, result)
				}
			}
			if !foundResult {
				t.Error("original result response missing")
			}
		}
		page++
		if page <= 2 {
			offset, limit := 0, 4
			if page == 2 {
				offset, limit = 4, 4000
			}
			writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_read_%d", page), "call_id": fmt.Sprintf("read_original_%d", page), "name": "read_tool_result", "arguments": fmt.Sprintf(`{"call_id":"call_1","offset":%d,"limit":%d}`, offset, limit)}})
			return
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := runAfterCompaction(t, process, compactionConfig(t, server.URL), durable)
	if err != nil || result.Answer != "done" || page != 3 {
		t.Fatalf("result=%+v page=%d err=%v", result, page, err)
	}
	if durable.ToolExecutions[0].Result != original {
		t.Fatal("original result mutated")
	}
}

func TestReactCanReadOriginalEvidenceAgain(t *testing.T) {
	durable := compactionHistory(1)
	durable.ToolExecutions[0].Result = "original evidence"
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		turn++
		var body struct {
			Input []struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			} `json:"input"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if turn == 3 {
			found := false
			for _, item := range body.Input {
				if item.Type == "function_call_output" && item.CallID == "reread_2" && strings.Contains(item.Output, "original evidence") {
					found = true
				}
			}
			if !found {
				t.Error("repeated original evidence lookup was suppressed")
			}
			writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
			return
		}
		writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", turn), "call_id": fmt.Sprintf("reread_%d", turn), "name": "read_tool_result", "arguments": `{"call_id":"call_1","offset":0,"limit":4000}`}})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := process.Run(compactionConfig(t, server.URL), durable)
	if err != nil || result.Answer != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReactReportsInvalidOriginalResultReadsAndContinues(t *testing.T) {
	for _, args := range []string{
		`{"call_id":"another-instance-call","offset":0,"limit":4000}`,
		`{"call_id":"call_1","offset":-1,"limit":4000}`,
		`{"call_id":"call_1","offset":0,"limit":0}`,
		`{"call_id":"call_1","offset":0,"limit":65537}`,
		`{"call_id":"call_1","offset":"0","limit":4000}`,
	} {
		t.Run(args, func(t *testing.T) {
			durable := compactionHistory(1)
			turn := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				turn++
				if turn == 1 {
					writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_invalid_read", "call_id": "invalid_read", "name": "read_tool_result", "arguments": args}})
					return
				}
				var body struct {
					Input []struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
						Output string `json:"output"`
					} `json:"input"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				found := false
				for _, item := range body.Input {
					if item.Type == "function_call_output" && item.CallID == "invalid_read" && strings.Contains(item.Output, "Error:") {
						found = true
					}
				}
				if !found {
					t.Error("invalid read not reported to reasoning as tool error")
				}
				writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"handled\"\nEND RESULT")})
			}))
			defer server.Close()
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := process.Run(compactionConfig(t, server.URL), durable)
			if err != nil || result.Answer != "handled" || turn != 2 {
				t.Fatalf("result=%+v turn=%d err=%v", result, turn, err)
			}
			if len(durable.ToolExecutions) != 2 || durable.ToolExecutions[1].Status != "failed" {
				t.Fatal("invalid read outcome not retained")
			}
		})
	}
}

func TestReactUsesConfiguredCompactOutput(t *testing.T) {
	for _, schema := range []string{"json_llm_output", "line_protocol_llm_output"} {
		t.Run(schema, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				requests++
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if requests == 1 {
					if body["model"] != "compact-model" {
						t.Errorf("compact model = %v", body["model"])
					}
					if tools, _ := body["tools"].([]any); len(tools) != 0 {
						t.Error("compact exposes tools")
					}
					prompt, _ := json.Marshal(body["input"])
					for _, want := range []string{"# Execution compaction", "## Output fields", "| Facts |", "| Actions |", "| OpenIssues |", "| Pending |", "| Text |", "| SourceCallIDs |", "## Additional instructions", "## Output format", "### Example 1", "### Example 2", "```", "compact custom instruction"} {
						if !strings.Contains(string(prompt), want) {
							t.Errorf("prompt missing %q", want)
						}
					}
					commonExample := "MESSAGE=\"hello\""
					if schema == "json_llm_output" {
						commonExample = `"message":"hello"`
					}
					var messages []struct {
						Content string `json:"content"`
					}
					raw, _ := json.Marshal(body["input"])
					if err := json.Unmarshal(raw, &messages); err != nil {
						t.Error(err)
						return
					}
					system := messages[0].Content
					common := strings.Index(system, commonExample)
					compact := strings.Index(system, "## Output fields")
					if common < 0 || compact < common {
						t.Error("common format examples must precede compact fields and examples")
					}
					if strings.Count(system, "## Common output format") != 1 {
						t.Error("expected one common output format section")
					}
					text, _ := body["text"].(map[string]any)
					format, _ := text["format"].(map[string]any)
					output := `{"Facts":[{"Text":"old evidence","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`
					if schema == "json_llm_output" {
						if format["type"] != "json_schema" {
							t.Error("missing JSON schema")
						}
					} else {
						if format["type"] == "json_schema" {
							t.Error("line protocol forced to JSON")
						}
						if !strings.Contains(string(prompt), "BEGIN RESULT") {
							t.Error("missing line protocol example")
						}
						output = "BEGIN RESULT\nBEGIN FACTS\nTEXT=\"old evidence\"\nSOURCECALLIDS=\"call_1\"\nEND FACTS\nEND RESULT"
					}
					writeObservationResponse(w, []any{observationMessage(output)})
					return
				}
				if body["model"] != "reasoning-model" {
					t.Errorf("reasoning model = %v", body["model"])
				}
				input, _ := json.Marshal(body["input"])
				if !strings.Contains(string(input), "old evidence") {
					t.Error("summary missing")
				}
				writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent.yaml")
			yaml := fmt.Sprintf("name: react\nlog_level: error\nbase:\n  llm:\n    model: reasoning-model\n    api_key: test\n    base_url: %s\n    output_schema: line_protocol_llm_output\nagents:\n  compact:\n    sys_prompt: compact custom instruction\n    llm:\n      model: compact-model\n      api_key: test\n      base_url: %s\n      output_schema: %s\n", server.URL, server.URL, schema)
			if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
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
			result, _, err := runAfterCompaction(t, process, cfg, compactionHistory(11))
			if err != nil || result.Answer != "done" || requests != 2 {
				t.Fatalf("result=%+v requests=%d err=%v", result, requests, err)
			}
		})
	}
}

func TestReactRetriesCompactFormatWithUserCorrections(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, recover := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/recover=%t", schema, recover), func(t *testing.T) {
				requests := 0
				wantError := "unexpected end of JSON input"
				bad := `{"Facts":`
				good := `{"Facts":[{"Text":"old evidence","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`
				if schema == config.LineProtocolLLMOutputSchema {
					wantError = "separate BEGIN FACTS / END FACTS blocks"
					bad = "```text\nBEGIN RESULT\nBEGIN FACTS\nTEXT=\"old evidence\"\nSOURCECALLIDS=\"call_1\"\nTEXT=\"second evidence\"\nSOURCECALLIDS=\"call_2\"\nEND FACTS\nEND RESULT\n```"
					good = "BEGIN RESULT\nBEGIN FACTS\nTEXT=\"old evidence\"\nSOURCECALLIDS=\"call_1\"\nEND FACTS\nBEGIN FACTS\nTEXT=\"second evidence\"\nSOURCECALLIDS=\"call_2\"\nEND FACTS\nEND RESULT"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					requests++
					if requests > 4 {
						writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
						return
					}
					var body struct {
						Input string `json:"input"`
					}
					if err := decodeCorrectionRequest(t, req.Body, &body, requests-1, "LLM output format error"); err != nil {
						t.Error(err)
					}
					if !strings.Contains(body.Input, "original goal") || !strings.Contains(body.Input, "raw-batch-1:") {
						t.Error("retry lost original context")
					}
					for i := 1; i < requests; i++ {
						if !strings.Contains(body.Input, wantError) || !strings.Contains(body.Input, fmt.Sprintf("LLM output format error (%d/3)", i)) || !strings.Contains(body.Input, fmt.Sprintf("%q", bad)) {
							t.Error("retry missing error history or invalid output")
						}
					}
					output := bad
					if recover && requests == 4 {
						output = good
					}
					writeObservationResponse(w, []any{observationMessage(output)})
				}))
				defer server.Close()
				path := filepath.Join(t.TempDir(), "retry.yaml")
				data := fmt.Sprintf("name: react\nlog_level: error\nbase:\n  llm:\n    model: test\n    api_key: test\n    base_url: %s\n    output_schema: line_protocol_llm_output\nagents:\n  compact:\n    llm:\n      model: test\n      api_key: test\n      base_url: %s\n      output_schema: %s\n", server.URL, server.URL, schema)
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
				durable := compactionHistory(11)
				result, _, err := runAfterCompaction(t, process, cfg, durable)
				if recover {
					if err != nil || result.Answer != "done" || requests != 5 || len(durable.ExecutionCompaction) == 0 {
						t.Fatalf("result=%+v requests=%d err=%v", result, requests, err)
					}
					if schema == config.LineProtocolLLMOutputSchema {
						var saved ExecutionCompaction
						if err := json.Unmarshal(durable.ExecutionCompaction, &saved); err != nil {
							t.Fatal(err)
						}
						if len(saved.Summary.Facts) != 2 || saved.Summary.Facts[0].Text != "old evidence" || len(saved.Summary.Facts[0].SourceCallIDs) != 1 || saved.Summary.Facts[0].SourceCallIDs[0] != "call_1" || saved.Summary.Facts[1].Text != "second evidence" || len(saved.Summary.Facts[1].SourceCallIDs) != 1 || saved.Summary.Facts[1].SourceCallIDs[0] != "call_2" {
							t.Fatalf("evidence lost or citations mixed: %+v", saved.Summary.Facts)
						}
					}
				} else if err == nil || !strings.Contains(err.Error(), "output format errors exceeded limit (3)") || requests != 4 || len(durable.ExecutionCompaction) != 0 || len(durable.ToolExecutions) != 11 {
					t.Fatalf("requests=%d compaction=%s err=%v", requests, durable.ExecutionCompaction, err)
				}
			})
		}
	}
}

func TestReactCompactsOversizedOlderBatch(t *testing.T) {
	durable := compactionHistory(4)
	durable.ToolExecutions[0].Result = strings.Repeat("x", 120000)
	for _, call := range durable.ToolExecutions[1:] {
		call.Result = fmt.Sprintf("raw-batch-%d:", call.Batch)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "# Execution compaction") {
			writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"old evidence","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
			return
		}
		if strings.Contains(string(encoded), strings.Repeat("x", 1000)) {
			t.Error("oversized batch reached reasoning")
		}
		if strings.Contains(string(encoded), "raw-batch-2:") {
			t.Error("covered batch reached reasoning")
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runAfterCompaction(t, process, compactionConfig(t, server.URL), durable)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReactRechecksOversizedSummary(t *testing.T) {
	durable := compactionHistory(11)
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "# Execution compaction") {
			attempts++
			summary := "short evidence"
			if attempts == 1 {
				summary = strings.Repeat("z", 120000)
			}
			result, _ := json.Marshal(ExecutionSummary{Facts: []Evidence{{Text: summary, SourceCallIDs: []string{"call_1"}}}})
			writeObservationResponse(w, []any{observationMessage(string(result))})
			return
		}
		if strings.Contains(string(encoded), strings.Repeat("z", 1000)) {
			t.Error("oversized summary reached reasoning")
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runAfterCompaction(t, process, compactionConfig(t, server.URL), durable)
	if err != nil {
		t.Fatal(err)
	}
	if attempts < 2 {
		t.Fatal("summary was not compressed again")
	}
}

func TestReactStopsWhenCompactionCannotFitBudget(t *testing.T) {
	durable := compactionHistory(4)
	durable.ToolExecutions[0].Result = strings.Repeat("x", 120000)
	for _, call := range durable.ToolExecutions[1:] {
		call.Result = fmt.Sprintf("raw-batch-%d:", call.Batch)
	}
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		if !strings.Contains(string(encoded), "# Execution compaction") {
			t.Error("reasoning continued with oversized context")
		}
		attempts++
		if attempts > 3 {
			http.Error(w, "retry limit exceeded", 500)
			return
		}
		result, _ := json.Marshal(ExecutionSummary{Facts: []Evidence{{Text: strings.Repeat("z", 120000), SourceCallIDs: []string{"call_1"}}}})
		writeObservationResponse(w, []any{observationMessage(string(result))})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runAfterCompaction(t, process, compactionConfig(t, server.URL), durable)
	if err == nil || !strings.Contains(err.Error(), "token budget") {
		t.Fatalf("expected budget error, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected bounded attempts, got %d", attempts)
	}
	if len(durable.ExecutionCompaction) != 0 {
		t.Fatal("oversized candidate was committed")
	}
	if durable.ToolExecutions[0].Result != strings.Repeat("x", 120000) {
		t.Fatal("original evidence changed")
	}
}

func TestReactDropsAllCoveredBatches(t *testing.T) {
	durable := compactionHistory(12)
	for i, call := range durable.ToolExecutions {
		call.Result = fmt.Sprintf("small-batch-%d", i+1)
	}
	durable.ToolExecutions[0].Result = strings.Repeat("x", 120000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		encoded, _ := json.Marshal(body)
		input := string(encoded)
		if strings.Contains(input, "# Execution compaction") {
			writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"old evidence","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
			return
		}
		for batch := 2; batch <= 12; batch++ {
			present := strings.Contains(input, fmt.Sprintf("small-batch-%d", batch))
			if present {
				t.Errorf("batch %d retained=%v, want no covered batches", batch, present)
			}
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := runAfterCompaction(t, process, compactionConfig(t, server.URL), durable)
	if err != nil || result.Answer != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// runAfterCompaction tests summary generation, validation and restore separately
// from asynchronous scheduling, which has its own channel-controlled tests.
func runAfterCompaction(t *testing.T, process *ReactProcess[ReactResult], cfg *config.AgentConfig, durable *state.DurableState) (ReactResult, []AgentProcessorType, error) {
	t.Helper()
	rc := state.NewReactContext(durable)
	for _, call := range durable.ToolExecutions {
		rc.AppendToolResult(call)
	}
	model := &compactModelContext{Contexter: rc}
	if len(durable.ExecutionCompaction) > 0 {
		var saved ExecutionCompaction
		if err := json.Unmarshal(durable.ExecutionCompaction, &saved); err != nil {
			t.Fatal(err)
		}
		model.compaction = &saved
	}
	if err := process.compactContext(cfg, durable, model, 0); err != nil {
		if process.mcpClientSess != nil {
			process.mcpClientSess.Close()
		}
		if process.serverSess != nil {
			process.serverSess.Close()
		}
		return ReactResult{}, nil, err
	}
	return process.Run(cfg, durable)
}

func TestAsyncCompactionSnapshotAndHandoff(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		input, _ := json.Marshal(body)
		if strings.Contains(string(input), "new-result") {
			t.Error("snapshot contains later writes")
		}
		close(started)
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"snapshot evidence","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
	}))
	defer server.Close()
	durable := compactionHistory(11)
	rc := state.NewReactContext(durable)
	for _, call := range durable.ToolExecutions {
		rc.AppendToolResult(call)
	}
	model := &compactModelContext{Contexter: rc}
	process := &BaseReactProcess[ReactResult]{ctx: t.Context()}
	checkpoints := 0
	process.checkpoint = func() error { checkpoints++; return nil }
	async := &asyncCompaction{}
	defer async.close()
	cfg := compactionConfig(t, server.URL)
	if applied, err := process.advanceCompaction(async, cfg, durable, model, 0); applied || err != nil {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not start")
	}
	// While compact is blocked, completed tool batches still append to live history.
	for batch := 12; batch <= 16; batch++ {
		call := &state.ToolResult{Batch: batch, ToolCallId: fmt.Sprintf("call_%d", batch), Result: "new-result", Status: "success"}
		durable.ToolExecutions = append(durable.ToolExecutions, call)
		rc.AppendToolResult(call)
		if applied, err := process.advanceCompaction(async, cfg, durable, model, exceedTokenThreshold+1); applied || err != nil {
			t.Fatalf("applied=%v err=%v", applied, err)
		}
	}
	if len(model.ModelRequest().ToolResults) != 16 || checkpoints != 0 {
		t.Fatal("live context changed before completion")
	}
	close(release)
	select {
	case result := <-async.pending:
		async.pending <- result
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not complete")
	}
	applied, err := process.advanceCompaction(async, cfg, durable, model, 0)
	if err != nil || !applied || checkpoints != 1 {
		t.Fatalf("applied=%v checkpoints=%d err=%v", applied, checkpoints, err)
	}
	request := model.ModelRequest()
	if len(request.ToolResults) != 5 || request.ToolResults[0].Batch != 12 || model.compaction.ThroughBatch != 11 {
		t.Fatalf("unexpected handoff: %+v", request.ToolResults)
	}
	if !strings.Contains(request.Input, "snapshot evidence") || len(durable.ToolExecutions) != 16 {
		t.Fatal("lost summary or durable history")
	}
	rc.AppendToolResult(&state.ToolResult{Batch: 17, ToolCallId: "call_17"})
	if len(model.ModelRequest().ToolResults) != 6 {
		t.Fatal("new calls must append after handoff")
	}
}

func TestAsyncCompactionCheckpointFailure(t *testing.T) {
	durable := compactionHistory(4)
	durable.ExecutionCompaction = []byte("previous")
	model := &compactModelContext{Contexter: state.NewReactContext(durable)}
	process := &BaseReactProcess[ReactResult]{ctx: t.Context(), checkpoint: func() error { return errors.New("save failed") }}
	async := &asyncCompaction{pending: make(chan compactionResult, 1), cancel: func() {}}
	async.pending <- compactionResult{compaction: &ExecutionCompaction{ThroughBatch: 4}}
	applied, err := process.advanceCompaction(async, nil, durable, model, 0)
	if applied || err == nil || model.compaction != nil || string(durable.ExecutionCompaction) != "previous" {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
}

func TestAsyncCompactionReportedTokensAndCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Consume the request before observing cancellation on the server side.
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		close(started)
		<-req.Context().Done()
	}))
	defer server.Close()
	durable := compactionHistory(4)
	for _, call := range durable.ToolExecutions {
		call.Result = "small"
	}
	rc := state.NewReactContext(durable)
	for _, call := range durable.ToolExecutions {
		rc.AppendToolResult(call)
	}
	model := &compactModelContext{Contexter: rc}
	process := &BaseReactProcess[ReactResult]{ctx: t.Context()}
	async := &asyncCompaction{}
	cfg := compactionConfig(t, server.URL)
	if _, err := process.advanceCompaction(async, cfg, durable, model, 0); err != nil || async.pending != nil {
		t.Fatal("below threshold started compaction")
	}
	if _, err := process.advanceCompaction(async, cfg, durable, model, exceedTokenThreshold+1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("reported usage did not trigger")
	}
	async.close()
	if len(durable.ExecutionCompaction) != 0 {
		t.Fatal("cancelled summary committed")
	}
}

func TestCompactionResumesWithOnlyNewEvidence(t *testing.T) {
	durable := compactionHistory(8)
	previous := ExecutionCompaction{ExecutionID: durable.ExecutionID, ThroughBatch: 5, Summary: ExecutionSummary{Facts: []Evidence{{Text: "prior evidence", SourceCallIDs: []string{"call_1"}}}}}
	durable.ExecutionCompaction, _ = json.Marshal(previous)
	for _, call := range durable.ToolExecutions {
		call.Result = fmt.Sprintf("raw-batch-%d:", call.Batch)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		input, _ := json.Marshal(body)
		for _, want := range []string{"prior evidence", "raw-batch-6:", "raw-batch-7:", "raw-batch-8:"} {
			if !strings.Contains(string(input), want) {
				t.Errorf("missing %s", want)
			}
		}
		if strings.Contains(string(input), "raw-batch-5:") {
			t.Error("repeated covered evidence")
		}
		writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"merged evidence","SourceCallIDs":["call_1","call_8"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
	}))
	defer server.Close()
	rc := state.NewReactContext(durable)
	for _, call := range durable.ToolExecutions {
		rc.AppendToolResult(call)
	}
	model := &compactModelContext{Contexter: rc, compaction: &previous}
	process := &BaseReactProcess[ReactResult]{ctx: t.Context()}
	async := &asyncCompaction{}
	defer async.close()
	cfg := compactionConfig(t, server.URL)
	if _, err := process.advanceCompaction(async, cfg, durable, model, exceedTokenThreshold+1); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-async.pending:
		async.pending <- result
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not finish")
	}
	if applied, err := process.advanceCompaction(async, cfg, durable, model, 0); !applied || err != nil {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
	if model.compaction.ThroughBatch != 8 {
		t.Fatalf("wrong watermark: %+v", model.compaction)
	}
	// Persisted handoff retains the same raw history on restore.
	var saved ExecutionCompaction
	if err := json.Unmarshal(durable.ExecutionCompaction, &saved); err != nil {
		t.Fatal(err)
	}
	restored := &compactModelContext{Contexter: rc, compaction: &saved}
	if len(restored.ModelRequest().ToolResults) != 0 {
		t.Fatal("restore changed retained history")
	}
}
