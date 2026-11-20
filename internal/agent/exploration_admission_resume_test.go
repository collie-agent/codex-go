package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

// 在公开检查点边界模拟进程退出，只用已成功保存的副本恢复。
func TestReactResumeImplementationAdmissionCompletesRealChange(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name := "workspace-check"
		if rejected {
			name = "rejected-investigation"
		}
		t.Run(name, func(t *testing.T) {
			remaining := 2
			if rejected {
				remaining = 3
			}
			workspace := t.TempDir()
			path := filepath.Join(workspace, "main.go")
			if err := os.WriteFile(path, []byte("package old\n"), 0600); err != nil {
				t.Fatal(err)
			}
			durable := &state.DurableState{ExecutionID: name, Goal: "fix main.go", CompletionCriteria: []string{"changed and verified"}}
			var operation []any
			// git status 在非仓库会失败；用真实仓库保证检查成功且窗口不退款。
			if !rejected {
				cmd := exec.Command("git", "init", "--quiet")
				cmd.Dir = workspace
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git init: %v %s", err, output)
				}
				operation = modificationTool("check", "exec", map[string]any{"command": "git", "args": []string{"status", "--short"}})
			} else {
				operation = modificationTool("denied", "exec", map[string]any{"command": "rg", "args": []string{"package", "main.go"}})
			}
			outputs := [][]any{modificationTool("read", "read_file", map[string]any{"path": "main.go"}), modificationHandoff(), operation}
			requests := 0
			resumed := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Input json.RawMessage }
				_ = json.NewDecoder(r.Body).Decode(&body)
				input := string(body.Input)
				if strings.Contains(input, "你是 ReAct agent 的工具结果观察器") {
					writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
					return
				}
				if resumed && requests == 0 {
					var messages []struct{ Role, Content string }
					if err := json.Unmarshal(body.Input, &messages); err != nil {
						t.Error(err)
						http.Error(w, "invalid model input", http.StatusBadRequest)
						return
					}
					last := messages[len(messages)-1]
					if last.Role != "user" || !strings.HasPrefix(last.Content, fmt.Sprintf("已进入实施阶段，剩余修改窗口为 %d 轮。", remaining)) {
						t.Errorf("restored model missing final implementation instruction: %+v", last)
					}
					for _, want := range []string{fmt.Sprintf("modification_left=%d", remaining), "phase=implement", "source inspected", "apply patch"} {
						if !strings.Contains(input, want) {
							t.Errorf("restored model missing %q: %s", want, input)
						}
					}
					if rejected && !strings.Contains(input, "Operation rejected") {
						t.Error("restored model lost rejection")
					}
				}
				if requests >= len(outputs) {
					t.Error("unexpected model request")
					http.Error(w, "unexpected", 400)
					return
				}
				out := outputs[requests]
				requests++
				writeObservationResponse(w, out)
			}))
			defer server.Close()
			cfg := &config.AgentConfig{ExplorationBudget: config.DefaultExplorationBudget(), AllowTools: map[config.McpToolName]struct{}{config.ReadFileMCPToolName: {}, config.ExecMCPToolName: {}, config.ApplyPatchMCPToolName: {}}, LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: config.JsonLLMOutputSchema}}
			cfg.ExplorationBudget.InitialTurns = 1
			process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			var saved []byte
			stopped := false
			outage := errors.New("checkpoint unavailable after saved admission")
			process.SetCheckpoint(func() error {
				if stopped {
					return outage
				}
				// 模拟存储已提交，但调用方在收到确认前失去连接。恢复只读取该持久副本。
				saved, _ = json.Marshal(durable)
				var progress struct {
					Exploration struct {
						ModificationLeft int
						Rejection        string
					}
				}
				_ = json.Unmarshal(durable.ExecutionProgress, &progress)
				if progress.Exploration.ModificationLeft == remaining && ((rejected && strings.Contains(progress.Exploration.Rejection, "Operation rejected")) || (!rejected && len(durable.ToolExecutions) == 2 && durable.ToolExecutions[1].Status == "success")) {
					stopped = true
					return outage
				}
				return nil
			})
			_, _, err = process.Run(cfg, durable)
			if !errors.Is(err, outage) || !stopped {
				t.Fatalf("err=%v stopped=%t", err, stopped)
			}
			if err = json.Unmarshal(saved, &durable); err != nil {
				t.Fatal(err)
			}
			resumed = true
			requests = 0
			outputs = [][]any{modificationTool("patch", "apply_patch", map[string]any{"patch": "--- main.go\n+++ main.go\n@@ -1 +1 @@\n-package old\n+package main\n"}), modificationTool("verify", "exec", map[string]any{"command": "go", "args": []string{"test", "main.go"}}), {observationMessage(`{"status":"completed","answer":"changed and verified"}`)}}
			cfg.ExplorationBudget.InitialTurns = 100
			cfg.ExplorationBudget.ModificationWindowTurns = 100
			process, err = NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(previous)
			result, _, err := process.Run(cfg, durable)
			if err != nil || result.Status != CompletedReactStatus {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != "package main\n" {
				t.Fatalf("content=%q err=%v", content, err)
			}
			expected := 4
			if rejected {
				expected = 3
			}
			if len(durable.ToolExecutions) != expected {
				t.Fatalf("tools replayed or rejection executed: %+v", durable.ToolExecutions)
			}
			for _, call := range durable.ToolExecutions {
				if call.Status != "success" {
					t.Fatalf("unsuccessful tool: %+v", call)
				}
			}
			var progress struct {
				Exploration struct {
					Used, Extensions      int
					Policy                config.ExplorationBudget
					ModificationConfirmed bool
				}
			}
			_ = json.Unmarshal(durable.ExecutionProgress, &progress)
			if progress.Exploration.Used != 1 || progress.Exploration.Extensions != 0 || progress.Exploration.Policy.InitialTurns != 1 || !progress.Exploration.ModificationConfirmed {
				t.Fatalf("restored budget/evidence: %s", durable.ExecutionProgress)
			}
			if rejected && !strings.Contains(logs.String(), `rejection="Operation rejected`) {
				t.Errorf("restore log lost rejection context: %s", logs.String())
			}
		})
	}
}
