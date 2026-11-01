package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/collie-agent/codex-go/internal/testutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/database"
	"gorm.io/gorm"
)

func buildCommands(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, target := range []string{"./cmd", "./cmd/codex-cli"} {
		path := filepath.Join(t.TempDir(), "agent")
		cmd := exec.Command("go", "build", "-o", path, target)
		cmd.Dir = ".."
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v: %s", err, output)
		}
		paths = append(paths, path)
	}
	return paths
}

func configFile(t *testing.T, endpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte("name: react\nbase:\n  llm:\n    base_url: "+endpoint+"\n    api_key: test\n    model: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgentCommandsRequireDatabase(t *testing.T) {
	for _, binary := range buildCommands(t) {
		cmd := exec.Command(binary, "--config", configFile(t, "http://127.0.0.1:1"), "--workspace", t.TempDir(), "--task", "inspect")
		cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL=")
		output, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "CODEX_MYSQL_URL is required") {
			t.Fatalf("output=%s, err=%v", output, err)
		}
	}
}

func TestAgentCommandsPersistSuccessfulRuns(t *testing.T) {
	db, store := persistenceStore(t)
	_ = db
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_final","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\",\"Observation\":[\"found README\"],\"CompletionEvidence\":[\"read README\"]}"}]}]}`)
	}))
	defer server.Close()
	mcp := filepath.Join(t.TempDir(), "mcp")
	build := exec.Command("go", "build", "-o", mcp, "./cmd/mcp")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build MCP: %v: %s", err, output)
	}
	for i, binary := range buildCommands(t) {
		workspace := t.TempDir()
		args := []string{"--config", configFile(t, server.URL), "--task", "inspect", "--workspace", workspace}
		if i == 1 {
			args = append(args, "--mode", "react", "--mcp-command", mcp)
		}
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+os.Getenv("CODEX_TEST_MYSQL_URL"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("run: %v: %s", err, &stderr)
		}
		ids := regexp.MustCompile(`run_id=([a-f0-9]+) conv_id=([a-f0-9]+)`).FindStringSubmatch(stderr.String())
		if len(ids) != 3 {
			t.Fatalf("missing IDs: %s", &stderr)
		}
		saved, err := store.Get(context.Background(), ids[1])
		if err != nil {
			t.Fatal(err)
		}
		var result agent.ReactResult
		if err := json.Unmarshal(saved.Result, &result); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != "final answer: done\n" || saved.Status != "completed" || saved.ConvID != ids[2] || saved.Goal != "inspect" || saved.Mode != "react" || saved.Workspace != workspace || result.Answer != "done" || len(result.Observation) != 1 || result.Observation[0] != "found README" || len(result.CompletionEvidence) != 1 || result.CompletionEvidence[0] != "read README" {
			t.Fatalf("stdout=%q, run=%+v, result=%+v", &stdout, saved, result)
		}
	}
}

func persistenceStore(t *testing.T) (*gorm.DB, *database.RunStore) {
	t.Helper()
	url := os.Getenv("CODEX_TEST_MYSQL_URL")
	if url == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	db, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db, database.NewRunStore(db)
}

func TestMigrationCommandIsExplicitAndRepeatable(t *testing.T) {
	db, store := persistenceStore(t)
	_ = db
	run, err := store.Create(context.Background(), "", "preserve migration data", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "migrate")
	build := exec.Command("go", "build", "-o", binary, "./cmd/migrate")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build migration: %v: %s", err, output)
	}
	for range 2 {
		cmd := exec.Command(binary)
		cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+os.Getenv("CODEX_TEST_MYSQL_URL"))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("migrate: %v: %s", err, output)
		}
	}
	saved, err := store.Get(context.Background(), run.RunID)
	if err != nil || saved.Goal != "preserve migration data" {
		t.Fatalf("migration lost run: %+v, %v", saved, err)
	}
}

func TestServerDefaultsToReactWithoutAnalyzer(t *testing.T) {
	_, store := persistenceStore(t)
	var observed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		var request struct{ Model, Input string }
		if err := decodeModelRequest(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		text := `{"status":"completed","answer":"done"}`
		if request.Model == "analyzer" {
			t.Error("default entry must not call analyzer")
		}
		if request.Model != "analyzer" {
			ids := regexp.MustCompile(`## RunId\n\n> ([a-f0-9]+)`).FindStringSubmatch(request.Input)
			if len(ids) != 2 {
				t.Error("missing run identity in model context")
				return
			}
			run, err := store.Get(context.Background(), ids[1])
			if err != nil || run.Status != "running" || run.Mode != "react" || len(run.CompletionCriteria) != 1 {
				t.Errorf("run was not checkpointed before execution: %+v, %v", run, err)
			}
			observed.Store(true)
			text = `{"status":"completed","answer":"done"}`
		}
		writeModelResult(w, text)
	}))
	defer server.Close()
	config := configFile(t, server.URL)
	binaries := buildCommands(t)
	mcp := buildMCP(t)
	cmd := exec.Command(binaries[1], "--config", config, "--task", "inspect and report", "--workspace", t.TempDir(), "--mcp-command", mcp)
	cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+os.Getenv("CODEX_TEST_MYSQL_URL"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("execute: %v: %s", err, output)
	}
	if !observed.Load() {
		t.Fatal("ReAct did not execute")
	}
}

