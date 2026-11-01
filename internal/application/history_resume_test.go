package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
)

func TestResumeKeepsHistoricalMarkdownSeparateFromCurrentToolMessages(t *testing.T) {
	db, store := runStore(t)
	ctx := context.Background()
	prior, err := store.Create(ctx, "", "historical goal", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&prior).Update("create_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	prior.Status = "interrupted"
	if err := store.SaveDurableCheckpoint(ctx, &prior, &state.DurableState{
		RunId: prior.RunID, ExecutionID: prior.RunID,
		ToolExecutions: []*state.ToolResult{
			{ToolCallId: "history-first", ToolName: "read_file", Arguments: `{"path":"a"}`, Status: "success", Result: "first historical result", Batch: 1},
			{ToolCallId: "history-second", ToolName: "read_file", Arguments: `{"path":"b"}`, Status: "failed", Error: "permission denied", Batch: 1, Position: 1},
		},
	}, "react"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(ctx, prior.ConvID, "continue current goal", prior.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDurableCheckpoint(ctx, &current, &state.DurableState{
		RunId: current.RunID, ExecutionID: current.RunID,
		ToolExecutions: []*state.ToolResult{{ToolCallId: "current-call", ToolName: "read_file", Arguments: `{}`, Status: "success", Result: "current result"}},
	}, "react"); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Input []struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				CallID  string          `json:"call_id"`
				Output  string          `json:"output"`
				Content json.RawMessage `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		items := request.Input
		if len(items) != 4 {
			t.Errorf("want system, current call/output, current state; got %d messages", len(items))
		} else {
			if items[0].Role != "system" || items[3].Role != "user" || items[1].CallID != "current-call" || items[2].CallID != "current-call" || items[2].Output != "current result" {
				t.Errorf("current tool messages lost: %+v", items)
			}
			for _, want := range []string{"- tool_interactions:", "history-first", "history-second", "first historical result", "permission denied"} {
				if !strings.Contains(string(items[0].Content), want) {
					t.Errorf("historical Markdown missing %q", want)
				}
			}
		}
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	_, err = application.Resume(ctx, application.Options{RunID: current.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("want one model request, got %d", requests)
	}
}

func TestResumeKeepsHistoricalToolOwnershipAndUnknownOutcome(t *testing.T) {
	db, store := runStore(t)
	ctx := context.Background()
	prior, err := store.Create(ctx, "", "historical goal", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&prior).Update("create_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	prior.Status = "interrupted"
	if err := store.SaveDurableCheckpoint(ctx, &prior, &state.DurableState{
		RunId: prior.RunID, ExecutionID: prior.RunID,
		ToolExecutions: []*state.ToolResult{
			{ToolCallId: "confirmed-call", ToolName: "read_file", Arguments: `{}`, Status: "success", Result: "saved evidence"},
			{ToolCallId: "unknown-call", ToolName: "write_file", Arguments: `{"path":"a"}`},
		},
	}, "react"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(ctx, prior.ConvID, "continue", prior.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Input []struct {
				Role    string `json:"role"`
				CallID  string `json:"call_id"`
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Input) != 2 {
			t.Errorf("want historical system reference and current user; got %d messages", len(request.Input))
		} else {
			system := request.Input[0].Content
			// 历史工具保留执行实例归属和结果不确定性，不能作为当前原生工具消息。
			for _, want := range []string{prior.RunID, "- tool_interactions:", "confirmed-call", "unknown-call", "saved evidence", "outcome unknown"} {
				if !strings.Contains(system, want) {
					t.Errorf("system history missing ownership/uncertainty %q", want)
				}
			}
		}
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	_, err = application.Resume(ctx, application.Options{RunID: current.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard})
	if err != nil || requests != 1 {
		t.Fatalf("resume error=%v requests=%d", err, requests)
	}
}

// Resume 的可观察边界是发给模型的请求，历史必须按步骤进度选择内容。
func TestResumeSelectsInterruptedPlanHistoryByStepProgress(t *testing.T) {
	db, store := runStore(t)
	ctx := context.Background()
	prior, err := store.Create(ctx, "", "historical plan", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&prior).Update("create_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: prior.RunID, Goal: prior.Goal, StepExecutions: []state.StepExecution{
		{StepId: "first", Revision: 1, Intent: "first-intent", Status: state.CompletedExecutionStatus, CompletionCriteria: "first-criterion", Observation: []string{"first-observation"}, CompletionEvidence: []string{"first-evidence"}},
		{StepId: "second", Revision: 1, Intent: "second-intent", Status: state.CompletedExecutionStatus, CompletionCriteria: "second-criterion", Observation: []string{"second-observation"}, CompletionEvidence: []string{"second-evidence"}},
		{StepId: "third", Revision: 1, Intent: "third-intent", Status: state.FailedExecutionStatus, CompletionCriteria: "third-criterion"},
		{StepId: "fourth", Revision: 1, Intent: "fourth-intent", Status: state.PendingExecutionStatus, CompletionCriteria: "fourth-criterion"},
	}}
	// 用公开存储接口准备每步独立实例，避免把测试绑到数据库表结构。
	for i, name := range []string{"first", "second", "third"} {
		durable.ExecutionID = prior.RunID + "-" + name
		durable.StepExecutions[i].ExecutionID = durable.ExecutionID
		durable.ToolExecutions = []*state.ToolResult{{ToolCallId: name + "-call", ToolName: "read_file", Arguments: `{"path":"` + name + `-path"}`, Status: "success", Result: name + "-tool-result"}}
		durable.ExecutionResult = nil
		if i < 2 {
			durable.ExecutionResult = json.RawMessage(`{"status":"completed","answer":"` + name + `-answer"}`)
		}
		if err := store.SaveDurableCheckpoint(ctx, &prior, durable, "plan-execute"); err != nil {
			t.Fatal(err)
		}
	}
	prior.Status = "interrupted"
	// 保留运行级回答和完成证据，但其中的步骤和观察不能再次展开。
	prior.Result = json.RawMessage(`{"answer":"historical run answer\nsecond line","Observation":["duplicated-run-observation"],"CompletionEvidence":["historical-run-evidence", "historical-run-proof-two"],"steps":[{"Observation":["first-observation"],"CompletionEvidence":["first-evidence"]}]}`)
	if err := store.SaveDurableCheckpoint(ctx, &prior, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Create(ctx, prior.ConvID, "new task", prior.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		history := ""
		for _, item := range request.Input {
			if item.Role == "system" {
				history += item.Content
			}
		}
		for _, want := range []string{"Conversation history", "historical run answer", "historical-run-evidence", "historical-run-proof-two", "> second line", "first-intent", "second-intent", "completed", "first-observation", "second-observation", "first-evidence", "second-evidence", "third-intent", "failed", "third-criterion", "third-call", "third-path", "third-tool-result"} {
			if !strings.Contains(history, want) {
				t.Errorf("history missing %q", want)
			}
		}
		for _, absent := range []string{"duplicated-run-observation", "first-answer", "second-answer", prior.RunID + "-first", prior.RunID + "-second", "first-criterion", "second-criterion", "first-call", "second-call", "first-path", "second-path", "first-tool-result", "second-tool-result", "fourth", "pending"} {
			if strings.Contains(history, absent) {
				t.Errorf("history unexpectedly includes %q", absent)
			}
		}
		for _, summary := range []string{"historical run answer", "historical-run-evidence", "historical-run-proof-two", "first-observation", "second-observation", "first-evidence", "second-evidence"} {
			if count := strings.Count(history, summary); count != 1 {
				t.Errorf("completed step summary %q occurs %d times, want once", summary, count)
			}
		}
		writeResult(w, `{"status":"completed","answer":"done"}`)
	}))
	defer server.Close()
	if _, err := application.Resume(ctx, application.Options{RunID: current.RunID, ConfigPath: agentConfig(t, server.URL), Timeout: time.Second, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
}
