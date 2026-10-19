package mcp_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestCommandExecutorRejectsUnapprovedEntrypoints(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sh", "/usr/bin/python3", "./git", "GIT", ""} {
		if _, err := executor.Execute(context.Background(), t.TempDir(), name, nil); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestCommandExecutorWorkspaceReadWrite(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import pathlib,sys
p=pathlib.Path('example.txt'); p.write_text('workspace data'); print(p.read_text()); print('diagnostic', file=sys.stderr); assert sys.stdin.read()==''; sys.exit(7)`})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "workspace data\n" || result.Stderr != "diagnostic\n" || result.ExitCode != 7 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestCommandExecutorTreatsCommandStderrAsOutput(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import sys; sys.stderr.write('bwrap: command diagnostic\n'); sys.exit(1)`})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 1 || result.Stderr != "bwrap: command diagnostic\n" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestCommandExecutorRejectsSystemWorkspaces(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"/", "/usr", "/usr/bin", "/etc", "/proc", "/dev", "/sys", "/run"} {
		t.Run(root, func(t *testing.T) {
			if _, err := executor.Execute(context.Background(), root, "python3", []string{"-c", "print('must not run')"}); err == nil {
				t.Fatalf("accepted system workspace %s", root)
			}
		})
	}
}

func TestCommandExecutorIsolatesHostResources(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("host secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(workspace, "outside")); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("SANDBOX_SECRET", "host token")
	t.Setenv("SSH_AUTH_SOCK", secret)
	t.Setenv("HTTPS_PROXY", "http://host.invalid")
	t.Setenv("PATH", workspace)
	script := `import os,pathlib,socket,sys
assert pathlib.Path.cwd() == pathlib.Path(sys.argv[1])
for p in [sys.argv[2], 'outside']:
 try: pathlib.Path(p).read_text()
 except (FileNotFoundError, PermissionError): pass
 else: raise AssertionError('host secret visible')
for key in ['SANDBOX_SECRET', 'SSH_AUTH_SOCK', 'HTTPS_PROXY']:
 assert key not in os.environ, key
assert os.environ['PATH'] == '/usr/bin:/bin'
assert pathlib.Path(os.environ['HOME']).is_dir()
assert not list(pathlib.Path(os.environ['HOME']).iterdir())
for fd in range(3, 32):
 try: os.fstat(fd)
 except OSError: pass
 else: raise AssertionError('inherited fd '+str(fd))
for address in [('127.0.0.1', int(sys.argv[3])), ('1.1.1.1', 53)]:
 s=socket.socket(); s.settimeout(0.2)
 try: s.connect(address)
 except OSError: pass
 else: raise AssertionError('network available')
 finally: s.close()
try: pathlib.Path('/usr/sandbox-write-probe').write_text('bad')
except OSError: pass
else: raise AssertionError('writable runtime')
print('isolated')`
	port := strings.Split(listener.Addr().String(), ":")[1]
	result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", script, workspace, secret, port})
	if err != nil || result.ExitCode != 0 || result.Stdout != "isolated\n" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommandExecutorCleansBackgroundProcesses(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import os,time,pathlib
if os.fork()==0:
 os.setsid(); time.sleep(0.2); pathlib.Path('escaped').write_text('alive'); os._exit(0)
print('parent exited')`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import pathlib,time; time.sleep(0.4); assert not pathlib.Path('escaped').exists(); print('clean')`})
	if err != nil || result.ExitCode != 0 || result.Stdout != "clean\n" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommandExecutorCancellationCleansProcessTree(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result, err := executor.Execute(ctx, workspace, "python3", []string{"-u", "-c", `import os,time,pathlib
print('started')
if os.fork()==0:
 os.setsid(); time.sleep(0.5); pathlib.Path('escaped').write_text('alive'); os._exit(0)
time.sleep(20)`})
	if !errors.Is(err, context.DeadlineExceeded) || result.Stdout != "started\n" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import pathlib,time; time.sleep(0.6); assert not pathlib.Path('escaped').exists()`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommandExecutorBoundsCapturedOutput(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import os; os.write(1,b'x'*1048576); os.write(2,b'y'*1048576)`})
	if err != nil || result.ExitCode != 0 || !result.Truncated || len(result.Stdout)+len(result.Stderr) != 1<<20 {
		t.Fatalf("stdout=%d stderr=%d truncated=%v exit=%d err=%v", len(result.Stdout), len(result.Stderr), result.Truncated, result.ExitCode, err)
	}
}

func TestCommandExecutorDoesNotResolveWorkspaceCommands(t *testing.T) {
	for _, dir := range []string{"/usr/bin", "/bin"} {
		if _, err := os.Stat(filepath.Join(dir, "tsx")); err == nil {
			t.Skip("tsx is installed in system PATH")
		}
	}
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "tsx"), []byte("#!/bin/sh\nprintf hijacked"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", workspace)
	result, err := executor.Execute(context.Background(), workspace, "tsx", nil)
	if err == nil || !strings.Contains(err.Error(), "command unavailable in controlled PATH") || result.Stdout != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommandExecutorNormalizesWorkspaceSymlinks(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "runtime")
	if err := os.Symlink("/usr", link); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), link, "python3", []string{"-c", "print('must not run')"}); err == nil {
		t.Fatal("accepted symlink to runtime")
	}
	workspace := t.TempDir()
	link = filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), link, "python3", []string{"-c", `import pathlib,sys; assert str(pathlib.Path.cwd()) == sys.argv[1]`, workspace})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommandExecutorSupportsExplicitCancellation(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := executor.Execute(ctx, t.TempDir(), "python3", []string{"-c", `print('must not run')`})
	if !errors.Is(err, context.Canceled) || result.Stdout != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommandExecutorAllowsGitMetadataAndFileDeletion(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	result, err := executor.Execute(context.Background(), workspace, "git", []string{"init", "--quiet"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import pathlib,subprocess
p=pathlib.Path('discard'); p.write_text('one'); p.write_text('two'); p.unlink()
assert not p.exists()
subprocess.run(['git','config','example.key','workspace'],check=True)
assert subprocess.check_output(['git','config','example.key']) == b'workspace\n'
assert pathlib.Path('.git/config').is_file()`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
