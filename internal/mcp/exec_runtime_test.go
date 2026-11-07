package mcp_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestCommandExecutorDoesNotRequireHostHome(t *testing.T) {
	for _, home := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		t.Run(home, func(t *testing.T) {
			t.Setenv("HOME", home)
			executor, err := mcp.NewCommandExecutor()
			if err != nil {
				t.Fatalf("executor depends on host HOME: %v", err)
			}
			result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import os,pathlib
assert os.environ['HOME'] == '/run/home'
assert pathlib.Path(os.environ['HOME']).is_dir()
print('isolated-home')`})
			if err != nil || result.ExitCode != 0 || result.Stdout != "isolated-home\n" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestCommandExecutorBlocksMountedHostSockets(t *testing.T) {
	runtimeDir := t.TempDir()
	executor, err := mcp.NewCommandExecutor(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	// Create sockets after initialization: validating directory contents once
	// would miss them. Cover both read-only runtime and writable Workspace.
	for _, dir := range []string{runtimeDir, workspace} {
		socketPath := filepath.Join(dir, "host.sock")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import errno,socket,subprocess,sys
script = '''import errno,socket,sys
for kind in [socket.SOCK_STREAM, socket.SOCK_DGRAM, socket.SOCK_SEQPACKET]:
 try:
  s = socket.socket(socket.AF_UNIX, kind)
  s.connect(sys.argv[1])
 except OSError as e:
  assert e.errno == errno.EPERM, e
 else:
  raise AssertionError('host socket accessible')
# Anonymous local IPC must remain usable.
a,b = socket.socketpair()
a.send(b'ok'); assert b.recv(2) == b'ok'
a.close(); b.close()
'''
exec(script)
subprocess.run([sys.executable, '-c', script, sys.argv[1]], check=True)
print('blocked')`, socketPath})
		if err != nil || result.ExitCode != 0 || result.Stdout != "blocked\n" {
			t.Fatalf("socket=%s result=%+v err=%v", socketPath, result, err)
		}
	}
}

