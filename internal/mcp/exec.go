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
	network     string
	environment []string
	bwrap       string
	path        []string
	mounts      []string
	runtime     []string
	goModCache  string
	caches      []cacheMount
	goProxy     string
	allowed     map[string]bool
	discovered  map[string]string
}

// CommandResult retains the complete stdout and stderr captured during execution.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Error classes let callers distinguish rejected inputs from a sandbox that
// could not be started, while retaining the underlying diagnostic.
var (
	ErrCommandParameter = errors.New("invalid command parameter")
	ErrSandboxStartup   = errors.New("sandbox startup failed")
	ErrCommandCleanup   = errors.New("sandbox cleanup failed")
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
	controlledPath := []string{"/usr/local/bin", "/usr/bin", "/bin"}
	for _, dir := range runtimes {
		controlledPath = append(controlledPath, dir, filepath.Join(dir, "bin"))
	}
	// Trust the Go distribution selected by the host program GOROOT,
	// never a go executable discovered through the inherited host PATH.
	goRoot := runtime.GOROOT()
	if resolved, err := filepath.EvalSymlinks(goRoot); err == nil && filepath.IsAbs(resolved) {
		if info, err := os.Stat(filepath.Join(resolved, "bin", "go")); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			controlledPath = append(controlledPath, filepath.Join(resolved, "bin"))
			// System mounts already expose distributions installed under /usr.
			covered := false
			for _, mount := range mounts {
				covered = covered || containsDirectory(mount, resolved)
			}
			if !covered {
				mounts = append(mounts, resolved)
				runtimes = append(runtimes, resolved)
			}
		}
	}
	// Resolve only the module cache, never expose the whole host GOPATH/HOME.
	cacheCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cacheCommand := exec.CommandContext(cacheCtx, filepath.Join(goRoot, "bin", "go"), "env", "GOMODCACHE")
	cacheCommand.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	var goModCache string
	if output, err := cacheCommand.Output(); err == nil {
		candidate := strings.TrimSpace(string(output))
		if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
			if validated, err := validateRuntimeDirs([]string{resolved}, mounts); err == nil {
				goModCache = validated[0]
			}
		}
	}
	return &CommandExecutor{bwrap: path, path: controlledPath, mounts: mounts, runtime: runtimes, goModCache: goModCache}, nil
}

// Execute runs at the canonical Workspace root with a 5-minute deadline.
// Nonzero command exits are results; startup failures and interruption are errors.
// Captured output is retained on errors, and interruption wraps ctx.Err().
func (e *CommandExecutor) Execute(ctx context.Context, workspace, name string, args []string) (CommandResult, error) {
	return e.execute(ctx, workspace, name, args, nil)
}

