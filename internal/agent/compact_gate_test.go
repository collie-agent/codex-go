package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactWaitsForCompactionBeforeReasoningAtSyncThreshold(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "# Execution compaction") {
			writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"compressed evidence","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
			return
		}
		if !strings.Contains(string(encoded), "compressed evidence") {
			t.Error("reasoning sent before compaction was applied")
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := process.Run(compactionConfig(t, server.URL), compactionHistory(11))
	if err != nil || result.Answer != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReactPreservesEveryBatchNotCoveredBySummary(t *testing.T) {
	durable := compactionHistory(8)
	for _, call := range durable.ToolExecutions {
		call.Result = "small result"
	}
	durable.ExecutionCompaction = []byte(`{"ExecutionID":"compact-instance","ThroughBatch":1,"Summary":{"Facts":[],"Actions":[],"OpenIssues":[],"Pending":[]}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		encoded, _ := json.Marshal(body["input"])
		for _, want := range []string{"call_2", "call_3", "call_4", "call_5", "call_6", "call_7", "call_8"} {
			if !strings.Contains(string(encoded), want) {
				t.Errorf("uncovered batch missing: %s", want)
			}
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = process.Run(compactionConfig(t, server.URL), durable)
	if err != nil {
		t.Fatal(err)
	}
	if len(durable.ToolExecutions) != 8 {
		t.Fatal("durable history was replaced")
	}
}

func TestReactContinuesBelowSyncThresholdThenWaitsForPendingCompaction(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	firstReasoning, nextReasoning := make(chan struct{}), make(chan struct{})
	var compactRequests, reasoningRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "# Execution compaction") {
			if compactRequests.Add(1) != 1 {
				t.Error("started duplicate compaction")
				return
			}
			close(started)
			select {
			case <-release:
			case <-req.Context().Done():
				return
			}
			writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"async summary","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
			return
		}
		if reasoningRequests.Add(1) == 1 {
			select {
			case <-started:
			case <-req.Context().Done():
				return
			}
			close(firstReasoning)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "resp_usage", "status": "completed", "usage": map[string]any{"input_tokens": syncTokenThreshold, "output_tokens": 1, "total_tokens": syncTokenThreshold + 1}, "output": []any{map[string]any{"type": "function_call", "id": "fc_read", "call_id": "new_read", "name": "read_tool_result", "arguments": `{"call_id":"call_1","offset":0,"limit":10}`}}})
			return
		}
		close(nextReasoning)
		if !strings.Contains(string(encoded), "async summary") || !strings.Contains(string(encoded), "new_read") {
			t.Error("reasoning lost summary or appended tool interaction")
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	process, err := NewReactProcess[ReactResult](ctx, filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := compactionHistory(11)
	for _, call := range durable.ToolExecutions {
		// ASCII 约四字节一个 token，使初始上下文落在 24K 与 32K 两个阈值之间。
		call.Result = strings.Repeat("x", 10000)
	}
	cfg := compactionConfig(t, server.URL)
	done := make(chan error, 1)
	go func() { _, _, err := process.Run(cfg, durable); done <- err }()
	select {
	case <-firstReasoning:
	case <-ctx.Done():
		t.Fatal("reasoning did not continue below sync threshold")
	}
	select {
	case <-nextReasoning:
		t.Error("reasoning continued at sync threshold before summary")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if compactRequests.Load() != 1 || reasoningRequests.Load() != 2 {
		t.Fatalf("compact=%d reasoning=%d", compactRequests.Load(), reasoningRequests.Load())
	}
}

func TestReactStopsWaitingOnCancellationOrDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) {
			started := make(chan struct{})
			var reasoning atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var body map[string]any
				json.NewDecoder(req.Body).Decode(&body)
				encoded, _ := json.Marshal(body)
				if !strings.Contains(string(encoded), "# Execution compaction") {
					reasoning.Add(1)
					return
				}
				close(started)
				<-req.Context().Done()
			}))
			defer server.Close()
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(t.Context(), 200*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			process, err := NewReactProcess[ReactResult](ctx, filemcp.Config{Workspace: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			cfg := compactionConfig(t, server.URL)
			durable := compactionHistory(11)
			done := make(chan error, 1)
			go func() { _, _, err := process.Run(cfg, durable); done <- err }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("compaction did not start")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				want := context.Canceled
				if deadline {
					want = context.DeadlineExceeded
				}
				if !errors.Is(err, want) {
					t.Fatalf("err=%v want=%v", err, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("wait ignored cancellation/deadline")
			}
			if reasoning.Load() != 0 || len(durable.ExecutionCompaction) != 0 {
				t.Fatal("cancelled compaction allowed reasoning or committed summary")
			}
		})
	}
}

func TestReactRecompactsUncoveredGrowthAndFailsClosedWhenBudgetCannotFit(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var compactions, reasoning atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "# Execution compaction") {
			if compactions.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-req.Context().Done():
					return
				}
			} else if compactions.Load() == 2 && !strings.Contains(string(encoded), "new_read_1") {
				t.Error("recompaction omitted growth during first compaction")
			}
			summary := map[string]any{"Facts": []any{map[string]any{"Text": "updated summary", "SourceCallIDs": []string{"call_1"}}}, "Actions": []any{}, "OpenIssues": []any{}, "Pending": []string{}}
			if compactions.Load() > 1 {
				// 后续摘要本身仍超预算，验证三次重试后停止推理。
				summary["Pending"] = []string{strings.Repeat("p", exceedTokenThreshold*4)}
			}
			encoded, _ := json.Marshal(summary)
			writeObservationResponse(w, []any{observationMessage(string(encoded))})
			return
		}
		if reasoning.Add(1) != 1 {
			t.Error("oversized context was allowed to reason")
			writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
			return
		}
		select {
		case <-started:
		case <-req.Context().Done():
			return
		}
		calls := make([]any, 0, 3)
		for i := 1; i <= 3; i++ {
			calls = append(calls, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", i), "call_id": fmt.Sprintf("new_read_%d", i), "name": "read_tool_result", "arguments": fmt.Sprintf(`{"call_id":"call_1","offset":%d,"limit":65536}`, i-1)})
		}
		writeObservationResponse(w, calls)
		close(release)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	process, err := NewReactProcess[ReactResult](ctx, filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := compactionHistory(8)
	for _, call := range durable.ToolExecutions {
		call.Result = "small"
	}
	durable.ToolExecutions[0].Result = strings.Repeat("x", 70000)
	durable.ExecutionCompaction, _ = json.Marshal(ExecutionCompaction{ExecutionID: durable.ExecutionID, ThroughBatch: 8, Summary: ExecutionSummary{Pending: []string{strings.Repeat("p", 110000)}}})
	_, _, err = process.Run(compactionConfig(t, server.URL), durable)
	if err == nil || !strings.Contains(err.Error(), "context exceeds token budget after 3 attempts") {
		t.Fatalf("expected bounded compaction failure, got %v", err)
	}
	if compactions.Load() != 4 || reasoning.Load() != 1 {
		t.Fatalf("compact=%d reasoning=%d", compactions.Load(), reasoning.Load())
	}
	if len(durable.ToolExecutions) != 11 {
		t.Fatal("lost appended tool executions")
	}
}

func TestReactDoesNotReasonAfterCompactionFailsAtSyncThreshold(t *testing.T) {
	var reasoning atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		json.NewDecoder(req.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		if !strings.Contains(string(encoded), "# Execution compaction") {
			reasoning.Add(1)
		}
		writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"invalid evidence","SourceCallIDs":["nonexistent_call"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := compactionHistory(11)
	_, _, err = process.Run(compactionConfig(t, server.URL), durable)
	if err == nil {
		t.Fatal("compaction failure was ignored")
	}
	if reasoning.Load() != 0 || len(durable.ExecutionCompaction) != 0 {
		t.Fatal("failed summary was used or reasoning proceeded")
	}
}

// 固定规格边界，避免测试随实现常量一起漂移。
func TestCompactionThresholdsMatchSpec(t *testing.T) {
	if exceedTokenThreshold != 24*1024 || syncTokenThreshold != 32*1024 {
		t.Fatalf("thresholds = %d/%d, want 24K/32K", exceedTokenThreshold, syncTokenThreshold)
	}
}

func TestReactCorrectionBudgetBlocksOversizedRetry(t *testing.T) {
	var reasoning, compactions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "# Execution compaction") {
			compactions.Add(1)
			writeObservationResponse(w, []any{observationMessage(`{"Facts":[{"Text":"small summary","SourceCallIDs":["call_1"]}],"Actions":[],"OpenIssues":[],"Pending":[]}`)})
			return
		}
		if reasoning.Add(1) == 1 {
			// 原请求很小，但无效输出新增的纠错消息单独超过 32K。
			writeObservationResponse(w, []any{observationMessage(strings.Repeat("x", 33*1024*4))})
			return
		}
		writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
	}))
	defer server.Close()
	process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := compactionHistory(1)
	durable.ToolExecutions[0].Result = "small result"
	_, _, err = process.Run(compactionConfig(t, server.URL), durable)
	if err == nil || !strings.Contains(err.Error(), "context exceeds token budget after 3 attempts") {
		t.Fatalf("expected correction budget failure, got %v", err)
	}
	if reasoning.Load() != 1 || compactions.Load() != 3 {
		t.Fatalf("reasoning=%d compactions=%d, want 1/3", reasoning.Load(), compactions.Load())
	}
	if len(durable.ExecutionCompaction) != 0 {
		t.Fatal("failed compaction changed checkpoint")
	}
}
