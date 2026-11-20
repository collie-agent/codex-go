package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactImplementationHandoffRestoresCompactedSourceForPatch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	initial := map[string]string{
		"src/main.go":      "package main\n\nconst amount = 17\n",
		"src/main_test.go": "package main\n\nconst expected = 17\n// after the read page\n",
	}
	resetFiles := func() {
		for path, content := range initial {
			if err := os.WriteFile(filepath.Join(workspace, path), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	resetFiles()

	requests := 0
	var originalEvidence string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		switch {
		case strings.Contains(body.Input[0].Content, "# 执行上下文压缩"):
			writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"both constants were inspected","SourceCallIDs":["source-read","test-read"]}],"Actions":[],"OpenIssues":[]}`)})
			return
		case strings.Contains(body.Input[0].Content, "你是 ReAct agent 的工具结果观察器"):
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		requests++
		switch requests {
		case 1:
			writeObservationResponse(w, modificationTool("source-read", "read_file", map[string]any{"path": "src/./main.go", "offset": 3, "limit": 20}))
		case 2:
			w.Header().Set("Content-Type", "application/json")
			// 强制下轮等待压缩，免去测试对后台任务完成时机的依赖。
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp_read", "status": "completed", "usage": map[string]int{"input_tokens": 70000},
				"output": modificationTool("test-read", "read_file", map[string]any{"path": "src/main_test.go", "limit": 3}),
			})
		case 3:
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"task_type":"modification","basis":"requested fix","choice":"implement","target":"change both constants","target_files":["src/main.go","src/main_test.go"],"evidence":"both constants were inspected","next_operation":"apply patch"}}`)})
		case 4:
			var evidence string
			for _, message := range body.Input {
				if _, section, ok := strings.Cut(message.Content, "# 实施交接原文"); ok {
					evidence, _, _ = strings.Cut(section, "\nImplementation state:")
				}
			}
			if originalEvidence == "" {
				originalEvidence = evidence
			} else if originalEvidence != evidence {
				t.Error("restored implementation handoff differs from continuous execution")
			}
			for _, want := range []string{"src/main.go", "src/main_test.go", "source-read", "test-read", "3-3", "1-3", "3: const amount = 17", "3: const expected = 17", "未读取第 1-2 行", "读取截断，后续原文从第 4 行开始", "历史"} {
				if !strings.Contains(evidence, want) {
					t.Errorf("implementation evidence missing %q", want)
				}
			}
			patch := "--- src/main.go\n+++ src/main.go\n@@ -3 +3 @@\n"
			oldSource := strings.Contains(evidence, "3: const amount = 17")
			oldTest := strings.Contains(evidence, "3: const expected = 17")
			if !oldSource || !oldTest {
				http.Error(w, "patch needs both original lines", http.StatusBadRequest)
				return
			}
			patch += "-const amount = 17\n+const amount = 23\n"
			patch += "--- src/main_test.go\n+++ src/main_test.go\n@@ -3 +3 @@\n-const expected = 17\n+const expected = 23\n"
			writeObservationResponse(w, modificationTool("patch", "apply_patch", map[string]string{"patch": patch}))
		case 5:
			writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"both files changed"}`)})
		default:
			t.Error("unexpected model request")
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	durable := &state.DurableState{ExecutionID: "handoff-source", Goal: "change both constants", CompletionCriteria: []string{"both files changed"}}
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{
		ExplorationBudget: config.DefaultExplorationBudget(),
		AllowTools:        map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ApplyPatchMCPToolName: {}},
		LLMConfig:         &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema},
	}
	cfg.ExplorationBudget.InitialTurns = 2
	var handoffCheckpoint []byte
	process.SetCheckpoint(func() error {
		var progress struct{ Exploration struct{ Phase string } }
		if err := json.Unmarshal(durable.ExecutionProgress, &progress); err == nil && progress.Exploration.Phase == "implement" && handoffCheckpoint == nil {
			saved, err := json.Marshal(durable)
			handoffCheckpoint = saved
			return err
		}
		return nil
	})
	assertPatch := func(process *ReactProcess[ReactResult], durable *state.DurableState) {
		t.Helper()
		result, _, err := process.Run(cfg, durable)
		if err != nil || result.Status != CompletedReactStatus || requests != 5 {
			t.Fatalf("result=%+v requests=%d err=%v", result, requests, err)
		}
		for path, want := range map[string]string{
			"src/main.go":      "package main\n\nconst amount = 23\n",
			"src/main_test.go": "package main\n\nconst expected = 23\n// after the read page\n",
		} {
			content, err := os.ReadFile(filepath.Join(workspace, path))
			if err != nil || string(content) != want {
				t.Errorf("%s=%q err=%v", path, content, err)
			}
		}
		var progress struct{ Exploration struct{ Used int } }
		if err := json.Unmarshal(durable.ExecutionProgress, &progress); err != nil || progress.Exploration.Used != 2 || len(durable.ToolExecutions) != 3 {
			t.Fatalf("expected two exploration turns and only two reads plus patch: %s, err=%v", durable.ExecutionProgress, err)
		}
	}
	assertPatch(process, durable)

	resetFiles()
	var restored state.DurableState
	if err := json.Unmarshal(handoffCheckpoint, &restored); err != nil {
		t.Fatal(err)
	}
	requests = 3 // 恢复后的第一轮直接请求实施。
	process, err = NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	assertPatch(process, &restored)
}

func TestReactResumeHandoffSourcesFitContextAndExposeGaps(t *testing.T) {
	textResult := func(source string) string {
		result, err := json.Marshal([]map[string]string{{"type": "text", "text": source}})
		if err != nil {
			t.Fatal(err)
		}
		return string(result)
	}
	read := func(path, id, source string, batch, lastLine int) *state.ToolResult {
		return &state.ToolResult{
			ToolCallId: id, ToolName: "read_file", Status: "success", Result: textResult(source), Batch: batch,
			FileEvidence: &filemcp.FileEvidence{Path: path, StartLine: 1, EndLine: lastLine},
		}
	}
	recovered := read("recovered.go", "compacted-read", "1: const recovered = true", 4, 1)
	readback, err := json.Marshal(map[string]string{"call_id": "compacted-read", "content": recovered.Result})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{
		ExecutionID: "handoff-gaps", Goal: "update the target files", CompletionCriteria: []string{"source updated"},
		ExecutionCompaction: json.RawMessage(`{"ExecutionID":"handoff-gaps","ThroughBatch":4,"Summary":{"Facts":[],"Actions":[],"OpenIssues":[]}}`),
		ExecutionProgress: json.RawMessage(`{"ExecutionID":"handoff-gaps","UsedTurns":6,"ObservedThroughBatch":5,"Exploration":{
			"Policy":{"initial_turns":5,"extension_turns":1,"max_extensions":1},"Used":5,"TaskType":"modification","Phase":"implement","ModificationLeft":2,
			"Handoff":{"target_files":["large.go","small_test.go","visible.go","missing.go","recovered.go"],"target":"update the files","evidence":"read available files","next_operation":"apply patch"}}}`),
		ToolExecutions: []*state.ToolResult{
			read("large.go", "large-read", "1: // header\n2: "+strings.Repeat("长", 6000)+"\n3: // omitted tail", 1, 3),
			read("small_test.go", "obsolete-read", "1: const obsolete = true", 2, 1),
			read("small_test.go", "latest-read", "1: const expected = true", 3, 1),
			{ToolCallId: "failed-read", ToolName: "read_file", Arguments: `{"path":"missing.go"}`, Status: "failed", Error: "file not accessible", Batch: 4},
			recovered,
			read("visible.go", "visible-read", "1: const visible = true", 5, 1),
			{ToolCallId: "read-back", ToolName: "read_tool_result", Status: "success", Batch: 5,
				Arguments:       `{"call_id":"compacted-read","offset":0,"limit":100}`,
				HistoryEvidence: &state.HistoryReadEvidence{SourceCallID: "compacted-read", Start: 0, End: utf8.RuneCountInString(recovered.Result), EOF: true},
				Result:          textResult(string(readback))},
		},
		PreviousRunOutcomes: []state.RunOutcome{{ToolResults: []*state.ToolResult{read("missing.go", "other-execution", "1: const unrelated = true", 1, 1)}}},
	}
	// 旧检查点只有 MCP 文本和调用参数，也应按原行号恢复。
	durable.ToolExecutions[2].FileEvidence = nil
	durable.ToolExecutions[2].Arguments = `{"path":"./small_test.go"}`
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "small_test.go"), []byte("const current = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests++
		if requests == 1 {
			var evidence string
			for _, message := range body.Input {
				if _, section, ok := strings.Cut(message.Content, "# 实施交接原文"); ok {
					evidence, _, _ = strings.Cut(section, "\nImplementation state:")
				}
			}
			if len(evidence) > 8*1024 || !utf8.ValidString(evidence) {
				t.Errorf("handoff evidence exceeded the byte budget or lost UTF-8, bytes=%d", len(evidence))
			}
			for _, want := range []string{"large.go", "large-read", "lines=1-1", "1: // header", "截断", "small_test.go", "latest-read", "1: const expected = true", "missing.go", "缺失"} {
				if !strings.Contains(evidence, want) {
					t.Errorf("resumed handoff evidence missing %q", want)
				}
			}
			for _, unexpected := range []string{"omitted tail", "obsolete-read", "visible-read", "other-execution", "failed-read", "const current", "compacted-read"} {
				if strings.Contains(evidence, unexpected) {
					t.Errorf("unexpected evidence %q", unexpected)
				}
			}
			last := body.Input[len(body.Input)-1].Content
			if !strings.Contains(last, "剩余修改窗口为 2 轮") {
				t.Errorf("saved window lost: %s", last)
			}
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"choice":"extend","gap":"missing.go source","blocking_reason":"need hunk context","investigation":"read missing.go","exit_condition":"required line is available"}}`)})
			return
		}
		if requests == 2 {
			writeObservationResponse(w, []any{observationMessage(`{"exploration":{"choice":"unable","reason":"fixture stops after extension","execution_state":"no changes made"}}`)})
			return
		}
		writeObservationResponse(w, []any{observationMessage(`{"status":"failed","answer":"fixture stops after extension"}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
	_, _, err = process.Run(cfg, durable)
	if err == nil || !strings.Contains(err.Error(), "fixture stops after extension") {
		t.Fatalf("expected scripted stop after extension, got %v", err)
	}
	var progress struct {
		Exploration struct{ Used, Extensions, ModificationLeft int }
	}
	if err := json.Unmarshal(durable.ExecutionProgress, &progress); err != nil ||
		progress.Exploration.Used != 5 || progress.Exploration.Extensions != 1 || progress.Exploration.ModificationLeft != 2 || len(durable.ToolExecutions) != 7 {
		t.Fatalf("resumed budget or tools changed unexpectedly: %s, err=%v", durable.ExecutionProgress, err)
	}
}
