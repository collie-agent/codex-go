package mcp

import (
	"fmt"
	hostconfig "github.com/collie-agent/codex-go/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var defaultExecCommands = []string{"cargo", "find", "git", "go", "grep", "npm", "poetry", "python", "python3", "rg", "rustc", "sed", "ts-node", "tsx"}

// execCommands keeps built-in entrypoints available; configuration only adds commands.
func execCommands(extra []string) []string {
	commands := make([]string, 0, len(defaultExecCommands)+len(extra))
	seen := make(map[string]bool)
	for _, name := range append(append([]string(nil), defaultExecCommands...), extra...) {
		if !seen[name] {
			commands = append(commands, name)
			seen[name] = true
		}
	}
	return commands
}

func (e *CommandExecutor) commandAllowed(name string) bool {
	if e.allowed != nil {
		return e.allowed[name]
	}
	for _, command := range defaultExecCommands {
		if command == name {
			return true
		}
	}
	return false
}

// NewCommandExecutorWithConfig snapshots host discovery before any tool call.
// Discovery grants read access to runtime directories, never host HOME wholesale.
func NewCommandExecutorWithConfig(config Config) (*CommandExecutor, error) {
	if err := hostconfig.ValidateExecPolicy(config.ExecNetwork, config.ExecEnv); err != nil {
		return nil, err
	}
	e, err := NewCommandExecutor(config.RuntimeDirs...)
	if err != nil {
		return nil, err
	}
	e.network = config.ExecNetwork
	// Resolve authorized values once; tool calls cannot change the environment grant.
	for _, name := range config.ExecEnv {
		if value, ok := os.LookupEnv(name); ok {
			e.environment = append(e.environment, name+"="+value)
		}
	}
	commands := execCommands(config.ExecCommands)
	e.allowed = make(map[string]bool)
	for _, name := range commands {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\\ \t\r\n") {
			return nil, fmt.Errorf("invalid exec whitelist entry %q", name)
		}
		e.allowed[name] = true
	}
	if !config.DiscoverHostCommands {
		return e, nil
	}
	workspace, err := resolveWorkspace(config.Workspace)
	if err != nil {
		return nil, err
	}
	e.discovered = make(map[string]string)
	for _, name := range commands {
		// Existing trusted runtimes retain priority over inherited host PATH.
		found := false
		for _, dir := range e.path {
			candidate := filepath.Join(dir, name)
			resolved, resolveErr := filepath.EvalSymlinks(candidate)
			info, statErr := os.Stat(candidate)
			if resolveErr != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
				continue
			}
			for _, mount := range e.mounts {
				if containsDirectory(mount, resolved) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			continue
		}
		candidate, lookupErr := exec.LookPath(name)
		if lookupErr != nil || !filepath.IsAbs(candidate) {
			continue
		}
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr != nil || containsDirectory(workspace, candidate) || containsDirectory(workspace, resolved) {
			continue
		}
		dir := filepath.Dir(resolved)
		// Python launchers need the venv configuration and site-packages beside bin.
		pathDir := dir
		if filepath.Base(dir) == "bin" {
			parent := filepath.Dir(dir)
			if info, err := os.Stat(filepath.Join(parent, "pyvenv.cfg")); err == nil && info.Mode().IsRegular() {
				dir = parent
			}
		}
		if containsDirectory(dir, workspace) {
			continue
		}
		covered := false
		for _, mount := range e.mounts {
			if containsDirectory(mount, dir) {
				covered = true
				break
			}
		}
		if !covered {
			validated, validateErr := validateRuntimeDirs([]string{dir}, e.mounts)
			if validateErr != nil {
				continue
			}
			e.mounts = append(e.mounts, validated...)
			e.runtime = append(e.runtime, validated...)
		}
		e.discovered[name] = resolved
		e.path = append(e.path, pathDir)
	}
	return e, nil
}
