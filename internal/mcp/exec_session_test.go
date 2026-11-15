package mcp

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	hostconfig "github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise the MCP contract rather than the session manager's internal state.
func callSessionTool(t *testing.T, client *sdk.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	result := fileToolCall(t, client, name, args)
	if result.IsError {
		t.Fatalf("%s: structured=%+v content=%v", name, result.StructuredContent, result.Content)
	}
	data, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExecSessionCannotBePolledOrStoppedByAnotherConnection(t *testing.T) {
	server := sdk.NewServer(&sdk.Implementation{Name: "session-isolation"}, nil)
	cleanup, err := RegisterToolsWithCleanup(server, Config{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	connect := func() *sdk.ClientSession {
		ct, st := sdk.NewInMemoryTransports()
		go func() { _ = server.Run(t.Context(), st) }()
		client, err := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil).Connect(t.Context(), ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return client
	}
	owner, other := connect(), connect()
	out := callSessionTool(t, owner, "exec_start", map[string]any{"command": "python3", "args": []string{"-c", "import time; time.sleep(60)"}})
	for _, name := range []string{"exec_poll", "exec_stop"} {
		if result := fileToolCall(t, other, name, map[string]any{"session_id": out["session_id"]}); !result.IsError {
			t.Fatalf("foreign connection could call %s", name)
		}
	}
	if out := callSessionTool(t, owner, "exec_poll", map[string]any{"session_id": out["session_id"]}); out["state"] != "running" {
		t.Fatalf("foreign stop killed owner process: %v", out)
	}
}

func TestExecSessionForceStopReapsDetachedServer(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir(), ExecNetwork: "host"})
	program := `import signal,subprocess,sys,time
signal.signal(signal.SIGTERM,signal.SIG_IGN)
child="import http.server,signal; signal.signal(signal.SIGTERM,signal.SIG_IGN); s=http.server.HTTPServer(('127.0.0.1',0),http.server.BaseHTTPRequestHandler); print(s.server_port,flush=True); s.serve_forever()"
subprocess.Popen([sys.executable,'-u','-c',child],start_new_session=True)
time.sleep(60)`
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", program}})
	port := ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": started["session_id"], "wait_ms": 100})
		port = strings.TrimSpace(out["stdout"].(string))
		if port != "" {
			break
		}
	}
	if port == "" {
		t.Fatal("detached server not ready")
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := client.CallTool(ctx, &sdk.CallToolParams{Name: "exec_stop", Arguments: map[string]any{"session_id": started["session_id"]}})
	if err != nil || result.IsError {
		t.Fatalf("forced stop: %v %v", result, err)
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("detached server survived stop")
	}
}

func TestExecSessionGoRunServerStopsCompiledChild(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "server.go", `package main
import("fmt";"net")
func main(){
 listener,err:=net.Listen("tcp","127.0.0.1:0");if err!=nil{panic(err)}
 fmt.Println(listener.Addr().String())
 for { connection,err:=listener.Accept();if err!=nil{return};fmt.Fprint(connection,"ok");connection.Close() }
}`)
	client := execToolSession(t, Config{Workspace: workspace, ExecNetwork: "host", Timeout: 90 * time.Second})
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "go", "args": []string{"run", "server.go"}})
	address := ""
	for deadline := time.Now().Add(80 * time.Second); time.Now().Before(deadline); {
		out := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": started["session_id"], "wait_ms": 1000})
		if out["state"] == "exited" {
			t.Fatalf("Go server failed to start: %v", out)
		}
		address = strings.TrimSpace(out["stdout"].(string))
		if address != "" {
			break
		}
	}
	if address == "" {
		t.Fatal("Go server not ready")
	}
	verified := callSessionTool(t, client, "exec", map[string]any{"command": "python3", "args": []string{"-c", "import socket,sys; s=socket.create_connection((sys.argv[1],int(sys.argv[2])),timeout=2); print(s.recv(100).decode()); s.close()", strings.Split(address, ":")[0], strings.Split(address, ":")[1]}})
	if verified["stdout"] != "ok\n" || verified["exit_code"] != float64(0) {
		t.Fatalf("verification=%v", verified)
	}
	out := callSessionTool(t, client, "exec_stop", map[string]any{"session_id": started["session_id"]})
	if out["state"] != "exited" {
		t.Fatalf("Go server still running: %v", out)
	}
	if conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("go run left the compiled server running")
	}
}

