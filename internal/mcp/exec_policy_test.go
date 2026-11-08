package mcp_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/mcp"
)

func TestCommandExecutorNetworkPolicy(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			fmt.Fprint(conn, "mysql-ready")
			conn.Close()
		}
	}()
	for _, mode := range []string{"", "none", "host"} {
		t.Run("network="+mode, func(t *testing.T) {
			workspace := t.TempDir()
			executor, err := mcp.NewCommandExecutorWithConfig(mcp.Config{Workspace: workspace, ExecNetwork: mode})
			if err != nil {
				t.Fatal(err)
			}
			program := `import socket,sys
try:
 s=socket.create_connection((sys.argv[1],int(sys.argv[2])),timeout=1)
 print(s.recv(100).decode()); s.close()
except OSError:
 print('isolated')`
			host, port, _ := net.SplitHostPort(listener.Addr().String())
			result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", program, host, port})
			want := "isolated\n"
			if mode == "host" {
				want = "mysql-ready\n"
			}
			if err != nil || result.ExitCode != 0 || result.Stdout != want {
				t.Fatalf("result=%+v err=%v want=%q", result, err, want)
			}
		})
	}
}

func TestCommandExecutorForwardsOnlyAuthorizedEnvironmentSnapshot(t *testing.T) {
	t.Setenv("CODEX_TDD_ALLOWED", "configured-value")
	t.Setenv("CODEX_TDD_PRIVATE", "private-value")
	workspace := t.TempDir()
	executor, err := mcp.NewCommandExecutorWithConfig(mcp.Config{Workspace: workspace, ExecEnv: []string{"CODEX_TDD_ALLOWED"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TDD_ALLOWED", "changed-after-initialization")
	result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import os
assert os.environ['CODEX_TDD_ALLOWED'] == 'configured-value'
assert 'CODEX_TDD_PRIVATE' not in os.environ
assert os.environ['HOME'] == '/run/home'
print('authorized')`})
	if err != nil || result.ExitCode != 0 || result.Stdout != "authorized\n" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
