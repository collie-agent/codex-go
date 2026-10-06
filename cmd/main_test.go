package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				input, _ := request["input"].(string)
				for _, required := range []string{"# Required result schema", "status:", "answer:", `"completed"`, `"failed"`} {
					if !strings.Contains(input, required) {
						t.Errorf("model request missing result requirement %q", required)
					}
				}
				if !strings.Contains(input, tc.body[:strings.Index(tc.body, "任务完成")]) {
					// 两种格式分别给出可解析的字段名与示例，任务本身无需提供协议。
					t.Errorf("model request missing %s result example", tc.name)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_final", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": tc.body}}}},
				})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent.yaml")
			contents := "name: react\nllm:\n  base_url: " + server.URL + "\n  api_key: test\n  model: test\n  output_schema: " + tc.schema + "\n"
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runAgent(path, "完成任务", t.TempDir(), 5*time.Second); err != nil {
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "resp_final", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": tc.body}}}},
				})
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "agent.yaml")
			contents := "name: react\nllm:\n  base_url: " + server.URL + "\n  api_key: test\n  model: test\n  output_schema: " + tc.schema + "\n"
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runAgent(path, "完成任务", t.TempDir(), time.Second); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestRunAgentToolTimeoutDoesNotLimitModelRequests(t *testing.T) {
	const toolTimeout = 50 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	contents := "name: react\nllm:\n  base_url: " + server.URL + "\n  api_key: test\n  model: test\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runAgent(path, "完成任务", t.TempDir(), toolTimeout); err != nil {
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
		var request struct {
			Input string `json:"input"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests++
		if len(request.Tools) != 1 || request.Tools[0].Name != "read_file" {
			t.Errorf("model saw tools %v, want only read_file", request.Tools)
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
		if !strings.Contains(request.Input, "依赖工具结果") {
			t.Errorf("second model request did not contain tool result: %s", request.Input)
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
	config := "name: react\nllm:\n  base_url: " + server.URL + "\n  api_key: test\n  model: test\nallow_tools:\n  - read_file\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runAgent(configPath, "读取 notes.txt", workspace, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("got %d model requests, want tool turn and final turn", requests)
	}
}

func TestRunAgentRejectsModelCallOutsideYAMLAllowedTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Tools) != 1 || request.Tools[0].Name != "read_file" {
			t.Errorf("model saw tools %v, want only read_file", request.Tools)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_tool", "status": "completed",
			"output": []any{map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "list_dir", "arguments": `{"path":"."}`}},
		})
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	config := "name: react\nllm:\n  base_url: " + server.URL + "\n  api_key: test\n  model: test\nallow_tools: [read_file]\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	err := runAgent(configPath, "列出目录", t.TempDir(), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "tool list_dir is not allowed") {
		t.Fatalf("error=%v, want unallowed tool rejection", err)
	}
}

func TestRunAgentWithEmptyYAMLAllowedToolsExposesNoTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Tools) != 0 {
			t.Errorf("model saw tools %s, want none", request.Tools)
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
	config := "name: react\nllm:\n  base_url: " + server.URL + "\n  api_key: test\n  model: test\nallow_tools: []\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runAgent(configPath, "直接回答", t.TempDir(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestRunAgentReportsModelFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	contents := "name: react\nllm:\n  base_url: " + server.URL + "\n  api_key: test\n  model: test\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runAgent(path, "完成任务", t.TempDir(), 5*time.Second); err == nil || !strings.Contains(err.Error(), "create response") {
		t.Fatalf("error=%v, want model failure", err)
	}
}

func TestRunRejectsMissingWorkspaceDirectory(t *testing.T) {
	if err := run(t.TempDir()+"/missing", 30*time.Second); err == nil {
		t.Fatal("expected invalid workspace to fail")
	}
}
