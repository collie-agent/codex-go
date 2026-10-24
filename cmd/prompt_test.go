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

func TestRunAgentSendsYAMLSysPromptAfterBuiltInPrompt(t *testing.T) {
	const customPrompt = "完成前必须核对完成标准。"
	const builtInMarker = "你是 ReAct agent 的单轮决策器。"

	var requestInput string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			return
		}
		requestInput = request.Input
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_final","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\"}"}]}]}`))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "agent.yaml")
	contents := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n  sys_prompt: " + customPrompt + "\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runPersistedTestAgent(t, path, "完成任务", t.TempDir(), 5*time.Second); err != nil {
		t.Fatal(err)
	}

	builtInAt := strings.Index(requestInput, builtInMarker)
	customAt := strings.Index(requestInput, customPrompt)
	if builtInAt < 0 || customAt < 0 {
		t.Fatalf("model request omitted prompt content: %q", requestInput)
	}
	if builtInAt >= customAt {
		t.Fatalf("custom prompt must follow built-in ReAct prompt: %q", requestInput)
	}
}

func TestRunAgentKeepsBuiltInPromptWhenYAMLSysPromptIsEmpty(t *testing.T) {
	for _, sysPrompt := range []string{"", "  sys_prompt: \"\"\n"} {
		t.Run(strings.ReplaceAll(sysPrompt, "\n", "empty"), func(t *testing.T) {
			var requestInput string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Input string `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode model request: %v", err)
					return
				}
				requestInput = request.Input
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_final","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"{\"status\":\"completed\",\"answer\":\"done\"}"}]}]}`))
			}))
			defer server.Close()

			path := filepath.Join(t.TempDir(), "agent.yaml")
			contents := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\n" + sysPrompt
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := runPersistedTestAgent(t, path, "完成任务", t.TempDir(), 5*time.Second); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(requestInput, "你是 ReAct agent 的单轮决策器。") {
				t.Fatalf("built-in prompt missing from model request: %q", requestInput)
			}
		})
	}
}
