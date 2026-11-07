package main

import (
	"context"
	"encoding/json"
	"github.com/collie-agent/codex-go/internal/testutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestRunRequiresWorkspace(t *testing.T) {
	if err := run("", 30*time.Second); err == nil {
		t.Fatal("expected missing workspace to fail")
	}
}

func TestRunAgentCompletesWithBothOutputSchemas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		body   string
	}{
		{"json", "json_llm_output", `{"status":"completed","answer":"任务完成"}`},
		{"line protocol", "line_protocol_llm_output", "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"任务完成\"\nEND RESULT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if testutil.RespondWithCriteria(t, w, r) {
					return
				}
				var request map[string]any
				if err := decodeModelRequest(r.Body, &request); err != nil {
					t.Error(err)
				}
				input, _ := request["input"].(string)
				for _, required := range []string{"# Required result schema", "status:", "answer:", `"completed"`, `"failed"`} {
					if !strings.Contains(input, required) {
						t.Errorf("model request missing result requirement %q", required)
					}
				}

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_final", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": tc.body}}}},
				})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent.yaml")
			contents := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n    output_schema: " + tc.schema + "\n"
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runPersistedTestAgent(t, path, "完成任务", t.TempDir(), 5*time.Second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunAgentRejectsInvalidFinalResults(t *testing.T) {
	for _, tc := range []struct {
		name, schema, body, want string
	}{
		{"missing status", "json_llm_output", `{"answer":"done"}`, "invalid status"},
		{"empty object", "json_llm_output", `{}`, "invalid status"},
		{"null", "json_llm_output", `null`, "invalid status"},
		{"unknown status", "json_llm_output", `{"status":"pending","answer":"waiting"}`, "invalid status"},
		{"missing answer", "json_llm_output", `{"status":"completed"}`, "non-empty answer"},
		{"blank answer", "json_llm_output", `{"status":"completed","answer":" \n\t"}`, "non-empty answer"},
		{"task failure", "json_llm_output", `{"status":"failed","answer":"permission denied"}`, "permission denied"},
		{"line missing status", "line_protocol_llm_output", "BEGIN RESULT\nANSWER=\"done\"\nEND RESULT", "invalid status"},
		{"line unknown status", "line_protocol_llm_output", "BEGIN RESULT\nSTATUS=\"pending\"\nANSWER=\"waiting\"\nEND RESULT", "invalid status"},
		{"line missing answer", "line_protocol_llm_output", "BEGIN RESULT\nSTATUS=\"completed\"\nEND RESULT", "non-empty answer"},
		{"line task failure", "line_protocol_llm_output", "BEGIN RESULT\nSTATUS=\"failed\"\nANSWER=\"permission denied\"\nEND RESULT", "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if testutil.RespondWithCriteria(t, w, r) {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_final", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": tc.body}}}},
				})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent.yaml")
			contents := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n    output_schema: " + tc.schema + "\n"
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runPersistedTestAgent(t, path, "完成任务", t.TempDir(), time.Second); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunAgentToolTimeoutDoesNotLimitModelRequests(t *testing.T) {
	const toolTimeout = 50 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		// 模型耗时超过工具时限，仍须正常完成任务。
		timer := time.NewTimer(3 * toolTimeout)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_final", "status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": `{"status":"completed","answer":"done"}`}}}},
		})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	contents := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runPersistedTestAgent(t, path, "完成任务", t.TempDir(), toolTimeout); err != nil {
		t.Fatalf("model request was limited by tool timeout: %v", err)
	}
}

func TestRunAgentUsesYAMLAllowedToolAndReturnsItsResultToModel(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("依赖工具结果"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		var request struct {
			Input json.RawMessage `json:"input"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests++
		names := make([]string, 0, len(request.Tools))
		for _, tool := range request.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"load_skill", "read_file", "read_tool_result", "switch_to_plan_execute"}) {
			t.Errorf("model saw tools %v, want read_file and built-in tools", request.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "resp_tool", "status": "completed",
				"output": []any{

					map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"notes.txt"}`},
				},
			})
			return
		}
		var items []struct {
			Type, Role, Output string
			CallID             string `json:"call_id"`
		}
		if err := json.Unmarshal(request.Input, &items); err != nil {
			t.Error(err)
		} else if len(items) != 6 || items[0].Role != "system" || items[3].Type != "function_call" || items[3].CallID != "call_1" || items[4].Type != "function_call_output" || items[4].CallID != "call_1" || !strings.Contains(items[4].Output, "依赖工具结果") {
			t.Errorf("second model request did not contain native tool result: %s", request.Input)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_final", "status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
				map[string]any{"type": "output_text", "text": `{"status":"completed","answer":"已读取"}`},
			}}},
		})
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	config := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n  allow_tools:\n    - read_file\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runPersistedTestAgent(t, configPath, "读取 notes.txt", workspace, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("got %d model requests, want tool turn and final turn", requests)
	}
}