func TestExecSessionBoundsRetainedLogsAndReportsTruncation(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir()})
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "python3", "args": []string{"-c", "import os; os.write(1,b'x'*200000); os.write(2,b'end')"}})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": started["session_id"], "wait_ms": 100})
		if out["state"] != "exited" {
			time.Sleep(time.Millisecond)
			continue
		}
		if out["truncated"] != true || out["next_cursor"] != float64(200003) || out["stdout"] != strings.Repeat("x", 65533) || out["stderr"] != "end" {
			t.Fatalf("truncated=%v cursor=%v stdout length=%d stderr=%q", out["truncated"], out["next_cursor"], len(out["stdout"].(string)), out["stderr"])
		}
		return
	}
	t.Fatal("session did not exit")
}

func TestExecSessionCanBePolledAndStoppedAfterStartReturns(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir()})
	started := callSessionTool(t, client, "exec_start", map[string]any{
		"command": "python3", "args": []string{"-u", "-c", "print('ready'); import time; time.sleep(60)"},
	})
	id, ok := started["session_id"].(string)
	if !ok || id == "" {
		t.Fatalf("missing session id: %v", started)
	}
	deadline := time.Now().Add(5 * time.Second)
	cursor := float64(0)
	output := ""
	for time.Now().Before(deadline) && output != "ready\n" {
		polled := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": id, "cursor": cursor, "wait_ms": 100})
		if polled["state"] != "running" {
			t.Fatalf("server exited: %v", polled)
		}
		output += polled["stdout"].(string)
		cursor = polled["next_cursor"].(float64)
	}
	if output != "ready\n" {
		t.Fatalf("output=%q", output)
	}
	stopped := callSessionTool(t, client, "exec_stop", map[string]any{"session_id": id, "cursor": cursor})
	if stopped["state"] != "exited" {
		t.Fatalf("stop did not wait for exit: %v", stopped)
	}
	if stopped["stdout"] != "" {
		t.Fatalf("already consumed logs repeated: %v", stopped)
	}
	if repeated := callSessionTool(t, client, "exec_stop", map[string]any{"session_id": id, "cursor": cursor}); repeated["state"] != "exited" {
		t.Fatalf("repeated stop: %v", repeated)
	}
}

func TestExecSessionPollReturnsNewOutputAndNaturalExit(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir()})
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", `import pathlib,time,sys
print('ready')
while not pathlib.Path('continue').exists(): time.sleep(.01)
print('finished'); print('diagnostic',file=sys.stderr); sys.exit(7)`}})
	id := started["session_id"]
	cursor := float64(0)
	ready := ""
	for deadline := time.Now().Add(5 * time.Second); ready != "ready\n" && time.Now().Before(deadline); {
		out := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": id, "cursor": cursor, "wait_ms": 100})
		ready += out["stdout"].(string)
		cursor = out["next_cursor"].(float64)
	}
	if ready != "ready\n" {
		t.Fatalf("ready=%q", ready)
	}
	callSessionTool(t, client, "exec", map[string]any{"command": "python3", "args": []string{"-c", "from pathlib import Path; Path('continue').touch()"}})
	stdout, stderr := "", ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": id, "cursor": cursor, "wait_ms": 100})
		stdout += out["stdout"].(string)
		stderr += out["stderr"].(string)
		cursor = out["next_cursor"].(float64)
		if out["state"] == "exited" {
			if stdout != "finished\n" || stderr != "diagnostic\n" || out["exit_code"] != float64(7) {
				t.Fatalf("stdout=%q stderr=%q out=%v", stdout, stderr, out)
			}
			return
		}
	}
	t.Fatal("session did not exit")
}

func TestExecSessionStopAllowsServerToShutDownGracefully(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir()})
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", `import signal,time,sys
def stop(*args):
 print('shutdown complete'); sys.exit(0)