func writeModelResult(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_final", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}})
}

func buildMCP(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/mcp")
	cmd.Dir = ".."
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build MCP: %v: %s", err, output)
	}
	return path
}

func TestCheckpointFailureStopsToolsAndSuppressesUnsavedAnswers(t *testing.T) {
	db, _ := persistenceStore(t)
	binaries := buildCommands(t)
	mcp := buildMCP(t)
	for i, binary := range binaries {
		for _, stage := range []string{"before tools", "final result"} {
			t.Run(fmt.Sprintf("entry%d/%s", i, stage), func(t *testing.T) {
				workspace := t.TempDir()
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if testutil.RespondWithCriteria(t, w, r) {
						return
					}
					var req struct{ Input json.RawMessage }
					if err := decodeModelRequest(r.Body, &req); err != nil {
						t.Error(err)
						return
					}
					if requests.Add(1) == 1 {
						var input string
						if err := json.Unmarshal(req.Input, &input); err != nil {
							t.Error(err)
							return
						}
						ids := regexp.MustCompile(`## RunId\n\n> ([a-f0-9]+)`).FindStringSubmatch(input)
						if len(ids) != 2 {
							t.Error("missing run identity")
							return
						}
						trigger := "reject_" + ids[1]
						// 真实 MySQL 在模型响应后拒绝此 run 的 checkpoint；其他运行不受影响。
						sql := "CREATE TRIGGER " + trigger + " BEFORE UPDATE ON agent_run FOR EACH ROW BEGIN IF NEW.run_id = '" + ids[1] + "' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'checkpoint unavailable'; END IF; END"
						if err := db.Exec(sql).Error; err != nil {
							t.Error(err)
							return
						}
						t.Cleanup(func() {
							if err := db.Exec("DROP TRIGGER IF EXISTS " + trigger).Error; err != nil {
								t.Error(err)
							}
						})
						if stage == "before tools" {
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprint(w, `{"id":"resp_tool","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"apply_patch","arguments":"{\"patch\":\"--- /dev/null\\n+++ b/marker.txt\\n@@ -0,0 +1 @@\\n+executed\\n\"}"}]}`)
							return
						}
					}
					writeModelResult(w, `{"status":"completed","answer":"unsaved answer"}`)
				}))
				defer server.Close()
				config := configFile(t, server.URL)
				f, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("  allow_tools: [apply_patch]\n")
				_ = f.Close()
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"--config", config, "--workspace", workspace, "--task", "write marker"}
				if i == 1 {
					args = append(args, "--mode", "react", "--mcp-command", mcp)
				}
				cmd := exec.Command(binary, args...)
				cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+os.Getenv("CODEX_TEST_MYSQL_URL"))
				var stdout, stderr bytes.Buffer
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				err = cmd.Run()
				if err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "checkpoint unavailable") {
					t.Fatalf("err=%v stdout=%s stderr=%s", err, &stdout, &stderr)
				}
				if _, err := os.Stat(filepath.Join(workspace, "marker.txt")); !os.IsNotExist(err) {
					t.Fatalf("tool executed after checkpoint became unavailable: %v", err)
				}
				if requests.Load() != 1 {
					t.Fatalf("scheduled further model requests after checkpoint failure: %d", requests.Load())
				}
			})
		}
	}
}