func TestRunAgentRejectsModelCallOutsideYAMLAllowedTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		names := make([]string, 0, len(request.Tools))
		for _, tool := range request.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"load_skill", "read_file", "read_tool_result", "switch_to_plan_execute"}) {
			t.Errorf("model saw tools %v, want read_file and built-in tools", request.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_tool", "status": "completed",
			"output": []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "list_dir", "arguments": `{"path":"."}`}},
		})
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	config := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n  allow_tools: [read_file]\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	err := runPersistedTestAgent(t, configPath, "列出目录", t.TempDir(), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "tool list_dir is not allowed") {
		t.Fatalf("error=%v, want unallowed tool rejection", err)
	}
}

func TestRunAgentUsesObserveAgentConfiguration(t *testing.T) {
	for _, mode := range []string{"CLI", "resolved ReAct config"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("before\n"), 0600); err != nil {
				t.Fatal(err)
			}
			observeRequest := make(chan struct {
				model string
				input string
				tools []struct {
					Name string `json:"name"`
				}
			}, 1)
			var reactRequests int
			observerStarted := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if testutil.RespondWithCriteria(t, w, r) {
					return
				}
				var request struct {
					Model string          `json:"model"`
					Input json.RawMessage `json:"input"`
					Tools []struct {
						Name string `json:"name"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				// 首轮和观察器使用字符串；工具执行后的 ReAct 使用结构化输入。
				var input string
				if err := json.Unmarshal(request.Input, &input); err != nil {
					var items []struct{ Content string }
					if err := json.Unmarshal(request.Input, &items); err != nil || len(items) == 0 {
						t.Errorf("invalid model input: %s", request.Input)
						return
					}
					for _, item := range items {
						input += item.Content + "\n"
					}
				}
				switch request.Model {
				case "react-model":
					reactRequests++
					if !strings.Contains(input, "react-prompt") || strings.Contains(input, "base-prompt") {
						t.Errorf("ReAct used wrong prompt: %s", input)
					}
					tools := make(map[string]bool)
					for _, tool := range request.Tools {
						tools[tool.Name] = true
					}
					if !tools["apply_patch"] || !tools["load_skill"] || tools["update_completion_criteria"] {
						t.Errorf("ReAct saw tools %v, want workspace tools and criteria tool only before criteria are defined", request.Tools)
					}
					if reactRequests > 1 {
						// 等待异步观察器收到请求，避免 CLI 完成后取消上下文使断言偶发失败。
						select {
						case <-observerStarted:
						case <-time.After(5 * time.Second):
							t.Error("observer request did not start before ReAct completed")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{
							"id": "resp_final", "status": "completed",
							"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
								map[string]any{"type": "output_text", "text": `{"status":"completed","answer":"done"}`},
							}}},
						})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "resp_tool", "status": "completed",
						"output": []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "apply_patch", "arguments": `{"patch":"--- /dev/null\n+++ b/created.txt\n@@ -0,0 +1 @@\n+created\n"}`}},
					})
				case "observe-model":
					observeRequest <- struct {
						model string
						input string
						tools []struct {
							Name string `json:"name"`
						}
					}{request.Model, input, request.Tools}
					close(observerStarted)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "resp_observe", "status": "completed",
						"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
							map[string]any{"type": "output_text", "text": `{"ConfirmedFacts":["created.txt created"],"ReportedErrors":[]}`},
						}}},
					})
				default:
					t.Errorf("unexpected model %q", request.Model)
				}
			}))
			defer server.Close()

			configPath := filepath.Join(t.TempDir(), "agent.yaml")
			contents := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: base-model\n  allow_tools: [apply_patch]\n  sys_prompt: base-prompt\nagents:\n  react:\n    llm:\n      base_url: " + server.URL + "\n      api_key: test\n      model: react-model\n    sys_prompt: react-prompt\n  observe:\n    llm:\n      base_url: " + server.URL + "\n      api_key: test\n      model: observe-model\n    sys_prompt: observe-prompt\n"
			if err := os.WriteFile(configPath, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "CLI" {
				if err := runPersistedTestAgent(t, configPath, "修改文件", workspace, 5*time.Second); err != nil {
					t.Fatal(err)
				}
			} else {
				loaded, err := config.LoadAgentConfig(configPath)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				process, err := agent.NewReactProcess[agent.ReactResult](ctx, filemcp.Config{Workspace: workspace, Timeout: 5 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := process.Run(loaded.ForAgent("react"), &fileContext.DurableState{RunId: "resolved-config-task", Goal: "修改文件"}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case request := <-observeRequest:
				if request.model != "observe-model" {
					t.Fatalf("observer used model %q, want observe-model", request.model)
				}
				if len(request.tools) != 0 {
					t.Fatalf("observer exposed tools: %v", request.tools)
				}
				if !strings.Contains(request.input, "工具结果观察器") || !strings.Contains(request.input, "observe-prompt") || strings.Contains(request.input, "react-prompt") {
					t.Fatalf("observer used wrong prompt: %s", request.input)
				}
				if strings.Index(request.input, "observe-prompt") > strings.Index(request.input, "# DurableState") {
					t.Fatalf("observer custom prompt must follow its built-in prompt before task data: %s", request.input)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("observer model request was not captured")
			}
		})
	}
}

func TestRunAgentWithEmptyYAMLAllowedToolsExposesOnlyBuiltInTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		var request struct {
			Tools []struct{ Name string } `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		names := make([]string, 0, len(request.Tools))
		for _, tool := range request.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"load_skill", "read_tool_result", "switch_to_plan_execute"}) {
			t.Errorf("model saw tools %v, want built-in tools", request.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_final", "status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{
				map[string]any{"type": "output_text", "text": `{"status":"completed","answer":"无需工具"}`},
			}}},
		})
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	config := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n  allow_tools: []\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runPersistedTestAgent(t, configPath, "直接回答", t.TempDir(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestRunAgentReportsModelFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if testutil.RespondWithCriteria(t, w, r) {
			return
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	contents := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runPersistedTestAgent(t, path, "完成任务", t.TempDir(), 5*time.Second); err == nil || !strings.Contains(err.Error(), "create response") {
		t.Fatalf("error=%v, want model failure", err)
	}
}

func TestRunRejectsMissingWorkspaceDirectory(t *testing.T) {
	if err := run(t.TempDir()+"/missing", 30*time.Second); err == nil {
		t.Fatal("expected invalid workspace to fail")
	}
}

// 命令入口现在必须持久化；测试只能使用显式指定的隔离测试库。
func runPersistedTestAgent(t *testing.T, configPath, task, workspace string, timeout time.Duration, runtimeDirs ...string) error {
	t.Helper()
	url := os.Getenv("CODEX_TEST_MYSQL_URL")
	if url == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistent command tests")
	}
	t.Setenv(database.MySQLURLEnv, url)
	db, err := database.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return runAgent(configPath, task, workspace, timeout, runtimeDirs...)
}

func TestResumeAgentRestoresMultiAgentEntry(t *testing.T) {
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
	durable := &fileContext.DurableState{RunId: run.RunID, ExecutionID: run.RunID, ExecutionResult: []byte(`{"entry_completion_criteria":["verified"],"result":{"status":"completed","answer":"saved CLI answer"},"next":[]}`)}
	if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, "multi-agent"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.yaml")
	config := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 3\n  max_parallel: 1\nbase:\n  llm:\n    base_url: http://127.0.0.1:1\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := resumeAgent(path, run.RunID, "", time.Second); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(t.Context(), run.RunID)
	if err != nil || saved.Status != "completed" || !strings.Contains(string(saved.Result), "saved CLI answer") {
		t.Fatalf("run=%+v error=%v", saved, err)
	}
}

func TestResumeAgentRestoresPlanStep(t *testing.T) {
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
	durable := &fileContext.DurableState{RunId: run.RunID, ExecutionID: run.RunID + "-cli-step", ExecutionResult: []byte(`{"status":"completed","answer":"saved CLI answer","Observation":["done"],"CompletionEvidence":["verified"]}`), StepExecutions: []fileContext.StepExecution{{StepId: "step", ExecutionID: run.RunID + "-cli-step", Revision: 1, Status: fileContext.RunningExecutionStatus, Intent: "finish", CompletionCriteria: "verified"}}}
	if err := store.SaveDurableCheckpoint(t.Context(), &run, durable, "plan-execute"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.yaml")
	config := "name: react\nbase:\n  llm:\n    base_url: http://127.0.0.1:1\n    api_key: test\n    model: test\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := resumeAgent(path, run.RunID, "", time.Second); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get(t.Context(), run.RunID)
	if err != nil || saved.Status != "completed" || !strings.Contains(string(saved.Result), "saved CLI answer") {
		t.Fatalf("run=%+v error=%v", saved, err)
	}
}
