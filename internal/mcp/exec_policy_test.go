package mcp_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/mcp"
)

func TestCommandExecutorResolverPolicy(t *testing.T) {
	resolver, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"none", "host"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			executor, err := mcp.NewCommandExecutorWithConfig(mcp.Config{Workspace: workspace, ExecNetwork: mode})
			if err != nil {
				t.Fatal(err)
			}
			program := `import pathlib,sys
p=pathlib.Path('/etc/resolv.conf')
if sys.argv[1] == 'none':
 assert not p.exists()
else:
 assert p.is_file() and not p.is_symlink()
 assert p.read_bytes().hex() == sys.argv[2]
 try:
  with p.open('ab'):
   pass
 except OSError:
  pass
 else:
  raise AssertionError('resolver is writable')
print('ok')`
			result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", program, mode, fmt.Sprintf("%x", resolver)})
			if err != nil || result.ExitCode != 0 || result.Stdout != "ok\n" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

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

func TestCommandExecutorPersistentLanguageCaches(t *testing.T) {
	workspace, cache := t.TempDir(), t.TempDir()
	cfg := mcp.Config{Workspace: workspace, ExecGoProxy: "https://proxy.example.invalid", ExecCaches: []config.ExecCache{{Name: "packages", Directory: cache, Env: []string{"GOMODCACHE", "PIP_CACHE_DIR"}}}}
	for _, program := range []string{
		`import os,pathlib; p=pathlib.Path(os.environ['GOMODCACHE']); assert os.environ['PIP_CACHE_DIR']==str(p); assert os.environ['GOPROXY']=='https://proxy.example.invalid'; (p/'saved').write_text('persistent')`,
		`import os,pathlib; assert (pathlib.Path(os.environ['PIP_CACHE_DIR'])/'saved').read_text()=='persistent'`,
	} {
		executor, err := mcp.NewCommandExecutorWithConfig(cfg)
		if err != nil {
			t.Fatal(err)
		}
		result, err := executor.Execute(t.Context(), workspace, "python3", []string{"-c", program})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func TestCommandExecutorRejectsCacheWorkspaceOverlapOnExecution(t *testing.T) {
	workspace, cache := t.TempDir(), t.TempDir()
	executor, err := mcp.NewCommandExecutorWithConfig(mcp.Config{Workspace: workspace, ExecCaches: []config.ExecCache{{Name: "pip", Directory: cache, Env: []string{"PIP_CACHE_DIR"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(t.Context(), cache, "python3", []string{"-c", "print('unexpected')"}); err == nil {
		t.Fatal("accepted persistent cache as workspace")
	}
}

func TestHostNetworkCanVerifyHTTPSUsingSystemCertificates(t *testing.T) {
	workspace := t.TempDir()
	executor, err := mcp.NewCommandExecutorWithConfig(mcp.Config{Workspace: workspace, ExecNetwork: "host"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(t.Context(), workspace, "python3", []string{"-c", `import ssl; assert len(ssl.create_default_context().get_ca_certs()) > 0, 'system trust roots missing'`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("certificate verification unavailable: %+v err=%v", result, err)
	}
}

func TestCommandExecutorRejectsWritableDiscoveredRuntimeCache(t *testing.T) {
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "cache-launcher"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cache+":"+os.Getenv("PATH"))
	_, err := mcp.NewCommandExecutorWithConfig(mcp.Config{Workspace: t.TempDir(), DiscoverHostCommands: true, ExecCommands: []string{"cache-launcher"}, ExecCaches: []config.ExecCache{{Name: "packages", Directory: cache, Env: []string{"PIP_CACHE_DIR"}}}})
	if err == nil {
		t.Fatal("accepted writable cache overlapping discovered runtime")
	}
}
