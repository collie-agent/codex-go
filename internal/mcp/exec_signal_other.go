//go:build !linux

package mcp

import (
	"os"
	"os/exec"
)

// The executor rejects non-Linux hosts before starting any process.
func configureSessionStop(cmd *exec.Cmd, status *os.File) func() error {
	return func() error { return nil }
}
