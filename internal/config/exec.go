package config

import (
	"fmt"
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
