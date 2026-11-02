package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// CommandExecutor executes approved entrypoints inside a fresh sandbox.
type CommandExecutor struct {
	bwrap   string
	path    []string
	mounts  []string
	runtime []string
}

// CommandResult retains at most 1 MiB of combined stdout and stderr.
type CommandResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// Error classes let callers distinguish rejected inputs from a sandbox that
// could not be started, while retaining the underlying diagnostic.
var (
	ErrCommandParameter = errors.New("invalid command parameter")
	ErrSandboxStartup   = errors.New("sandbox startup failed")
)

// Keep runtime validation and actual system mounts on the same directory list.
var commandSystemDirs = [...]string{"/usr", "/bin", "/sbin", "/lib", "/lib64"}

// NewCommandExecutor fixes the trusted system PATH without requiring every
// approved entrypoint to be installed. Isolation failures never fall back to host execution.
func NewCommandExecutor(runtimeDirs ...string) (*CommandExecutor, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("command sandbox requires Linux")
	}
	path, err := exec.LookPath("/usr/bin/bwrap")
	if err != nil {
		return nil, fmt.Errorf("bwrap unavailable: %w", err)
	}
	mounts := []string{}
	for _, dir := range commandSystemDirs {
		resolved, err := filepath.EvalSymlinks(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, resolved)
	}
	runtimes, err := validateRuntimeDirs(runtimeDirs, mounts)
	if err != nil {
		return nil, err
	}
	mounts = append(mounts, runtimes...)
	// PATH is fixed at initialization: it searches only trusted system
	// directories and host-authorized runtime directories, never Workspace.
	// Every runtime entry is mounted read-only but exposes its full contents.
	controlledPath := []string{"/usr/bin", "/bin"}
	controlledPath = append(controlledPath, runtimes...)
	return &CommandExecutor{bwrap: path, path: controlledPath, mounts: mounts, runtime: runtimes}, nil
}

