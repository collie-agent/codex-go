package mcp

import (
	"fmt"
	"os"
	"path/filepath"
)

// validateRuntimeDirs canonicalizes host supplied mounts before they become
// part of the immutable command PATH and sandbox mount namespace.
func validateRuntimeDirs(dirs []string, systemMounts []string) ([]string, error) {
	// Host HOME is needed only to reject unsafe explicit grants. The default
	// executor uses an isolated HOME and must work even without a host HOME.
	if len(dirs) == 0 {
		return nil, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve host home: %w", err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return nil, fmt.Errorf("resolve host home: %w", err)
	}
	result := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		absolute, err := filepath.Abs(filepath.Clean(dir))
		if err != nil {
			return nil, fmt.Errorf("invalid runtime directory %q: %w", dir, err)
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("invalid runtime directory %q: %w", dir, err)
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("runtime path is not a directory: %q", dir)
		}
		// A runtime mount grants every sandbox process read access, so the
		// host root and whole HOME are never valid explicit grants.
		if resolved == "/" || resolved == home || containsDirectory(resolved, home) {
			return nil, fmt.Errorf("runtime directory %q is outside the allowed boundary", dir)
		}
		protected := append(append([]string{}, systemMounts...), "/etc", "/proc", "/dev", "/sys", "/run")
		for _, system := range protected {
			if containsDirectory(system, resolved) || containsDirectory(resolved, system) {
				return nil, fmt.Errorf("runtime directory %q overlaps system runtime %s", dir, system)
			}
		}
		for _, previous := range result {
			if containsDirectory(previous, resolved) || containsDirectory(resolved, previous) {
				return nil, fmt.Errorf("runtime directories overlap: %s and %s", previous, resolved)
			}
		}
		result = append(result, resolved)
	}
	return result, nil
}
