package consumer_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/consumer"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/entity"
	"github.com/collie-agent/codex-go/internal/provider"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/server"
	"github.com/collie-agent/codex-go/internal/testutil"
	"github.com/redis/go-redis/v9"
)

func configuration(t *testing.T, addr, endpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	data := fmt.Sprintf("name: react\nredis:\n  addr: %s\nbase:\n  llm:\n    base_url: %s\n    api_key: test\n    model: executor\n", addr, endpoint)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConsumerCompletesTerminalTasksAfterOutputButNotExecutionFailure(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for consumer integration tests")
	}
	t.Setenv("CODEX_MYSQL_URL", raw)
	db, err := database.Open(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	client, addr := testutil.StartRedis(t)
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		requests <- string(body)
		if strings.Contains(string(body), "fail queued task") {
			http.Error(w, "model rejected task", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "business failure task") {
			fmt.Fprint(w, `{"id":"consumer-failure","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"status\":\"failed\",\"answer\":\"source unavailable\"}"}]}]}`)
			return
		}
		fmt.Fprint(w, `{"id":"consumer-test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"queue task done\"}"}]}]}`)
	}))
	defer server.Close()
	message, err := json.Marshal(entity.TaskMessage{Input: "inspect queued workspace", TaskId: "queued-task"})
	if err != nil {
		t.Fatal(err)
	}
	// 固定协议字面量独立于编码器，坏消息和失败任务不能阻塞后续任务。
	if err := client.RPush(t.Context(), rdb.TASK_QUEUE_NAME,
		`not-json`, `null`, `{}`, `{"Input":"missing ID"}`, `{"TaskId":"missing input"}`,
		`{"Input":"fail queued task","TaskId":"failed-task"}`,
		`{"Input":"business failure task","TaskId":"business-failed-task"}`, message).Err(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	done := make(chan error, 1)
	opts := application.Options{ConfigPath: configuration(t, addr, server.URL), Workspace: t.TempDir(), Timeout: time.Second, Stderr: io.Discard}
	go func() { done <- consumer.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
			for taskID, want := range map[string]int{"queued-task": 1, "business-failed-task": 1, "failed-task": 0} {
				events, err := client.XRange(context.Background(), rdb.STREAM_NAME+"-"+taskID, "-", "+").Result()
				if err != nil {
					t.Error(err)
					continue
				}
				count := 0
				for _, event := range events {
					if event.Values["type"] == "task_completed" {
						count++
					}
				}
				if count != want {
					t.Errorf("task %s: completion events = %d, want %d", taskID, count, want)
				}
			}
		case <-time.After(3 * time.Second):
			t.Error("consumer did not exit")
		}
	})
	for _, want := range []string{"fail queued task", "business failure task", "inspect queued workspace"} {
		select {
		case body := <-requests:
			if !strings.Contains(body, want) {
				t.Fatalf("model did not receive queued input %q in order: %s", want, body)
			}
		case <-ctx.Done():
			t.Fatal("queued task did not reach application.Run")
		}
	}
	// XREAD BLOCK 0 也必须受测试截止时间约束，输出关联回归时应失败而非挂起。
	reader := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	defer reader.Close()
	manager := rdb.NewRedisManagerWithClient(reader)
	lastID := ""
	sawAnswer := false
	for {
		events, err := manager.ReadTaskStream(ctx, "queued-task", lastID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			lastID = event.ID
			if event.Type == "text" && strings.Contains(event.Delta, "queue task done") {
				sawAnswer = true
			}
			if event.Type == "task_completed" {
				if event.ID == "" {
					t.Fatal("completion missing Redis stream ID")
				}
				replay, err := manager.ReadTaskStream(ctx, "queued-task", "0-0")
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, saved := range replay {
					if saved.ID == event.ID {
						found = true
					}
				}
				if !found {
					t.Fatal("completion unavailable in Redis replay")
				}

				if event.RunID == "" || event.RunID == "queued-task" || event.AgentInstanceID != "" || event.ParentAgentInstanceID != "" || event.AgentName != "" {
					t.Fatalf("wrong run summary attribution: %+v", event)
				}
				if !sawAnswer || event.Delta != "{}" {
					t.Fatalf("completion must follow task output with empty JSON payload: sawAnswer=%v event=%+v", sawAnswer, event)
				}
				return
			}
		}
	}
}

