package agent

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestReactLogLevels(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, level := range []string{"", "info", "error"} {
			t.Run(string(schema)+"/level="+level, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					calls++
					if calls == 1 {
						writeObservationResponse(w, []any{
							observationMessage("Inspecting the workspace."),
							map[string]any{"type": "function_call", "id": "fc_list", "call_id": "call_list", "name": "list_dir", "arguments": `{"path":".","depth":1,"offset":0,"limit":20}`},
						})
						return
					}
					var request struct{ Input json.RawMessage }
					if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if !strings.Contains(string(request.Input), "function_call_output") {
						t.Error("tool result missing from the next model request")
					}
					text := `{"status":"completed","answer":"done","Observation":[],"CompletionEvidence":[]}`
					if schema == config.LineProtocolLLMOutputSchema {
						text = "BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT"
					}
					writeObservationResponse(w, []any{observationMessage(text)})
				}))
				defer server.Close()
				dir := t.TempDir()
				cfgPath := filepath.Join(dir, "agent.yaml")
				yaml := "name: react\nbase:\n  allow_tools: [list_dir]\n  llm:\n    api_key: test\n    model: test\n    base_url: " + server.URL + "\n    output_schema: " + string(schema) + "\n"
				if level != "" {
					yaml += "log_level: " + level + "\n"
				}
				if err := os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.LoadAgentConfig(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				process, err := NewReactProcess[ReactResult](t.Context(), filemcp.Config{Workspace: dir})
				if err != nil {
					t.Fatal(err)
				}
				// 同时捕获标准日志和直接写入 stderr 的日志，验证真实输出路径。
				output, err := os.CreateTemp(t.TempDir(), "react-log")
				if err != nil {
					t.Fatal(err)
				}
				defer output.Close()
				stderr, writer := os.Stderr, log.Writer()
				os.Stderr = output
				log.SetOutput(output)
				defer func() { os.Stderr = stderr; log.SetOutput(writer) }()
				got, _, err := process.Run(cfg.ForAgent("react"), &fileContext.DurableState{CompletionCriteria: []string{"original task verified"}, RunId: "log-test", Goal: "list files"})
				if err != nil || got.Answer != "done" || calls != 2 {
					t.Fatalf("Run() = %+v, %v; model calls = %d", got, err, calls)
				}
				data, err := os.ReadFile(output.Name())
				if err != nil {
					t.Fatal(err)
				}
				logs := string(data)
				if got := strings.Count(logs, "LLM response text="); got != 2 {
					t.Errorf("response text logs = %d, want 2", got)
				}
				for _, detail := range []string{"[react] LLM context", "[react] tool calling result:", "assistant output="} {
					if strings.Contains(logs, detail) != (level != "error") {
						t.Errorf("level %q: unexpected visibility of %q in logs:\n%s", level, detail, logs)
					}
				}
				if strings.Contains(logs, "JSON schema=") != (schema == config.JsonLLMOutputSchema && level != "error") {
					t.Errorf("unexpected JSON schema visibility: %s", logs)
				}
				for _, text := range []string{`tool calling name=list_dir call_id=call_list arguments={"path":".","depth":1,"offset":0,"limit":20}`, "LLM response text=Inspecting the workspace.", "LLM response text=", "done"} {
					if !strings.Contains(logs, text) {
						t.Errorf("missing log %q: %s", text, logs)
					}
				}
			})
		}
	}
}