func TestAgentCommandsRetainFailedRunsInExistingConversation(t *testing.T) {
	_, store := persistenceStore(t)
	parent, err := store.Create(context.Background(), "", "parent task", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		var req struct{ Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			return
		}
		ids := regexp.MustCompile(`## RunId\n\n> ([a-f0-9]+)`).FindStringSubmatch(req.Input)
		if len(ids) != 2 {
			t.Error("missing identity before model execution")
			return
		}
		run, err := store.Get(context.Background(), ids[1])
		if err != nil || run.Status != "running" || run.Goal != "fail this task" || run.ConvID != parent.ConvID {
			t.Errorf("run was not stored before execution: %+v, %v", run, err)
		}
		writeModelResult(w, `{"status":"failed","answer":"permission denied"}`)
	}))
	defer server.Close()
	mcp := buildMCP(t)
	seen := map[string]bool{parent.RunID: true}
	for i, binary := range buildCommands(t) {
		args := []string{"--config", configFile(t, server.URL), "--workspace", t.TempDir(), "--task", "fail this task", "--conv-id", parent.ConvID}
		if i == 1 {
			args = append(args, "--mode", "react", "--mcp-command", mcp)
		}
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+os.Getenv("CODEX_TEST_MYSQL_URL"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil {
			t.Fatal("failed task exited successfully")
		}
		ids := regexp.MustCompile(`run_id=([a-f0-9]+) conv_id=([a-f0-9]+)`).FindStringSubmatch(stderr.String())
		if len(ids) != 3 || ids[2] != parent.ConvID || seen[ids[1]] {
			t.Fatalf("incorrect IDs: %s", &stderr)
		}
		seen[ids[1]] = true
		run, err := store.Get(context.Background(), ids[1])
		if err != nil || run.Status != "failed" || !strings.Contains(run.Error, "permission denied") || !strings.Contains(string(run.Result), "permission denied") || stdout.Len() != 0 {
			t.Fatalf("failed run=%+v, stdout=%s, err=%v", run, &stdout, err)
		}
	}
}