func TestConsumerContinuesConversationFromMessage(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for consumer integration tests")
	}
	t.Setenv("CODEX_MYSQL_URL", raw)
	db, err := database.Open(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	store := database.NewRunStore(db)
	prior, err := store.Create(t.Context(), "", "inspect workspace", workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	prior.Status = "completed"
	prior.Result = json.RawMessage(`{"status":"completed","answer":"previous conversation evidence"}`)
	if err := store.Save(t.Context(), &prior); err != nil {
		t.Fatal(err)
	}
	client, addr := testutil.StartRedis(t)
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		requests <- string(body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"conversation-test","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"continued\"}"}]}]}`)
	}))
	defer server.Close()
	// 使用协议字面量，避免编码器与解码器同时遗漏 ConvId 而误通过。
	message := fmt.Sprintf(`{"Input":"continue previous task","TaskId":"conversation-task","ConvId":%q}`, prior.ConvID)
	if err := client.RPush(t.Context(), rdb.TASK_QUEUE_NAME, message).Err(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	done := make(chan error, 1)
	opts := application.Options{ConfigPath: configuration(t, addr, server.URL), Workspace: workspace, Timeout: time.Second, Stderr: io.Discard}
	go func() { done <- consumer.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("consumer did not exit")
		}
	})
	reader := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	defer reader.Close()
	manager := rdb.NewRedisManagerWithClient(reader)
	lastID := ""
	var info provider.TaskInfoOutput
	for {
		events, err := manager.ReadTaskStream(ctx, "conversation-task", lastID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			lastID = event.ID
			if event.Type == "task_info" {
				if err := json.Unmarshal([]byte(event.Delta), &info); err != nil {
					t.Fatal(err)
				}
			}
			if event.Type == "task_completed" {
				if info.ConvID != prior.ConvID || info.RunID == "" || info.RunID == prior.RunID {
					t.Errorf("expected a new run in conversation %s, got %+v", prior.ConvID, info)
				}
				select {
				case body := <-requests:
					if !strings.Contains(body, "previous conversation evidence") {
						t.Errorf("model input missing previous conversation result: %s", body)
					}
				case <-ctx.Done():
					t.Fatal("queued task did not reach the model")
				}
				return
			}
		}
	}
}

func TestConsumerStopsWhileQueueIsEmpty(t *testing.T) {
	_, addr := testutil.StartRedis(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	opts := application.Options{ConfigPath: configuration(t, addr, "http://localhost"), Workspace: t.TempDir(), Timeout: time.Second, Stderr: io.Discard}
	go func() {
		done <- consumer.Run(ctx, opts)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("consumer did not stop waiting for a task")
	}
}

func TestConsumerRejectsInvalidModeBeforeRemovingTask(t *testing.T) {
	client, addr := testutil.StartRedis(t)
	if err := client.RPush(t.Context(), rdb.TASK_QUEUE_NAME, `{"Input":"keep task","TaskId":"keep"}`).Err(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := consumer.Run(ctx, application.Options{ConfigPath: configuration(t, addr, "http://localhost"), Workspace: t.TempDir(), Mode: "unknown", Timeout: time.Second, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("expected invalid mode error, got %v", err)
	}
	if count := client.LLen(t.Context(), rdb.TASK_QUEUE_NAME).Val(); count != 1 {
		t.Fatalf("invalid startup configuration removed queued task: remaining=%d", count)
	}
}

func TestConsumerResumesOriginalExecution(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL")
	}
	t.Setenv("CODEX_MYSQL_URL", raw)
	db, err := database.Open(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	store := database.NewRunStore(db)
	run, err := store.Create(t.Context(), "", "original goal", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, ExecutionResult: []byte(`{"status":"completed","answer":"saved answer"}`)}
	if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, "react"); err != nil {
		t.Fatal(err)
	}
	client, addr := testutil.StartRedis(t)
	if err := client.RPush(t.Context(), rdb.TASK_QUEUE_NAME, fmt.Sprintf(`{"RunId":%q,"TaskId":"resume-task"}`, run.RunID)).Err(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- consumer.Run(ctx, application.Options{ConfigPath: configuration(t, addr, "http://127.0.0.1:1"), Workspace: t.TempDir(), Timeout: time.Second, Stderr: io.Discard})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	reader := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	defer reader.Close()
	manager := rdb.NewRedisManagerWithClient(reader)
	last := ""
	for {
		events, err := manager.ReadTaskStream(ctx, "resume-task", last)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			last = event.ID
			if event.Type == "task_completed" {
				saved, err := store.Get(t.Context(), run.RunID)
				if err != nil || saved.Status != "completed" {
					t.Fatalf("run=%+v error=%v", saved, err)
				}
				return
			}
		}
	}
}

func TestConsumerResumesMultiAgentEntry(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL")
	}
	t.Setenv("CODEX_MYSQL_URL", raw)
	db, err := database.Open(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	store := database.NewRunStore(db)
	run, err := store.Create(t.Context(), "", "original goal", t.TempDir(), "multi-agent")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, ExecutionResult: []byte(`{"entry_completion_criteria":["verified"],"result":{"status":"completed","answer":"saved answer"},"next":[]}`)}
	if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, "multi-agent"); err != nil {
		t.Fatal(err)
	}
	client, addr := testutil.StartRedis(t)
	if err := client.RPush(t.Context(), rdb.TASK_QUEUE_NAME, fmt.Sprintf(`{"RunId":%q,"TaskId":"resume-task"}`, run.RunID)).Err(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := fmt.Sprintf("name: multi-agent\nredis:\n  addr: %s\nmulti-agent:\n  entry: supervisor\n  max_rounds: 3\n  max_parallel: 1\nbase:\n  llm:\n    base_url: http://127.0.0.1:1\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise\n", addr)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- consumer.Run(ctx, application.Options{ConfigPath: path, Workspace: t.TempDir(), Timeout: time.Second, Stderr: io.Discard})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	reader := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	defer reader.Close()
	manager := rdb.NewRedisManagerWithClient(reader)
	last := ""
	for {
		events, err := manager.ReadTaskStream(ctx, "resume-task", last)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			last = event.ID
			if event.Type == "task_completed" {
				saved, err := store.Get(t.Context(), run.RunID)
				if err != nil || saved.Status != "completed" {
					t.Fatalf("run=%+v error=%v", saved, err)
				}
				return
			}
		}
	}
}

func TestConsumerResumesPlanStepResult(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL")
	}
	t.Setenv("CODEX_MYSQL_URL", raw)
	db, err := database.Open(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	store := database.NewRunStore(db)
	run, err := store.Create(t.Context(), "", "original goal", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID + "-plan-step", ExecutionResult: []byte(`{"status":"completed","answer":"saved answer","Observation":["done"],"CompletionEvidence":["checked"]}`), StepExecutions: []state.StepExecution{{StepId: "step", ExecutionID: run.RunID + "-plan-step", Revision: 1, Status: state.RunningExecutionStatus, Intent: "finish", CompletionCriteria: "checked"}}}
	if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	client, addr := testutil.StartRedis(t)
	if err := client.RPush(t.Context(), rdb.TASK_QUEUE_NAME, fmt.Sprintf(`{"RunId":%q,"TaskId":"resume-task"}`, run.RunID)).Err(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- consumer.Run(ctx, application.Options{ConfigPath: configuration(t, addr, "http://127.0.0.1:1"), Workspace: t.TempDir(), Timeout: time.Second, Stderr: io.Discard})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	reader := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	defer reader.Close()
	manager := rdb.NewRedisManagerWithClient(reader)
	last := ""
	for {
		events, err := manager.ReadTaskStream(ctx, "resume-task", last)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			last = event.ID
			if event.Type == "task_completed" {
				saved, err := store.Get(t.Context(), run.RunID)
				if err != nil || saved.Status != "completed" {
					t.Fatalf("run=%+v error=%v", saved, err)
				}
				return
			}
		}
	}
}

func TestHTTPInterruptStopsConsumerRun(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for consumer integration tests")
	}
	t.Setenv("CODEX_MYSQL_URL", raw)
	db, err := database.Open(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	client, addr := testutil.StartRedis(t)
	entered := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		close(entered)
		<-r.Context().Done()
	}))
	defer model.Close()
	message, _ := json.Marshal(entity.TaskMessage{Input: "wait for interrupt", TaskId: "interrupt-integration"})
	if err := client.RPush(t.Context(), rdb.TASK_QUEUE_NAME, message).Err(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	child := exec.Command(os.Args[0], "-test.run=^TestConsumerInterruptProcess$")
	child.Env = append(os.Environ(), "CODEX_INTERRUPT_HELPER=1", "CODEX_INTERRUPT_CONFIG="+configuration(t, addr, model.URL), "CODEX_INTERRUPT_WORKSPACE="+t.TempDir())
	child.Stdout, child.Stderr = io.Discard, io.Discard
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Signal(os.Interrupt); _ = child.Wait() }()
	manager := rdb.NewRedisManagerWithClient(client)
	last := ""
	var runID string
	for runID == "" {
		events, err := manager.ReadTaskStream(ctx, "interrupt-integration", last)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			last = event.ID
			if event.Type == "task_info" {
				var info provider.TaskInfoOutput
				if err := json.Unmarshal([]byte(event.Delta), &info); err != nil {
					t.Fatal(err)
				}
				runID = info.RunID
			}
		}
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("model not entered")
	}
	store := database.NewRunStore(db)
	manager.SetRunStatusLookup(func(ctx context.Context, id string) (string, error) {
		run, err := store.Get(ctx, id)
		return run.Status, err
	})
	handler := server.New(manager, database.NewRunStore(db))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/runs/"+runID+"/interrupt", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("interrupt: %d %s", response.Code, response.Body)
	}
	interrupted := false
	for !interrupted {
		events, err := client.XRange(ctx, rdb.STREAM_NAME+"-interrupt-integration", "("+last, "+").Result()
		if err != nil {
			t.Fatalf("waiting for interrupted event: %v", err)
		}
		for _, event := range events {
			last = event.ID
			if event.Values["type"] == "task_completed" {
				t.Fatal("interrupted task reported completion")
			}
			if event.Values["type"] == "task_interrupted" {
				if event.Values["delta"] != "{}" {
					t.Fatalf("interrupted event payload: %v", event.Values["delta"])
				}
				run, err := store.Get(ctx, runID)
				if err != nil || run.Status != "interrupted" {
					t.Fatalf("interrupted event before saved state: run=%+v err=%v", run, err)
				}
				interrupted = true
			}
		}
		if !interrupted {
			select {
			case <-ctx.Done():
				t.Fatal("no interrupted event received")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	for {
		lock, err := store.AcquireRunLock(ctx, runID)
		if err == nil {
			lock.Release(ctx)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("run lock not released: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestConsumerInterruptProcess is launched by the integration test as a separate consumer process.
func TestConsumerInterruptProcess(t *testing.T) {
	if os.Getenv("CODEX_INTERRUPT_HELPER") != "1" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := consumer.Run(ctx, application.Options{ConfigPath: os.Getenv("CODEX_INTERRUPT_CONFIG"), Workspace: os.Getenv("CODEX_INTERRUPT_WORKSPACE"), Timeout: time.Minute, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHTTPInterruptExecutionModes(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for consumer integration tests")
	}
	t.Setenv("CODEX_MYSQL_URL", raw)
	db, err := database.Open(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"plan-execute", "multi-agent", "react-tools"} {
		t.Run(mode, func(t *testing.T) {
			client, addr := testutil.StartRedis(t)
			store := database.NewRunStore(db)
			workspace := t.TempDir()
			started := make(chan struct{}, 2)
			var calls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode != "multi-agent" && testutil.RespondWithCriteria(t, w, r) {
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				n := calls.Add(1)
				if mode == "react-tools" && n == 1 {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"id":"tools","status":"completed","output":[{"type":"function_call","id":"fc_a","call_id":"a","name":"exec","arguments":"{\"command\":\"python3\",\"args\":[\"-c\",\"import pathlib,time; pathlib.Path('a').touch(); time.sleep(20)\"]}"},{"type":"function_call","id":"fc_b","call_id":"b","name":"exec","arguments":"{\"command\":\"python3\",\"args\":[\"-c\",\"import pathlib,time; pathlib.Path('b').touch(); time.sleep(20)\"]}"}]}`)
					return
				}
				if mode == "multi-agent" && n == 1 {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"id":"multi","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"entry_completion_criteria\":[\"checked\"],\"result\":{\"status\":\"completed\",\"answer\":\"delegate\"},\"next\":[{\"agent\":\"worker\",\"task\":\"A\",\"completion_criteria\":[\"A checked\"]},{\"agent\":\"worker\",\"task\":\"B\",\"completion_criteria\":[\"B checked\"]}]}"}]}]}`)
					return
				}
				started <- struct{}{}
				<-r.Context().Done()
			}))
			defer model.Close()
			path := configuration(t, addr, model.URL)
			msg := entity.TaskMessage{Input: "interrupt service execution", TaskId: "service-" + mode}
			if mode == "plan-execute" {
				run, err := store.Create(t.Context(), "", "two steps", workspace, mode)
				if err != nil {
					t.Fatal(err)
				}
				durable := &state.DurableState{RunId: run.RunID, StepExecutions: []state.StepExecution{
					{StepId: "first", Revision: 1, Status: state.CompletedExecutionStatus, Intent: "first", CompletionCriteria: "done", CompletionEvidence: []string{"first evidence"}},
					{StepId: "second", Revision: 1, Status: state.PendingExecutionStatus, Intent: "second", CompletionCriteria: "done"},
				}}
				if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, mode); err != nil {
					t.Fatal(err)
				}
				msg.Input, msg.RunId = "", run.RunID
			} else if mode == "react-tools" {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("agents:\n  react:\n    allow_tools: [exec]\n")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				data := fmt.Sprintf("name: multi-agent\nlog_level: error\nredis:\n  addr: %s\nmulti-agent:\n  entry: supervisor\n  max_rounds: 5\n  max_parallel: 2\nbase:\n  llm:\n    base_url: %s\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    next_agents: [worker]\n  worker:\n    next_agents: [supervisor]\n", addr, model.URL)
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			message, _ := json.Marshal(msg)
			if err := client.RPush(t.Context(), rdb.TASK_QUEUE_NAME, message).Err(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- consumer.Run(ctx, application.Options{ConfigPath: path, Workspace: workspace, Timeout: time.Minute, Stderr: io.Discard})
			}()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Errorf("consumer: %v", err)
				}
			}()
			reader := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
			defer reader.Close()
			manager := rdb.NewRedisManagerWithClient(reader)
			last, runID := "", ""
			for runID == "" {
				events, err := manager.ReadTaskStream(ctx, msg.TaskId, last)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					last = event.ID
					if event.Type == "task_info" {
						var info provider.TaskInfoOutput
						if err := json.Unmarshal([]byte(event.Delta), &info); err != nil {
							t.Fatal(err)
						}
						runID = info.RunID
					}
				}
			}
			count := 1
			if mode == "multi-agent" {
				count = 2
			}
			if mode == "react-tools" {
				count = 0
				for {
					_, a := os.Stat(filepath.Join(workspace, "a"))
					_, b := os.Stat(filepath.Join(workspace, "b"))
					if a == nil && b == nil {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("tools did not start")
					case <-time.After(20 * time.Millisecond):
					}
				}
			}
			for i := 0; i < count; i++ {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("execution did not start")
				}
			}
			manager.SetRunStatusLookup(func(ctx context.Context, id string) (string, error) {
				run, err := store.Get(ctx, id)
				return run.Status, err
			})
			response := httptest.NewRecorder()
			server.New(manager, database.NewRunStore(db)).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/runs/"+runID+"/interrupt", nil))
			if response.Code != http.StatusAccepted {
				t.Fatalf("interrupt: %d %s", response.Code, response.Body)
			}
			for {
				run, err := store.Get(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				if run.Status == "interrupted" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("status=%s", run.Status)
				case <-time.After(20 * time.Millisecond):
				}
			}
			if mode == "plan-execute" {
				steps, err := store.Steps(ctx, runID)
				if err != nil || len(steps) != 2 || steps[0].Status != "completed" || len(steps[0].CompletionEvidence) != 1 || steps[0].CompletionEvidence[0] != "first evidence" {
					t.Fatalf("steps=%+v err=%v", steps, err)
				}
			}
			if mode == "react-tools" {
				saved, err := store.Execution(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				unknown := 0
				for _, tool := range saved.ToolExecutions {
					if (tool.ToolCallId == "a" || tool.ToolCallId == "b") && tool.Status == "" {
						unknown++
					}
				}
				if unknown != 2 || calls.Load() != 1 {
					t.Fatalf("execution=%+v calls=%d err=%v", saved, calls.Load(), err)
				}
			}
			if mode == "multi-agent" && calls.Load() != 3 {
				t.Fatalf("model calls=%d", calls.Load())
			}
		})
	}
}
