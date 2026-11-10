package mcp

import (
	"fmt"
	"os"
	"path/filepath"

	hostconfig "github.com/collie-agent/codex-go/internal/config"
)

type cacheMount struct {
	source, target string
	env            []string
}

func defaultGoProxy(proxy string) string {
	if proxy == "" {
		return "off"
	}
	return proxy
}

// Cache grants are independent of language runtimes: any tool can opt in by
// mapping its cache environment variable to a named persistent directory.
func (e *CommandExecutor) configureCaches(cfg Config) error {
	if err := hostconfig.ValidateExecCaches(cfg.ExecCaches, cfg.ExecEnv); err != nil {
		return err
	}
	workspace, err := resolveWorkspace(cfg.Workspace)
	if len(cfg.ExecCaches) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	for _, cache := range cfg.ExecCaches {
		// Resolve existing ancestors before creating directories, so symlinks cannot
		// turn an apparently dedicated cache into a protected host directory.
		candidate := filepath.Clean(cache.Directory)
		ancestor := candidate
		for {
			_, err := os.Lstat(ancestor)
			if err == nil {
				break
			}
			if !os.IsNotExist(err) {
				return err
			}
			ancestor = filepath.Dir(ancestor)
		}
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err != nil {
			return err
		}
		suffix, _ := filepath.Rel(ancestor, candidate)
		candidate = filepath.Join(resolved, suffix)
		home, _ := os.UserHomeDir()
		if home != "" {
			if real, err := filepath.EvalSymlinks(home); err == nil {
				home = real
			}
		}
		protected := append(append([]string{}, e.mounts...), workspace, "/etc", "/proc", "/dev", "/sys", "/run")
		if candidate == "/" || (home != "" && containsDirectory(candidate, home)) {
			return fmt.Errorf("unsafe exec cache directory %q", cache.Directory)
		}
		for _, dir := range protected {
			if containsDirectory(candidate, dir) || containsDirectory(dir, candidate) {
				return fmt.Errorf("exec cache directory overlaps protected directory %s", dir)
			}
		}
		for _, previous := range e.caches {
			if containsDirectory(candidate, previous.source) || containsDirectory(previous.source, candidate) {
				return fmt.Errorf("exec cache directories overlap")
			}
		}
		if err := os.MkdirAll(candidate, 0700); err != nil {
			return err
		}
		info, err := os.Stat(candidate)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("exec cache is not a directory")
		}
		// A dedicated Go grant replaces the legacy host cache instead of
		// exposing an additional, unused host dependency directory.
		for _, name := range cache.Env {
			if name == "GOMODCACHE" {
				e.goModCache = ""
			}
		}
		e.caches = append(e.caches, cacheMount{source: candidate, target: "/run/caches/" + cache.Name, env: append([]string(nil), cache.Env...)})
	}
	return nil
}
