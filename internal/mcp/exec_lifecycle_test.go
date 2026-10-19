package mcp_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	mcp "github.com/collie-agent/codex-go/internal/mcp"
)

func newLifecycleExecutor(t *testing.T) *mcp.CommandExecutor {
	t.Helper()
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Skipf("command sandbox unavailable: %v", err)
	}
	return executor
}

func TestExecutePreservesBothStreamsAndBoundsCombinedOutput(t *testing.T) {
	executor := newLifecycleExecutor(t)
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import os,threading
def write(fd, data): os.write(fd, data)
t1=threading.Thread(target=write,args=(1,b'o'*700000)); t2=threading.Thread(target=write,args=(2,b'e'*700000))
t1.start(); t2.start(); t1.join(); t2.join()`})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.ExitCode != 0 || len(result.Stdout)+len(result.Stderr) != 1<<20 || !result.Truncated {
		t.Fatalf("unexpected result: exit=%d stdout=%d stderr=%d truncated=%v", result.ExitCode, len(result.Stdout), len(result.Stderr), result.Truncated)
	}
}

func TestExecuteDoesNotMarkExactlyOneMiBAsTruncated(t *testing.T) {
	executor := newLifecycleExecutor(t)
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import os
os.write(1, b'x' * (1 << 20))`})
	if err != nil || result.ExitCode != 0 || len(result.Stdout) != 1<<20 || result.Stderr != "" || result.Truncated {
		t.Fatalf("unexpected exact-limit result: result=%+v err=%v", result, err)
	}
}

func TestExecuteReturnsCapturedOutputForNonzeroCommand(t *testing.T) {
	executor := newLifecycleExecutor(t)
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-c", `import sys
print('out')
print('err', file=sys.stderr)
sys.exit(23)`})
	if err != nil {
		t.Fatalf("ordinary command failure returned executor error: %v", err)
	}
	if result.ExitCode != 23 || result.Stdout != "out\n" || result.Stderr != "err\n" || result.Truncated {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestExecuteTimeoutReturnsOutputAndPreventsLaterWorkspaceWrite(t *testing.T) {
	executor := newLifecycleExecutor(t)
	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	result, err := executor.Execute(ctx, workspace, "python3", []string{"-u", "-c", `import os,pathlib,time
print('before-timeout')
if os.fork()==0:
 time.sleep(300/1000)
 pathlib.Path('late').write_text('must not survive')
 os._exit(0)
time.sleep(20)`})
	if !errors.Is(err, context.DeadlineExceeded) || result.Stdout != "before-timeout\n" {
		t.Fatalf("unexpected timeout result: result=%+v err=%v", result, err)
	}
	result, err = executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import pathlib,time
time.sleep(.6)
assert not pathlib.Path('late').exists(), 'timed out process survived'`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("timed out process survived: result=%+v err=%v", result, err)
	}
}

func TestExecuteCancellationReturnsOutputAndPreventsLaterWorkspaceWrite(t *testing.T) {
	executor := newLifecycleExecutor(t)
	workspace := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result mcp.CommandResult
		err    error
	}, 1)
	go func() {
		result, err := executor.Execute(ctx, workspace, "python3", []string{"-u", "-c", `import os,pathlib,time
print('before-cancel')
if os.fork()==0:
 time.sleep(300/1000)
 pathlib.Path('late').write_text('must not survive')
 os._exit(0)
time.sleep(20)`})
		resultCh <- struct {
			result mcp.CommandResult
			err    error
		}{result, err}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	called := <-resultCh
	if !errors.Is(called.err, context.Canceled) || called.result.Stdout != "before-cancel\n" {
		t.Fatalf("unexpected cancellation result: result=%+v err=%v", called.result, called.err)
	}
	result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", `import pathlib,time
time.sleep(.6)
assert not pathlib.Path('late').exists(), 'cancelled process survived'`})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("cancelled process survived: result=%+v err=%v", result, err)
	}
}

func TestExecuteClassifiesSandboxStartupFailure(t *testing.T) {
	runtimeDir := t.TempDir()
	executor, err := mcp.NewCommandExecutor(runtimeDir)
	if err != nil {
		t.Skipf("command sandbox unavailable: %v", err)
	}
	if err := os.RemoveAll(runtimeDir); err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", nil)
	if !errors.Is(err, mcp.ErrSandboxStartup) || result.ExitCode == 0 {
		t.Fatalf("expected classified sandbox startup failure: result=%+v err=%v", result, err)
	}
}

func TestExecuteKeepsCallsIndependent(t *testing.T) {
	executor := newLifecycleExecutor(t)
	workspace := t.TempDir()
	for _, want := range []string{"first", "second"} {
		result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-c", "print('" + want + "')"})
		if err != nil || result.ExitCode != 0 || result.Stdout != want+"\n" || result.Stderr != "" || result.Truncated {
			t.Fatalf("unexpected %s result: result=%+v err=%v", want, result, err)
		}
	}
}

func TestExecuteUsesDefaultThirtySecondTimeout(t *testing.T) {
	executor := newLifecycleExecutor(t)
	started := time.Now()
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", []string{"-u", "-c", "print('started'); import time; time.sleep(35)"})
	if !errors.Is(err, context.DeadlineExceeded) || result.Stdout != "started\n" {
		t.Fatalf("unexpected default timeout result: result=%+v err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed < 29*time.Second || elapsed > 34*time.Second {
		t.Fatalf("default timeout fired at %s", elapsed)
	}
}

func TestExecuteReturnsWhenDescendantKeepsOutputPipeOpen(t *testing.T) {
	executor := newLifecycleExecutor(t)
	workspace := t.TempDir()
	started := time.Now()
	result, err := executor.Execute(context.Background(), workspace, "python3", []string{"-u", "-c", `import subprocess,time
subprocess.Popen(['python3', '-c', 'import time; time.sleep(20)'])
print('parent-done')`})
	if err != nil || result.ExitCode != 0 || result.Stdout != "parent-done\n" {
		t.Fatalf("unexpected result: result=%+v err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("descendant holding output pipe delayed Execute for %s", elapsed)
	}
}