func TestCommandExecutorUsesConfiguredRuntimeReadOnly(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir, err := os.MkdirTemp(home, ".codex-go-runtime-")
	if err != nil {
		t.Skipf("cannot create host runtime directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	workspace := t.TempDir()
	entrypoint := filepath.Join(runtimeDir, "poetry")
	if err := os.WriteFile(entrypoint, []byte("#!/bin/sh\nprintf runtime > \"$1\"\nprintf workspace > \"$2\"\nif printf denied > \"$3\"; then exit 41; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	executor, err := mcp.NewCommandExecutor(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	runtimeProbe := filepath.Join(runtimeDir, "probe")
	workspaceOutput := filepath.Join(workspace, "output")
	result, err := executor.Execute(context.Background(), workspace, "poetry", []string{runtimeProbe, workspaceOutput, runtimeProbe})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	check, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", "from pathlib import Path; assert Path('output').read_text() == 'workspace'; assert not Path(r'" + runtimeProbe + "').exists()"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("runtime/workspace verification result=%+v err=%v", check, err)
	}
}

func TestCommandExecutorUsesConfiguredRuntimeUnderTmp(t *testing.T) {
	runtimeDir, err := os.MkdirTemp("/tmp", "codex-go-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(runtimeDir, "poetry"), []byte("#!/bin/sh\nprintf tmp-runtime > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	executor, err := mcp.NewCommandExecutor(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(workspace, "output")
	result, err := executor.Execute(context.Background(), workspace, "poetry", []string{output})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	check, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", "assert open('output').read() == 'tmp-runtime'"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("workspace output verification result=%+v err=%v", check, err)
	}
}

func TestCommandExecutorCanonicalizesRelativeRuntimeDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir, err := os.MkdirTemp(home, ".codex-go-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	if err := os.WriteFile(filepath.Join(runtimeDir, "poetry"), []byte("#!/bin/sh\nprintf relative > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Dir(runtimeDir)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	executor, err := mcp.NewCommandExecutor(filepath.Base(runtimeDir))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	output := filepath.Join(workspace, "output")
	result, err := executor.Execute(context.Background(), workspace, "poetry", []string{output})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	check, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", "assert open('output').read() == 'relative'"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("workspace output verification result=%+v err=%v", check, err)
	}
}

func TestCommandExecutorRunsConfiguredNodeTool(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm is not installed")
	}
	npmDir, err := filepath.EvalSymlinks(filepath.Dir(npm))
	if err != nil {
		t.Fatal(err)
	}
	var executor *mcp.CommandExecutor
	if strings.HasPrefix(npmDir, "/usr/") || npmDir == "/bin" {
		executor, err = mcp.NewCommandExecutor()
	} else {
		executor, err = mcp.NewCommandExecutor(npmDir)
	}
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "package.json"), []byte(`{"scripts":{"build":"node -e \"require('fs').writeFileSync('artifact','node-ok')\""}}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), workspace, "npm", []string{"run", "build", "--offline"})
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	check, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", "assert open('artifact').read() == 'node-ok'"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("node artifact verification result=%+v err=%v", check, err)
	}
}

func TestCommandExecutorRunsWorkspaceTaskThroughConfiguredPythonLauncher(t *testing.T) {
	runtimeDir := t.TempDir()
	launcher := filepath.Join(runtimeDir, "poetry")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	executor, err := mcp.NewCommandExecutor(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	venvPython := filepath.Join(workspace, ".venv", "bin", "python")
	setup, err := executor.Execute(context.Background(), workspace, "python3", []string{"-m", "venv", "--without-pip", ".venv"})
	if err != nil || setup.ExitCode != 0 {
		t.Fatalf("virtual environment setup result=%+v err=%v", setup, err)
	}
	result, err := executor.Execute(context.Background(), workspace, "poetry", []string{venvPython, "-c", "open('artifact', 'w').write('ok')"})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	check, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", "assert open('artifact').read() == 'ok'"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("workspace artifact verification result=%+v err=%v", check, err)
	}
}

func TestNewCommandExecutorRejectsDangerousRuntimeDirectories(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dangerous := []string{"/", home, "/etc", "/proc", "/dev", "/sys", "/run", filepath.Join(t.TempDir(), "missing")}
	if parent := filepath.Dir(home); parent != "/" {
		dangerous = append(dangerous, parent)
	}
	for _, dir := range dangerous {
		t.Run(dir, func(t *testing.T) {
			if _, err := mcp.NewCommandExecutor(dir); err == nil {
				t.Fatalf("accepted runtime directory %q", dir)
			}
		})
	}
}

func TestNewCommandExecutorCanonicalizesRuntimeSymlink(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "poetry"), []byte("#!/bin/sh\nprintf symlink > \"$1\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "runtime-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	executor, err := mcp.NewCommandExecutor(link)
	if err != nil {
		t.Fatalf("rejected symlink to safe runtime directory: %v", err)
	}
	workspace := t.TempDir()
	result, err := executor.Execute(context.Background(), workspace, "poetry", []string{filepath.Join(workspace, "output")})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	check, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", "assert open('output').read() == 'symlink'"})
	if err != nil || check.ExitCode != 0 {
		t.Fatalf("symlink runtime verification result=%+v err=%v", check, err)
	}
}

func TestCommandExecutorRuntimeDirectoryMustBeDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(file, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := mcp.NewCommandExecutor(file)
	if err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("expected runtime validation error, got %v", err)
	}
}

// The default host policy exposes the complete build toolchain, not just go/bin.
func TestCommandExecutorDefaultGoRuntime(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), t.TempDir(), "go", []string{"version"})
	if err != nil || result.ExitCode != 0 || !strings.HasPrefix(result.Stdout, "go version ") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCommandExecutorUsesHostModuleCacheOffline(t *testing.T) {
	cache := t.TempDir()
	moduleDir := filepath.Join(cache, "example.com", "dependency@v1.0.0")
	if err := os.MkdirAll(moduleDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte("module example.com/dependency\n\ngo 1.20\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleDir, "dep.go"), []byte("package dependency\n"), 0644); err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(cache, "cache", "download", "example.com", "dependency", "@v")
	if err := os.MkdirAll(download, 0755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"v1.0.0.mod":     "module example.com/dependency\n\ngo 1.20\n",
		"v1.0.0.ziphash": "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	} {
		if err := os.WriteFile(filepath.Join(download, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOMODCACHE", cache)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.com/app\n\ngo 1.20\n\nrequire example.com/dependency v1.0.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), workspace, "go", []string{"list", "-mod=mod", "example.com/dependency"})
	if err != nil || result.ExitCode != 0 || result.Stdout != "example.com/dependency\n" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import os,pathlib
assert os.environ['GOPROXY'] == 'off'
p=pathlib.Path(os.environ['GOMODCACHE'])/'write-probe'
try: p.write_text('bad')
except PermissionError: pass
except OSError: pass
else: raise AssertionError('module cache writable')
`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