// output streams session logs without retaining a second, unbounded copy.
func (e *CommandExecutor) execute(ctx context.Context, workspace, name string, args []string, output func(bool, []byte)) (CommandResult, error) {
	// Only entrypoint names are accepted; paths must never bypass controlled resolution.
	if !e.commandAllowed(name) {
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
	for _, cache := range e.caches {
		if containsDirectory(root, cache.source) || containsDirectory(cache.source, root) {
			return CommandResult{}, fmt.Errorf("%w: workspace overlaps persistent cache", ErrCommandParameter)
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
		executable = e.discovered[name]
	}
	if executable == "" {
		return CommandResult{}, fmt.Errorf("%w: command unavailable in controlled PATH: %s", ErrCommandParameter, name)
	}
	sandboxArgs := []string{"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--cap-drop", "ALL", "--die-with-parent", "--new-session", "--hostname", "command-sandbox", "--clearenv"}
	// Host networking is an explicit grant to the entire command process tree.
	if e.network != "host" {
		sandboxArgs = append(sandboxArgs, "--unshare-net")
	}
	// A fresh mount namespace exposes system runtimes read-only, never host HOME or /etc wholesale.
	for _, dir := range commandSystemDirs {
		if _, statErr := os.Stat(dir); statErr == nil {
			sandboxArgs = append(sandboxArgs, "--ro-bind", dir, dir)
		}
	}
	sandboxArgs = append(sandboxArgs, "--proc", "/proc", "--dev", "/dev", "--bind", "/tmp", "/tmp", "--tmpfs", "/run", "--dir", "/run/home", "--dir", "/run/cache", "--dir", "/etc")
	if e.network == "host" {
		// Resolve host symlinks before mounting: systemd-resolved commonly keeps
		// the target under /run, which is replaced by the sandbox's private tmpfs.
		resolver, err := filepath.EvalSymlinks("/etc/resolv.conf")
		if err != nil {
			return CommandResult{}, fmt.Errorf("%w: resolve host DNS configuration: %v", ErrSandboxStartup, err)
		}
		sandboxArgs = append(sandboxArgs, "--ro-bind", resolver, "/etc/resolv.conf")
		// Outbound HTTPS needs public trust roots, never private keys or all of /etc.
		for _, certificates := range []string{"/etc/ssl/certs", "/etc/pki/tls/certs", "/etc/pki/ca-trust/extracted"} {
			resolved, err := filepath.EvalSymlinks(certificates)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return CommandResult{}, fmt.Errorf("%w: resolve system certificates: %v", ErrSandboxStartup, err)
			}
			sandboxArgs = append(sandboxArgs, "--ro-bind", resolved, certificates)
		}
	}
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
	// Keep the default host module cache immutable; explicit cache grants below
	// override the cache location with a dedicated writable directory.
	if e.goModCache != "" {
		sandboxArgs = append(sandboxArgs, "--ro-bind", e.goModCache, "/run/go-mod-cache", "--setenv", "GOMODCACHE", "/run/go-mod-cache")
	}
	for _, cache := range e.caches {
		sandboxArgs = append(sandboxArgs, "--bind", cache.source, cache.target)
		for _, name := range cache.env {
			sandboxArgs = append(sandboxArgs, "--setenv", name, cache.target)
		}
	}
	sandboxArgs = append(sandboxArgs, "--setenv", "GOPROXY", defaultGoProxy(e.goProxy), "--setenv", "GOSUMDB", "off", "--setenv", "GOTOOLCHAIN", "local")
	for _, value := range e.environment {
		name, value, _ := strings.Cut(value, "=")
		sandboxArgs = append(sandboxArgs, "--setenv", name, value)
	}
	sandboxArgs = append(sandboxArgs, "--bind", root, root)
	// Rebind Git metadata explicitly: the workspace can contain an inherited
	// read-only .git submount. Never expose a symlink target outside Workspace.
	gitDir := filepath.Join(root, ".git")
	if info, err := os.Lstat(gitDir); err == nil && info.IsDir() {
		sandboxArgs = append(sandboxArgs, "--bind", gitDir, gitDir)
	}
	sandboxArgs = append(sandboxArgs, "--chdir", root,
		"--setenv", "PATH", strings.Join(e.path, ":"), "--setenv", "LANG", "C.UTF-8", "--setenv", "HOME", "/run/home", "--setenv", "XDG_CACHE_HOME", "/run/cache", "--setenv", "TMPDIR", "/tmp", "--", executable)
	sandboxArgs = append(sandboxArgs, args...)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
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
	var awaitCleanup func() error
	if output != nil {
		awaitCleanup = configureSessionStop(cmd, status)
	}
	capture := &commandCapture{output: output}
	cmd.Stdout = commandStream{capture, false}
	cmd.Stderr = commandStream{capture, true}
	err = cmd.Run()
	var cleanupErr error
	if awaitCleanup != nil {
		cleanupErr = awaitCleanup()
	}
	result := CommandResult{Stdout: capture.stdout.String(), Stderr: capture.stderr.String(), ExitCode: -1}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if cleanupErr != nil {
		return result, fmt.Errorf("%w: %v", ErrCommandCleanup, cleanupErr)
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

// Only execution time is bounded. There are no CPU,
// memory, process-count or disk quotas in this version.
type commandCapture struct {
	mu             sync.Mutex
	stdout, stderr bytes.Buffer
	output         func(bool, []byte)
}
type commandStream struct {
	capture *commandCapture
	stderr  bool
}

func (s commandStream) Write(data []byte) (int, error) {
	c := s.capture
	// stdout and stderr are copied concurrently; retain both streams in full.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.output != nil {
		c.output(s.stderr, data)
		return len(data), nil
	}
	if s.stderr {
		return c.stderr.Write(data)
	}
	return c.stdout.Write(data)
}
