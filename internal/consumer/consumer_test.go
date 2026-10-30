package consumer_test

import (
	"context"
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

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/consumer"
	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/entity"
	"github.com/collie-agent/codex-go/internal/rdb"
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

func TestConsumerCompletesSuccessfulTaskAfterOutputButNotFailedTask(t *testing.T) {
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
		`{"Input":"fail queued task","TaskId":"failed-task"}`, message).Err(); err != nil {
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
			for taskID, want := range map[string]int{"queued-task": 1, "failed-task": 0} {
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
	for _, want := range []string{"fail queued task", "inspect queued workspace"} {
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
				if !sawAnswer || event.Delta != "{}" {
					t.Fatalf("completion must follow task output with empty JSON payload: sawAnswer=%v event=%+v", sawAnswer, event)
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