func TestAgentCommandsResumePlanFinalizesSavedLastStepWithoutModelCalls(t *testing.T) {
	_, store := persistenceStore(t)
	t.Setenv("CODEX_MYSQL_URL", os.Getenv("CODEX_TEST_MYSQL_URL"))
	run, err := store.Create(context.Background(), "", "inspect and report", t.TempDir(), "plan-execute")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "running"
	run.Mode = "plan-execute"
	result := agent.ReactResult{Status: "completed", Answer: "already saved", Observation: []string{"observed"}, CompletionEvidence: []string{"verified"}}
	run.Result, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCheckpoint(context.Background(), &run, []database.StepExecution{{
		RunID: run.RunID, StepID: "final", Status: "completed", Intent: "report findings",
		CompletionCriteria: "findings reported", CompletionEvidence: []string{"verified"},
		Observation: []string{"observed"}, Revision: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(config, []byte("name: plan-execute\nbase:\n  llm:\n    base_url: http://127.0.0.1:1\n    api_key: test\n    model: test\n"), 0600); err != nil {
		t.Fatal(err)
	}

	for i, binary := range buildCommands(t) {
		args := []string{"--config", config, "--run-id", run.RunID, "--workspace", run.Workspace}
		if i == 1 {
			args = append(args, "--mode", "plan-execute")
		}
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+os.Getenv("CODEX_TEST_MYSQL_URL"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("entry %d resume: %v: %s", i, err, &stderr)
		}
		if stdout.String() != "final answer: already saved\n" {
			t.Fatalf("entry %d output=%q, want saved answer", i, &stdout)
		}
	}
	saved, err := store.Get(context.Background(), run.RunID)
	if err != nil || saved.Status != "completed" {
		t.Fatalf("run was not finalized: %+v, %v", saved, err)
	}
}

func TestAgentCommandsResumeSavedPlanAndSkipCompletedSteps(t *testing.T) {
	_, store := persistenceStore(t)
	t.Setenv("CODEX_MYSQL_URL", os.Getenv("CODEX_TEST_MYSQL_URL"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		var req struct{ Model, Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Model != "executor" || !strings.Contains(req.Input, "## CurrentStep\n\n> finish report") {
			t.Errorf("resume replanned or selected the wrong step: model=%q input=%s", req.Model, req.Input)
		}
		writeModelResult(w, `{"status":"completed","answer":"report finished","Observation":["report written"],"CompletionEvidence":["report checked"]}`)
	}))
	defer server.Close()
	mcp := buildMCP(t)
	config := filepath.Join(t.TempDir(), "plan.yaml")
	yaml := "name: plan-execute\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: executor\nagents:\n  plan:\n    llm:\n      base_url: " + server.URL + "\n      api_key: test\n      model: planner\n"
	if err := os.WriteFile(config, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}

	for i, binary := range buildCommands(t) {
		run, err := store.Create(context.Background(), "", "inspect and report", t.TempDir(), "plan-execute")
		if err != nil {
			t.Fatal(err)
		}
		run.Status, run.Mode = "failed", "plan-execute"
		run.CompletionCriteria = []string{"report exists"}
		steps := []database.StepExecution{
			{RunID: run.RunID, StepID: "inspect", Status: "completed", Intent: "inspect source", CompletionCriteria: "source inspected", CompletionEvidence: []string{"source read"}, Observation: []string{"source contents retained"}, Revision: 4},
			{RunID: run.RunID, StepID: "report", Status: "running", Intent: "finish report", CompletionCriteria: "report checked", Revision: 2},
		}
		if err := store.SaveCheckpoint(context.Background(), &run, steps); err != nil {
			t.Fatal(err)
		}
		args := []string{"--config", config, "--run-id", run.RunID, "--workspace", run.Workspace}
		if i == 1 {
			args = append(args, "--mode", "plan-execute", "--mcp-command", mcp)
		}
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+os.Getenv("CODEX_TEST_MYSQL_URL"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("entry %d resume: %v: %s", i, err, &stderr)
		}
		if stdout.String() != "final answer: report finished\n" {
			t.Fatalf("entry %d output=%q, want resumed answer", i, &stdout)
		}
		saved, err := store.Get(context.Background(), run.RunID)
		if err != nil || saved.Status != "completed" || saved.Goal != "inspect and report" || saved.Mode != "plan-execute" || !strings.Contains(string(saved.Result), "report checked") {
			t.Fatalf("entry %d saved run=%+v err=%v", i, saved, err)
		}
		savedSteps, err := store.Steps(context.Background(), run.RunID)
		if err != nil || len(savedSteps) != 2 || savedSteps[0].StepID != "inspect" || savedSteps[0].Revision != 4 || savedSteps[0].Observation[0] != "source contents retained" || savedSteps[1].Status != "completed" || savedSteps[1].Revision != 2 {
			t.Fatalf("entry %d saved steps=%+v err=%v", i, savedSteps, err)
		}
	}
}

func TestAgentCommandsResumeFailedReactRunAndReplayCompletedResult(t *testing.T) {
	_, store := persistenceStore(t)
	t.Setenv("CODEX_MYSQL_URL", os.Getenv("CODEX_TEST_MYSQL_URL"))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		var req struct{ Model, Input string }
		if err := decodeModelRequest(r.Body, &req); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Model != "updated-model" || !strings.Contains(req.Input, "original goal") {
			t.Errorf("resume did not use the saved goal and current model configuration: model=%q input=%s", req.Model, req.Input)
		}
		requests.Add(1)
		writeModelResult(w, `{"status":"completed","answer":"resumed result","Observation":[],"CompletionEvidence":[]}`)
	}))
	defer server.Close()
	config := configFile(t, server.URL)
	if err := os.WriteFile(config, []byte("name: react\nbase:\n  llm:\n    base_url: "+server.URL+"\n    api_key: test\n    model: updated-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mcp := buildMCP(t)
	for i, binary := range buildCommands(t) {
		workspace := t.TempDir()
		run, err := store.Create(context.Background(), "", "original goal", workspace, "react")
		if err != nil {
			t.Fatal(err)
		}
		run.Status, run.Error = "failed", "previous attempt failed"
		run.CompletionCriteria = []string{"goal checked"}
		if err := store.Save(context.Background(), &run); err != nil {
			t.Fatal(err)
		}
		args := []string{"--config", config, "--run-id", run.RunID}
		if i == 1 {
			args = append(args, "--mcp-command", mcp)
		}
		for attempt := 0; attempt < 2; attempt++ {
			cmd := exec.Command(binary, args...)
			cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+os.Getenv("CODEX_TEST_MYSQL_URL"))
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("entry %d attempt %d resume: %v: %s", i, attempt, err, &stderr)
			}
			if stdout.String() != "final answer: resumed result\n" {
				t.Fatalf("entry %d attempt %d output=%q", i, attempt, &stdout)
			}
		}
		saved, err := store.Get(context.Background(), run.RunID)
		if err != nil || saved.Status != "completed" || saved.RunID != run.RunID || saved.ConvID != run.ConvID || saved.Goal != run.Goal || saved.Workspace != workspace || saved.CompletionCriteria[0] != "goal checked" {
			t.Fatalf("entry %d changed run identity or goal: run=%+v err=%v", i, saved, err)
		}
		steps, err := store.Steps(context.Background(), run.RunID)
		if err != nil || len(steps) != 0 {
			t.Fatalf("ordinary ReAct created steps: %+v, %v", steps, err)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("model requests=%d; completed replay should not call the model", requests.Load())
	}
}

func TestResumeRejectsChangingGoalWorkspaceOrMode(t *testing.T) {
	_, store := persistenceStore(t)
	t.Setenv("CODEX_MYSQL_URL", os.Getenv("CODEX_TEST_MYSQL_URL"))
	run, err := store.Create(context.Background(), "", "original goal", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "failed"
	if err := store.Save(context.Background(), &run); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(config, []byte("name: react\nbase:\n  llm:\n    base_url: http://127.0.0.1:1\n    api_key: test\n    model: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []application.Options{
		{RunID: run.RunID, ConfigPath: config, Task: "changed goal", Timeout: time.Second},
		{RunID: run.RunID, ConfigPath: config, Workspace: t.TempDir(), Timeout: time.Second},
		{RunID: run.RunID, ConfigPath: config, Mode: "plan-execute", Timeout: time.Second},
	} {
		if _, err := application.Resume(context.Background(), opts); err == nil {
			t.Fatalf("Resume accepted an immutable run field change: %+v", opts)
		}
	}
}

func TestAgentCommandsRejectUnavailableOrUnmigratedDatabase(t *testing.T) {
	db, _ := persistenceStore(t)
	// 使用独立空库验证入口不自动迁移，不改动已迁移的测试库。
	emptyDB := fmt.Sprintf("checkpoint_unmigrated_%d", time.Now().UnixNano())
	if err := db.Exec("CREATE DATABASE " + emptyDB).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Exec("DROP DATABASE " + emptyDB).Error; err != nil {
			t.Error(err)
		}
	})
	emptyURL, err := url.Parse(os.Getenv("CODEX_TEST_MYSQL_URL"))
	if err != nil {
		t.Fatal(err)
	}
	emptyURL.Path = "/" + emptyDB
	for _, binary := range buildCommands(t) {
		for _, tc := range []struct{ url, want string }{{"mysql://test:test@127.0.0.1:1/test", "ping MySQL"}, {emptyURL.String(), "migration command"}} {
			cmd := exec.Command(binary, "--config", configFile(t, "http://127.0.0.1:1"), "--workspace", t.TempDir(), "--task", "inspect")
			cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+tc.url)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.want) || strings.Contains(stderr.String(), "run_id=") {
				t.Fatalf("unexpected database failure: stdout=%s, stderr=%s, err=%v", &stdout, &stderr, err)
			}
		}
	}
	empty, err := database.Open(context.Background(), emptyURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(empty)
	if empty.Migrator().HasTable("agent_run") || empty.Migrator().HasTable("conversation") {
		t.Fatal("agent entry migrated automatically")
	}
}

func TestMCPCommandsDoNotRequireDatabase(t *testing.T) {
	main := buildCommands(t)[0]
	for _, binary := range []string{main, buildMCP(t)} {
		cmd := exec.Command(binary, "--workspace", t.TempDir())
		cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL=")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("MCP required database: %v: %s", err, output)
		}
	}
}
