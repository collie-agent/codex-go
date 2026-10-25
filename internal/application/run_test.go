package application_test

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
	"github.com/collie-agent/codex-go/internal/database"
	"gorm.io/gorm"
)

func runStore(t *testing.T) (*gorm.DB, *database.RunStore) {
	t.Helper()
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	t.Setenv("CODEX_MYSQL_URL", raw)
	db, err := database.Open(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db, database.NewRunStore(db)
}

func agentConfig(t *testing.T, endpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	// 恢复使用更新后的 react 配置，但原运行的 plan-execute 模式必须保留。
	yaml := "name: react\nbase:\n  llm:\n    base_url: " + endpoint + "\n    api_key: test\n    model: executor\nagents:\n  plan:\n    llm:\n      base_url: " + endpoint + "\n      api_key: test\n      model: planner\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeResult(w http.ResponseWriter, result string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"review","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}`, result)
}

func TestResumePreparationPreservesOriginalPlanMode(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "finish task", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	models := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		models <- req.Model
		switch req.Model {
		case "planner":
			writeResult(w, `{"StepExecutions":[{"Revision":1,"StepId":"report","Status":"pending","Intent":"finish report","CompletionCriteria":"report checked","Observation":[],"CompletionEvidence":[]}]}`)
		default:
			writeResult(w, `{"status":"completed","answer":"done","Observation":["report saved"],"CompletionEvidence":["report checked"]}`)
		}
	}))
	defer server.Close()
	results, err := application.Resume(ctx, application.Options{ConfigPath: agentConfig(t, server.URL), RunID: run.RunID, Timeout: time.Second, Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var requested []string
	for len(models) > 0 {
		requested = append(requested, <-models)
	}
	if saved.Mode != "plan-execute" || len(saved.CompletionCriteria) != 0 || len(results) != 1 || results[0].Answer != "done" || strings.Join(requested, ",") != "planner,executor" {
		t.Fatalf("resume changed preparation: mode=%q criteria=%v results=%+v models=%v", saved.Mode, saved.CompletionCriteria, results, requested)
	}
	steps, err := store.Steps(ctx, run.RunID)
	if err != nil || len(steps) != 1 || steps[0].Status != "completed" {
		t.Fatalf("steps=%+v err=%v", steps, err)
	}
}

func TestLockLossCancelsInFlightModel(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%v", resume), func(t *testing.T) {
			db, store := runStore(t)
			workspace := t.TempDir()
			run, err := store.Create(context.Background(), "", "finish task", workspace, "react")
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			canceled := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				<-r.Context().Done()
				close(canceled)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			opts := application.Options{ConfigPath: agentConfig(t, server.URL), Task: run.Goal, Workspace: workspace, Mode: "react", Timeout: time.Second, Stderr: io.Discard}
			result := make(chan error, 1)
			go func() {
				var err error
				if resume {
					opts.RunID = run.RunID
					_, err = application.Resume(ctx, opts)
				} else {
					_, err = application.Run(ctx, opts)
				}
				result <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("model did not start")
			}
			// 新建入口的 ID 在请求上下文中不可见，按此测试专用 workspace 定位运行。
			run = database.AgentRun{}
			if err := db.Where("workspace = ? AND status = ?", workspace, "running").First(&run).Error; err != nil {
				t.Fatal(err)
			}
			var owner int64
			if err := db.Raw("SELECT IS_USED_LOCK(?)", "codex:run:"+run.RunID).Scan(&owner).Error; err != nil {
				t.Fatal(err)
			}
			if owner == 0 {
				t.Fatal("run has no lock")
			}
			if err := db.Exec(fmt.Sprintf("KILL CONNECTION %d", owner)).Error; err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "run lock: connection lost") {
					t.Fatalf("lock loss error=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("lock loss did not cancel running model")
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("model request stayed active")
			}
			next, err := store.AcquireRunLock(context.Background(), run.RunID)
			if err != nil {
				t.Fatalf("cannot take over after connection loss: %v", err)
			}
			if err := next.Release(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLockLossStopsRunningTool(t *testing.T) {
	db, store := runStore(t)
	workspace := t.TempDir()
	run, err := store.Create(context.Background(), "", "run command", workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		script := "import pathlib,time\np=pathlib.Path('heartbeat')\nwhile True:\n p.write_text(str(time.monotonic_ns()))\n time.sleep(0.02)\n"
		args, _ := json.Marshal(map[string]any{"command": "python3", "args": []string{"-c", script}})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "tool-test", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "fc_exec", "call_id": "call_exec", "name": "exec", "arguments": string(args)}}})
	}))
	defer server.Close()
	cfg := agentConfig(t, server.URL)
	yaml := "name: react\nbase:\n  allow_tools: [exec]\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: executor\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := application.Resume(ctx, application.Options{ConfigPath: cfg, RunID: run.RunID, Timeout: 7 * time.Second, Stderr: io.Discard})
		result <- err
	}()
	marker := filepath.Join(workspace, "heartbeat")
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
waiting:
	for {
		select {
		case err := <-result:
			t.Fatalf("tool ended before starting: %v", err)
		case <-ctx.Done():
			t.Fatal("tool did not start")
		case <-ticker.C:
			if content, err := os.ReadFile(marker); err == nil && len(content) > 0 {
				break waiting
			}
		}
	}
	var owner int64
	if err := db.Raw("SELECT IS_USED_LOCK(?)", "codex:run:"+run.RunID).Scan(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if owner == 0 {
		t.Fatal("run has no lock")
	}
	if err := db.Exec(fmt.Sprintf("KILL CONNECTION %d", owner)).Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "run lock: connection lost") {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("lock loss did not cancel running tool")
	}
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	// 执行入口返回后，工具进程必须已停止写入，不能只取消等待工具结果。
	time.Sleep(150 * time.Millisecond)
	after, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("tool kept writing after lock loss returned")
	}
}

func TestAgentRunsShareOnlyEarlierConversationOutcomes(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%v", resume), func(t *testing.T) {
			db, store := runStore(t)
			ctx := context.Background()
			workspace := t.TempDir()
			create := func(conv, goal string, at time.Time) database.AgentRun {
				t.Helper()
				run, err := store.Create(ctx, conv, goal, workspace, "react")
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&run).Update("create_at", at).Error; err != nil {
					t.Fatal(err)
				}
				return run
			}
			now := time.Now().UTC().Truncate(time.Second)
			first := create("", "first", now.Add(-3*time.Hour))
			first.Status, first.Result = "completed", json.RawMessage(`{"status":"completed","answer":"previous-success","Observation":["full-evidence"]}`)
			if err := store.Save(ctx, &first); err != nil {
				t.Fatal(err)
			}
			failed := create(first.ConvID, "failed", now.Add(-2*time.Hour))
			failed.Status, failed.Error = "failed", "previous-error"
			if err := store.Save(ctx, &failed); err != nil {
				t.Fatal(err)
			}
			current := create(first.ConvID, "continue", now.Add(-time.Hour))
			current.Error = "current-error-excluded"
			if err := store.Save(ctx, &current); err != nil {
				t.Fatal(err)
			}
			later := create(first.ConvID, "later", now.Add(time.Hour))
			later.Error = "future-error-excluded"
			if err := store.Save(ctx, &later); err != nil {
				t.Fatal(err)
			}
			other := create("", "other", now.Add(-4*time.Hour))
			other.Error = "other-conversation-excluded"
			if err := store.Save(ctx, &other); err != nil {
				t.Fatal(err)
			}
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var decoded any
				if err := json.Unmarshal(body, &decoded); err != nil {
					t.Error(err)
				}
				// 解码外层请求后，检查完整模型输入中的 Markdown 与结构化结果。
				requests <- fmt.Sprintf("%v", decoded)
				writeResult(w, `{"status":"completed","answer":"done"}`)
			}))
			defer server.Close()
			path := agentConfig(t, server.URL)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("  react:\n    sys_prompt: history-instructions\n")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			opts := application.Options{ConfigPath: path, ConvID: first.ConvID, Task: "continue", Workspace: workspace, Timeout: time.Second, Stderr: io.Discard}
			if resume {
				opts.RunID = current.RunID
				_, err = application.Resume(ctx, opts)
			} else {
				_, err = application.Run(ctx, opts)
			}
			if err != nil {
				t.Fatal(err)
			}
			input := <-requests
			for _, want := range []string{"# Conversation history", "previous-success", "full-evidence", "previous-error"} {
				if !strings.Contains(input, want) {
					t.Errorf("model input missing %q: %s", want, input)
				}
			}
			for _, excluded := range []string{"future-error-excluded", "other-conversation-excluded"} {
				if strings.Contains(input, excluded) {
					t.Errorf("model input leaked %q", excluded)
				}
			}
			if resume && strings.Contains(input, "current-error-excluded") {
				t.Error("current run leaked into history")
			}
			if strings.Index(input, "previous-success") > strings.Index(input, "previous-error") {
				t.Error("history is not chronological")
			}
		})
	}
}
