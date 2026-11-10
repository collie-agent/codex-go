package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

const sessionHTTPServer = `import http.server,signal,sys
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  self.send_response(200); self.end_headers(); self.wfile.write(b'ok')
 def log_message(self,*args): pass
server=http.server.HTTPServer(('127.0.0.1',0),Handler)
def stop(*args):
 server.server_close(); print('shutdown'); sys.exit(0)
signal.signal(signal.SIGTERM,stop)
print(server.server_port,flush=True)
server.serve_forever()`

// Read only the public model request: process handles and durable-state
// internals are deliberately not used to drive the simulated agent.
func sessionToolOutput(t *testing.T, items []map[string]any, id string) map[string]any {
	t.Helper()
	for _, item := range items {
		if item["type"] != "function_call_output" || item["call_id"] != id {
			continue
		}
		output, _ := item["output"].(string)
		var blocks []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(output), &blocks); err != nil {
			t.Errorf("tool output: %s: %v", output, err)
			return nil
		}
		if len(blocks) != 1 {
			t.Errorf("tool blocks=%v", blocks)
			return nil
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(blocks[0].Text), &out); err != nil {
			t.Errorf("tool result: %s: %v", blocks[0].Text, err)
			return nil
		}
		return out
	}
	t.Errorf("missing tool result %s", id)
	return nil
}

