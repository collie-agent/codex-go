package config

import (
	"fmt"
	"path/filepath"
	"regexp"
)

var execEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateExecPolicy validates host authorization without resolving or exposing values.
// Runtime-managed variables cannot be overridden by an environment grant.
func ValidateExecPolicy(network string, env []string) error {
	if network != "" && network != "none" && network != "host" {
		return fmt.Errorf("exec_network must be none or host")
	}
	for _, name := range env {
		if !execEnvName.MatchString(name) {
			return fmt.Errorf("exec_env entries must be environment variable names, not assignments")
		}
		switch name {
		case "PATH", "HOME", "LANG", "TMPDIR", "XDG_CACHE_HOME", "GOMODCACHE", "GOPROXY", "GOSUMDB", "GOTOOLCHAIN":
			return fmt.Errorf("exec_env cannot override runtime-managed variable %s", name)
		}
	}
	return nil
}

// ExecCache grants a dedicated persistent directory to command processes.
// Env maps language-specific cache variables to the sandbox cache directory.
type ExecCache struct {
	Name      string   `yaml:"name" json:"name"`
	Directory string   `yaml:"directory" json:"directory"`
	Env       []string `yaml:"env" json:"env"`
}

var execCacheName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ValidateExecCaches checks portable configuration without creating directories.
// Filesystem grants are resolved and validated by the command executor.
func ValidateExecCaches(caches []ExecCache, environment []string) error {
	names, variables := map[string]bool{}, map[string]bool{}
	for _, cache := range caches {
		if !execCacheName.MatchString(cache.Name) || names[cache.Name] {
			return fmt.Errorf("invalid or duplicate exec cache name %q", cache.Name)
		}
		names[cache.Name] = true
		if !filepath.IsAbs(cache.Directory) {
			return fmt.Errorf("exec cache directory must be absolute")
		}
		for _, name := range cache.Env {
			if name != "GOMODCACHE" {
				if err := ValidateExecPolicy("", []string{name}); err != nil {
					return err
				}
			}
			if variables[name] {
				return fmt.Errorf("duplicate exec cache environment variable %s", name)
			}
			for _, grant := range environment {
				if name == grant {
					return fmt.Errorf("exec cache environment conflicts with exec_env: %s", name)
				}
			}
			variables[name] = true
		}
	}
	return nil
}
