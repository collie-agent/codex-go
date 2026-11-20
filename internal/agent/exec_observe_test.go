package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactExecObservationSwitch(t *testing.T) {
	for _, tc := range []struct {
		name, setting, tool, arguments, output string
		wantObservation                        bool
	}{
		{"default off", "", "exec", `{"command":"python3","args":["-c","print('exec-output-marker')"]}`, "exec-output-marker", false},
		{"explicitly off", "observe: false\n", "exec", `{"command":"python3","args":["-c","print('exec-output-marker')"]}`, "exec-output-marker", false},
		{"enabled", "observe: true\n", "exec", `{"command":"rg","args":["--version"]}`, "ripgrep", true},
		{"failed exec", "observe: true\n", "exec", `{"command":"python3","args":["-c","raise SystemExit(7)"]}`, `\"exit_code\":7`, true},
		{"invalid exec arguments", "observe: true\n", "exec", `{"command":"git","args":"status"}`, "tool was not executed", false},
		{"patch", "observe: true\n", "apply_patch", `{"patch":"--- /dev/null\n+++ output.txt\n@@ -0,0 +1 @@\n+patched\n"}`, "Applied patch to: output.txt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testReactExecObservationSwitch(t, tc.setting, tc.tool, tc.arguments, tc.output, tc.wantObservation)
		})
	}
}

func testReactExecObservationSwitch(t *testing.T, setting, tool, arguments, output string, wantObservation bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed := make(chan string, 1)
	releaseObservation := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Model string
			Input []struct {
				Type, Output, Content string
				CallID                string `json:"call_id"`
			}
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Model == "observer" {
			var input strings.Builder
			for _, item := range body.Input {
				input.WriteString(item.Content)
			}
			observed <- input.String()
			if wantObservation {
				// Hold the observer response until the next ReAct request arrives.
				select {
				case <-releaseObservation:
				case <-ctx.Done():
					return
				}
			}
			writeObservationResponse(w, []any{observationMessage(`{"ConfirmedFacts":[],"ReportedErrors":[]}`)})
			return
		}
		for _, item := range body.Input {
			if item.Type == "function_call_output" && item.CallID == "tool_check" {
				if !strings.Contains(item.Output, output) {
					t.Errorf("ReAct tool output = %s", item.Output)
				}
				if wantObservation {
					select {
					case input := <-observed:
						if !strings.Contains(input, "tool_check") || !strings.Contains(input, output) {
							t.Errorf("observation input = %s", input)
						}
						close(releaseObservation)
					case <-ctx.Done():
						t.Error("observer did not receive the completed exec result")
						return
					}
				} else {
					select {
					case <-observed:
						t.Error("tool triggered observation unexpectedly")
					case <-time.After(100 * time.Millisecond):
					}
				}
				writeObservationResponse(w, []any{observationMessage(`{"status":"completed","answer":"done"}`)})
				return
			}
		}
		writeObservationResponse(w, []any{map[string]any{
			"type": "function_call", "id": "fc_tool", "call_id": "tool_check", "name": tool,
			"arguments": arguments,
		}})
	}))
	defer server.Close()
	cfg := loadExecObservationConfig(t, server.URL, setting)
	process, err := NewReactProcess[ReactResult](ctx, filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	durable := &state.DurableState{RunId: "exec-observe", Goal: "run a check", CompletionCriteria: []string{"checked"}}
	if got, _, err := process.Run(cfg, durable); err != nil || got.Answer != "done" {
		t.Fatalf("Run() = %+v, %v", got, err)
	}
}

func loadExecObservationConfig(t *testing.T, url, observeSetting string) *config.AgentConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	data := fmt.Sprintf(`name: react
%sbase:
  allow_tools: [exec, apply_patch]
  llm:
    model: react
    api_key: test
    base_url: %s
agents:
  observe:
    llm:
      model: observer
      api_key: test
      base_url: %s
`, observeSetting, url, url)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