signal.signal(signal.SIGTERM,stop)
print('ready')
time.sleep(60)`}})
	cursor := float64(0)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": started["session_id"], "cursor": cursor, "wait_ms": 100})
		cursor = out["next_cursor"].(float64)
		if out["stdout"] == "ready\n" {
			stopped := callSessionTool(t, client, "exec_stop", map[string]any{"session_id": started["session_id"], "cursor": cursor})
			if stopped["stdout"] != "shutdown complete\n" || stopped["exit_code"] != float64(0) {
				t.Fatalf("graceful shutdown failed: %v", stopped)
			}
			return
		}
	}
	t.Fatal("server not ready")
}

func TestExecSessionTimeoutRetainsDiagnosticsAndReportsFailure(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir(), Timeout: 200 * time.Millisecond})
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", "print('started'); import time; time.sleep(60)"}})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		result := fileToolCall(t, client, "exec_poll", map[string]any{"session_id": started["session_id"], "wait_ms": 100})
		var out map[string]any
		data, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		if out["state"] != "exited" {
			time.Sleep(time.Millisecond)
			continue
		}
		if !result.IsError || out["error_kind"] != "timeout" || out["stdout"] != "started\n" {
			t.Fatalf("isError=%v out=%v", result.IsError, out)
		}
		return
	}
	t.Fatal("session survived its timeout")
}

func TestExecSessionLimitsActiveProcessesAndReusesExitedSlots(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir()})
	args := map[string]any{"command": "python3", "args": []string{"-c", "import time; time.sleep(60)"}}
	var first string
	for i := 0; i < 32; i++ {
		out := callSessionTool(t, client, "exec_start", args)
		if i == 0 {
			first = out["session_id"].(string)
		}
	}
	if result := fileToolCall(t, client, "exec_start", args); !result.IsError {
		t.Fatal("accepted more than 32 active process sessions")
	}
	callSessionTool(t, client, "exec_stop", map[string]any{"session_id": first})
	callSessionTool(t, client, "exec_start", args)
}

func TestExecSessionInvalidStopDoesNotTerminateProcess(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir()})
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "python3", "args": []string{"-c", "import time; time.sleep(60)"}})
	result := fileToolCall(t, client, "exec_stop", map[string]any{"session_id": started["session_id"], "cursor": 999})
	if !result.IsError {
		t.Fatal("accepted cursor ahead of output")
	}
	out := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": started["session_id"]})
	if out["state"] != "running" {
		t.Fatalf("invalid stop killed process: %v", out)
	}
}

func TestExecSessionSharesPersistentCacheWithExec(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir(), ExecCaches: []hostconfig.ExecCache{{Name: "pip", Directory: t.TempDir(), Env: []string{"PIP_CACHE_DIR"}}}})
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "python3", "args": []string{"-c", `import os,pathlib; (pathlib.Path(os.environ['PIP_CACHE_DIR'])/'session').write_text('cached')`}})
	exited := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		out := callSessionTool(t, client, "exec_poll", map[string]any{"session_id": started["session_id"], "wait_ms": 100})
		if out["state"] == "exited" {
			if out["exit_code"] != float64(0) {
				t.Fatalf("session=%v", out)
			}
			exited = true
			break
		}
	}
	if !exited {
		t.Fatal("cache writer did not exit")
	}
	result := fileToolCall(t, client, "exec", map[string]any{"command": "python3", "args": []string{"-c", `import os,pathlib; print((pathlib.Path(os.environ['PIP_CACHE_DIR'])/'session').read_text())`}})
	out := decodeExecResult(t, result)
	if result.IsError || out.ExitCode != 0 || out.Stdout != "cached\n" {
		t.Fatalf("cache read=%+v", out)
	}
	callSessionTool(t, client, "exec_stop", map[string]any{"session_id": started["session_id"]})
}

func TestExecSessionContinuousOutputDoesNotExtendDeadline(t *testing.T) {
	client := execToolSession(t, Config{Workspace: t.TempDir(), Timeout: 200 * time.Millisecond})
	started := callSessionTool(t, client, "exec_start", map[string]any{"command": "python3", "args": []string{"-u", "-c", "import time\nwhile True: print('tick'); time.sleep(.01)"}})
	cursor := float64(0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		result := fileToolCall(t, client, "exec_poll", map[string]any{"session_id": started["session_id"], "cursor": cursor, "wait_ms": 100})
		var out map[string]any
		for _, content := range result.Content {
			if text, ok := content.(*sdk.TextContent); ok {
				if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
					t.Fatal(err)
				}
			}
		}
		if c, ok := out["next_cursor"].(float64); ok {
			cursor = c
		}
		if out["state"] == "exited" {
			if !result.IsError || out["error_kind"] != "timeout" || out["elapsed_ms"].(float64) < 200 || out["lifetime_ms"] != float64(200) {
				t.Fatalf("deadline evidence: %v error=%v", out, result.IsError)
			}
			return
		}
	}
	t.Fatal("continuous output extended absolute deadline")
}