func TestReactCleansUpServerBeforeRunReturns(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex-mcp")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mcp").CombinedOutput(); err != nil {
		t.Fatalf("build MCP: %v\n%s", err, output)
	}
	for _, external := range []bool{false, true} {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("external=%v/interrupted=%v", external, interrupted), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				turn, id, port := 0, "", ""
				model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					var body struct {
						Input []map[string]any `json:"input"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					turn++
					if turn == 1 {
						// Make shutdown observably asynchronous, to verify Run waits for it.
						program := strings.Replace(sessionHTTPServer, "server.server_close();", "__import__('time').sleep(.25); server.server_close();", 1)
						sessionModelCall(w, "start", "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", program}})
						return
					}
					if turn == 2 {
						id, _ = sessionToolOutput(t, body.Input, "start")["session_id"].(string)
					} else {
						out := sessionToolOutput(t, body.Input, fmt.Sprintf("poll%d", turn-1))
						port = strings.TrimSpace(fmt.Sprint(out["stdout"]))
						if port != "" {
							conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
							if err != nil {
								t.Errorf("server never became reachable: %v", err)
							} else {
								_ = conn.Close()
							}
							if interrupted {
								cancel()
								return
							}
							writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
							return
						}
					}
					sessionModelCall(w, fmt.Sprintf("poll%d", turn), "exec_poll", map[string]any{"session_id": id, "wait_ms": 1000})
				}))
				defer model.Close()
				cfg := &config.AgentConfig{Name: "react", LogLevel: config.ErrorLogLevel, LLMConfig: &config.LLMConfig{BaseUrl: model.URL, ApiKey: "test", Model: "test", OutputSchema: config.LineProtocolLLMOutputSchema}, AllowTools: map[config.McpToolName]struct{}{"exec_start": {}, "exec_poll": {}}}
				var process *ReactProcess[ReactResult]
				var err error
				workspace := t.TempDir()
				if external {
					process, err = NewReactProcessWithCommand[ReactResult](ctx, exec.CommandContext(ctx, binary, "--workspace", workspace, "--exec-network", "host", "--log-dir", t.TempDir()))
				} else {
					process, err = NewReactProcess[ReactResult](ctx, filemcp.Config{Workspace: workspace, ExecNetwork: "host"})
				}
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = process.Run(cfg, &state.DurableState{RunId: "cleanup", Goal: "start server", CompletionCriteria: []string{"ready"}})
				if !interrupted && err != nil {
					t.Fatal(err)
				}
				if interrupted && err == nil {
					t.Fatal("cancellation was lost")
				}
				if port == "" {
					t.Fatal("server not started")
				}
				if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 100*time.Millisecond); err == nil {
					_ = conn.Close()
					t.Fatal("Run returned before server shutdown")
				}
			})
		}
	}

}

func sessionModelCall(w http.ResponseWriter, id, name string, args map[string]any) {
	data, _ := json.Marshal(args)
	writeObservationResponse(w, []any{map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": string(data)}})
}

func TestReactCanPollRepeatedlyVerifyHTTPAndStopServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	turn := 0
	id := ""
	port := ""
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		instructions, _ := body.Input[0]["content"].(string)
		if strings.Contains(instructions, "工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
			return
		}
		turn++
		switch turn {
		case 1:
			sessionModelCall(w, "start", "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", sessionHTTPServer}})
		case 2:
			out := sessionToolOutput(t, body.Input, "start")
			id, _ = out["session_id"].(string)
			sessionModelCall(w, "poll1", "exec_poll", map[string]any{"session_id": id, "wait_ms": 1000})
		case 3:
			sessionModelCall(w, "poll2", "exec_poll", map[string]any{"session_id": id, "wait_ms": 1000})
		case 4:
			out := sessionToolOutput(t, body.Input, "poll2")
			port = strings.TrimSpace(fmt.Sprint(out["stdout"]))
			if _, err := strconv.Atoi(port); err != nil {
				t.Errorf("server readiness output=%v", out)
				writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"failed\"\nANSWER=\"poll failed\"\nEND RESULT")})
				return
			}
			sessionModelCall(w, "verify", "exec", map[string]any{"command": "python3", "args": []string{"-c", "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:" + port + "',timeout=2).read().decode())"}})
		case 5:
			out := sessionToolOutput(t, body.Input, "verify")
			if out["stdout"] != "ok\n" {
				t.Errorf("verification=%v", out)
			}
			sessionModelCall(w, "stop", "exec_stop", map[string]any{"session_id": id})
		case 6:
			out := sessionToolOutput(t, body.Input, "stop")
			if out["state"] != "exited" || !strings.Contains(fmt.Sprint(out["stdout"]), "shutdown") {
				t.Errorf("stop=%v", out)
			}
			sessionModelCall(w, "restart", "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", sessionHTTPServer}})
		case 7:
			out := sessionToolOutput(t, body.Input, "restart")
			restarted, _ := out["session_id"].(string)
			if restarted == "" || restarted == id {
				t.Errorf("restart did not create new session: %v", out)
			}
			sessionModelCall(w, "stop_restart", "exec_stop", map[string]any{"session_id": restarted})
		default:
			out := sessionToolOutput(t, body.Input, "stop_restart")
			if out["state"] != "exited" {
				t.Errorf("restarted process not stopped: %v", out)
			}
			writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"verified and stopped\"\nEND RESULT")})
		}
	}))
	defer model.Close()
	// Load the same public YAML tool permissions used by real agent runs.
	configPath := filepath.Join(t.TempDir(), "agent.yaml")
	yaml := "name: react\nlog_level: error\nbase:\n  allow_tools: [exec, exec_start, exec_poll, exec_stop]\n  llm:\n    api_key: test\n    model: test\n    output_schema: line_protocol_llm_output\n    base_url: " + model.URL + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadAgentConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	process, err := NewReactProcess[ReactResult](ctx, filemcp.Config{Workspace: t.TempDir(), ExecNetwork: "host"})
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := process.Run(cfg, &state.DurableState{RunId: "server", Goal: "verify server", CompletionCriteria: []string{"HTTP returns ok and server exits"}})
	if err != nil || result.Answer != "verified and stopped" {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestReactCanRepeatExecAfterProcessLifecycleChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	turn, id := 0, ""
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if instructions, _ := body.Input[0]["content"].(string); strings.Contains(instructions, "工具结果观察器") {
			writeObservationResponse(w, []any{observationMessage("BEGIN OBSERVATION\nEND OBSERVATION")})
			return
		}
		turn++
		switch turn {
		case 2:
			sessionModelCall(w, "start", "exec_start", map[string]any{"command": "python3", "args": []string{"-c", "import time; time.sleep(30)"}})
		case 3:
			id, _ = sessionToolOutput(t, body.Input, "start")["session_id"].(string)
			sessionModelCall(w, "check2", "exec", map[string]any{"command": "python3", "args": []string{"-c", "print('checked')"}})
		case 4:
			sessionModelCall(w, "stop", "exec_stop", map[string]any{"session_id": id})
		case 6:
			writeObservationResponse(w, []any{observationMessage("BEGIN RESULT\nSTATUS=\"completed\"\nANSWER=\"done\"\nEND RESULT")})
		default:
			sessionModelCall(w, fmt.Sprintf("check%d", turn), "exec", map[string]any{"command": "python3", "args": []string{"-c", "print('checked')"}})
		}
	}))
	defer model.Close()
	process, err := NewReactProcess[ReactResult](ctx, filemcp.Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.AgentConfig{LogLevel: config.ErrorLogLevel, AllowTools: map[config.McpToolName]struct{}{"exec": {}, "exec_start": {}, "exec_stop": {}}, LLMConfig: &config.LLMConfig{BaseUrl: model.URL, ApiKey: "test", Model: "test", OutputSchema: config.LineProtocolLLMOutputSchema}}
	durable := &state.DurableState{RunId: "lifecycle", Goal: "check process lifecycle", CompletionCriteria: []string{"checked"}}
	_, _, err = process.Run(cfg, durable)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	for _, call := range durable.ToolExecutions {
		if call.ToolName == "exec" {
			checks++
			if call.Status != filemcp.ToolCallSuceessStatus {
				t.Errorf("check failed: %v", call)
			}
		}
	}
	if checks != 3 {
		t.Fatalf("actual checks=%d, want 3", checks)
	}
}
