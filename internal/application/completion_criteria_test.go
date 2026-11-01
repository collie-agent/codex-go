package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/collie-agent/codex-go/internal/testutil"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
)

func TestReactFirstCompletionPersistsCriteria(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	var stderr bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r, "Answer the original question") {
			return
		}
		writeResult(w, `{"status":"completed","answer":"first answer"}`)
	}))
	defer server.Close()
	opts := application.Options{ConfigPath: agentConfig(t, server.URL), Task: "answer the question", Workspace: t.TempDir(), Timeout: time.Second, Stderr: &stderr}
	if _, err := application.Run(ctx, opts); err != nil {
		t.Fatal(err)
	}
	ids := regexp.MustCompile(`run_id=([a-f0-9]+)`).FindStringSubmatch(stderr.String())
	if len(ids) != 2 {
		t.Fatalf("missing run ID: %s", &stderr)
	}
	saved, err := store.Get(ctx, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.CompletionCriteria, []string{"Answer the original question"}) {
		t.Fatalf("saved criteria = %v", saved.CompletionCriteria)
	}
}

func TestReactCriteriaSurviveFailedRunAndResume(t *testing.T) {
	_, store := runStore(t)
	ctx := context.Background()
	run, err := store.Create(ctx, "", "write result.txt", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	criteria := []string{"result.txt contains the requested result"}
	turn := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage
			Model string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Model == "observer" {
			writeResult(w, `{"ConfirmedFacts":[],"ReportedErrors":[]}`)
			return
		}
		turn++
		switch turn {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"criteria","status":"completed","output":[{"type":"function_call","id":"fc_criteria","call_id":"criteria","name":"update_completion_criteria","arguments":%q}]}`, `{"completion_criteria":["result.txt contains the requested result"]}`)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"write","status":"completed","output":[{"type":"function_call","id":"fc_write","call_id":"write","name":"apply_patch","arguments":%q}]}`, `{"patch":"--- /dev/null\n+++ result.txt\n@@ -0,0 +1 @@\n+requested result\n"}`)
		case 3:
			saved, err := store.Get(ctx, run.RunID)
			if err != nil || !reflect.DeepEqual(saved.CompletionCriteria, criteria) {
				t.Errorf("criteria must be saved before the next decision: %v, %v", saved.CompletionCriteria, err)
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"model unavailable"}}`)
		default:
			if strings.Contains(string(req.Input), "GenerateCompletionCriteria") || !strings.Contains(string(req.Input), criteria[0]) {
				t.Error("resume must use saved criteria")
			}
			writeResult(w, `{"status":"completed","answer":"done","completion_criteria":["replacement must be ignored"]}`)
		}
	}))
	defer server.Close()
	path := agentConfig(t, server.URL)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("  react:\n    allow_tools: [apply_patch]\n  observe:\n    llm:\n      model: observer\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	opts := application.Options{ConfigPath: path, RunID: run.RunID, Timeout: time.Second, Stderr: io.Discard}
	if _, err := application.Resume(ctx, opts); err == nil {
		t.Fatal("expected provider failure")
	}
	saved, err := store.Get(ctx, run.RunID)
	if err != nil || saved.Status != "failed" || !reflect.DeepEqual(saved.CompletionCriteria, criteria) {
		t.Fatalf("failed run lost criteria: %+v, %v", saved, err)
	}
	got, err := application.Resume(ctx, opts)
	if err != nil || len(got) != 1 || got[0].Answer != "done" {
		t.Fatalf("resume = %+v, %v", got, err)
	}
	saved, err = store.Get(ctx, run.RunID)
	if err != nil || !reflect.DeepEqual(saved.CompletionCriteria, criteria) {
		t.Fatalf("resume changed criteria: %v, %v", saved.CompletionCriteria, err)
	}
	content, err := os.ReadFile(filepath.Join(run.Workspace, "result.txt"))
	if err != nil || string(content) != "requested result\n" {
		t.Fatalf("tool result = %q, %v", content, err)
	}
}

func TestReactCriteriaCheckpointFailureStopsExecution(t *testing.T) {
	for _, tools := range []bool{false, true} {
		t.Run(fmt.Sprintf("with text=%v", tools), func(t *testing.T) {
			db, store := runStore(t)
			ctx := context.Background()
			run, err := store.Create(ctx, "", "write result.txt", t.TempDir(), "react")
			if err != nil {
				t.Fatal(err)
			}
			trigger := "criteria_fail_" + run.RunID
			// 仅拒绝首次标准写入，运行初始化仍可成功；模拟真实数据库 checkpoint 失败。
			sql := "CREATE TRIGGER " + trigger + " BEFORE UPDATE ON agent_run FOR EACH ROW BEGIN IF NEW.run_id = '" + run.RunID + "' AND JSON_TYPE(NEW.completion_criteria) = 'ARRAY' AND JSON_LENGTH(NEW.completion_criteria) > 0 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'criteria checkpoint unavailable'; END IF; END"
			if err := db.Exec(sql).Error; err != nil {
				t.Fatal(err)
			}
			defer db.Exec("DROP TRIGGER " + trigger)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var output []any
				if tools {
					output = append(output, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": `I will set the completion criteria first.`}}})
				}
				output = append(output, map[string]any{"type": "function_call", "id": "fc_criteria", "call_id": "criteria", "name": "update_completion_criteria", "arguments": `{"completion_criteria":["result.txt contains the requested result"]}`})
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "criteria", "status": "completed", "output": output})
			}))
			defer server.Close()
			path := agentConfig(t, server.URL)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("  react:\n    allow_tools: [apply_patch]\n")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			results, err := application.Resume(ctx, application.Options{ConfigPath: path, RunID: run.RunID, Timeout: time.Second, Stderr: io.Discard})
			if err == nil || !strings.Contains(err.Error(), "criteria checkpoint unavailable") || len(results) != 0 || calls != 1 {
				t.Fatalf("results=%+v err=%v requests=%d", results, err, calls)
			}
			if _, err := os.Stat(filepath.Join(run.Workspace, "result.txt")); !os.IsNotExist(err) {
				t.Fatalf("tool ran despite checkpoint failure: %v", err)
			}
			saved, err := store.Get(ctx, run.RunID)
			if err != nil || len(saved.CompletionCriteria) != 0 {
				t.Fatalf("criteria unexpectedly persisted: %v, %v", saved.CompletionCriteria, err)
			}
		})
	}
}