// Execute runs at the canonical Workspace root with a 30-second deadline.
// Nonzero command exits are results; startup failures and interruption are errors.
// Captured output is retained on errors, and interruption wraps ctx.Err().
func (e *CommandExecutor) Execute(ctx context.Context, workspace, name string, args []string) (CommandResult, error) {
	// Only entrypoint names are accepted; paths must never bypass controlled resolution.
	switch name {
	case "find", "git", "grep", "npm", "poetry", "python", "python3", "rg", "sed", "ts-node", "tsx":
	default:
		return CommandResult{}, fmt.Errorf("%w: unapproved command %q", ErrCommandParameter, name)
	}
	root, err := resolveWorkspace(workspace)
	if err != nil {
		return CommandResult{}, fmt.Errorf("%w: invalid workspace: %v", ErrCommandParameter, err)
	}
	if root == "/" {
		return CommandResult{}, fmt.Errorf("%w: root filesystem cannot be workspace", ErrCommandParameter)
	}
	for _, dir := range append(append([]string{}, e.mounts...), "/etc", "/proc", "/dev", "/sys", "/run") {
		if containsDirectory(root, dir) || containsDirectory(dir, root) {
			return CommandResult{}, fmt.Errorf("%w: workspace overlaps system runtime %s", ErrCommandParameter, dir)
		}
	}
	// Resolve only from the immutable system PATH, never the caller's PATH or Workspace.
	executable := ""
	for _, dir := range e.path {
		candidate := filepath.Join(dir, name)
		info, statErr := os.Stat(candidate)
		if statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			if resolveErr != nil {
				continue
			}
			for _, mount := range e.mounts {
				if containsDirectory(mount, resolved) {
					executable = candidate
					break
				}
			}
			if executable != "" {
				break
			}
		}
	}
	if executable == "" {
		return CommandResult{}, fmt.Errorf("%w: command unavailable in controlled PATH: %s", ErrCommandParameter, name)
	}
	sandboxArgs := []string{"--unshare-user", "--unshare-pid", "--unshare-net", "--unshare-ipc", "--unshare-uts", "--cap-drop", "ALL", "--die-with-parent", "--new-session", "--hostname", "command-sandbox", "--clearenv"}
	// A fresh mount namespace exposes system runtimes read-only, never host HOME or /etc wholesale.
	for _, dir := range commandSystemDirs {
		if _, statErr := os.Stat(dir); statErr == nil {
			sandboxArgs = append(sandboxArgs, "--ro-bind", dir, dir)
		}
	}
	sandboxArgs = append(sandboxArgs, "--proc", "/proc", "--dev", "/dev", "--bind", "/tmp", "/tmp", "--tmpfs", "/run", "--dir", "/run/home", "--dir", "/run/cache", "--dir", "/etc")
	// Mount runtime directories last so they remain read-only even under /tmp.
	// Each host supplied directory remains read-only at its canonical path.
	for _, dir := range e.runtime {
		sandboxArgs = append(sandboxArgs, "--ro-bind", dir, dir)
	}
	for _, file := range []string{"/etc/ld.so.cache", "/etc/alternatives"} {
		if _, statErr := os.Stat(file); statErr == nil {
			sandboxArgs = append(sandboxArgs, "--ro-bind", file, file)
		}
	}
	sandboxArgs = append(sandboxArgs, "--bind", root, root, "--chdir", root,
		"--setenv", "PATH", strings.Join(e.path, ":"), "--setenv", "LANG", "C.UTF-8", "--setenv", "HOME", "/run/home", "--setenv", "XDG_CACHE_HOME", "/run/cache", "--setenv", "TMPDIR", "/tmp", "--", executable)
	sandboxArgs = append(sandboxArgs, args...)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// A dedicated bwrap status descriptor distinguishes sandbox startup failures
	// from ordinary nonzero command exits without interpreting command stderr.
	status, err := os.CreateTemp("", "command-sandbox-status-*")
	if err != nil {
		return CommandResult{}, fmt.Errorf("%w: create sandbox status pipe: %v", ErrSandboxStartup, err)
	}
	defer status.Close()
	if err := os.Remove(status.Name()); err != nil {
		return CommandResult{}, fmt.Errorf("%w: prepare sandbox status pipe: %v", ErrSandboxStartup, err)
	}
	// Network namespaces do not isolate pathname Unix sockets in bind mounts.
	// An inherited seccomp filter closes that route even if a host socket is
	// created in a mounted directory after validation.
	socketFilter, err := commandSocketFilter()
	if err != nil {
		return CommandResult{}, fmt.Errorf("%w: prepare socket filter: %v", ErrSandboxStartup, err)
	}
	defer socketFilter.Close()
	sandboxArgs = append([]string{"--json-status-fd", "3", "--seccomp", "4"}, sandboxArgs...)
	cmd := exec.CommandContext(ctx, e.bwrap, sandboxArgs...)
	cmd.ExtraFiles = []*os.File{status, socketFilter}
	cmd.Env = []string{}
	// Killing bwrap triggers --die-with-parent; exiting the PID namespace's init
	// kills every remaining descendant, including detached background processes.
	cmd.WaitDelay = time.Second
	capture := &commandCapture{}
	cmd.Stdout = commandStream{capture, false}
	cmd.Stderr = commandStream{capture, true}
	err = cmd.Run()
	result := CommandResult{Stdout: capture.stdout.String(), Stderr: capture.stderr.String(), ExitCode: -1, Truncated: capture.truncated}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("command interrupted: %w", ctx.Err())
	}
	_, _ = status.Seek(0, io.SeekStart)
	decoder := json.NewDecoder(io.LimitReader(status, 4096))
	completed := false
	for {
		var message map[string]json.RawMessage
		if decoder.Decode(&message) != nil {
			break
		}
		if _, ok := message["exit-code"]; ok {
			completed = true
		}
	}
	if err != nil {
		// bwrap reports the command's exit through the status fd. A descendant
		// that inherited a pipe can make os/exec return ErrWaitDelay after the
		// entrypoint already completed; that is still an ordinary command result
		// once the sandbox has been torn down.
		var exitErr *exec.ExitError
		if completed && (errors.Is(err, exec.ErrWaitDelay) || errors.As(err, &exitErr)) {
			return result, nil
		}
		return result, fmt.Errorf("%w: %v: %s", ErrSandboxStartup, err, result.Stderr)
	}
	return result, nil
}

func containsDirectory(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Only execution time and captured output are bounded. There are no CPU,
// memory, process-count or disk quotas in this version.
type commandCapture struct {
	mu             sync.Mutex
	stdout, stderr bytes.Buffer
	size           int
	truncated      bool
}
type commandStream struct {
	capture *commandCapture
	stderr  bool
}

func (s commandStream) Write(data []byte) (int, error) {
	c := s.capture
	// Both stream copier goroutines take this lock before consuming the shared
	// budget, so concurrent stdout/stderr writes can never exceed 1 MiB.
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(data)
	available := (1 << 20) - c.size
	if len(data) > available {
		data = data[:available]
		c.truncated = true
	}
	if s.stderr {
		c.stderr.Write(data)
	} else {
		c.stdout.Write(data)
	}
	c.size += len(data)
	return n, nil // Keep draining discarded output so a full pipe cannot stall the child.
}
