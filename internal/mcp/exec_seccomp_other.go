//go:build !linux

package mcp

import (
	"fmt"
	"os"
)

func commandSocketFilter() (*os.File, error) {
	return nil, fmt.Errorf("command sandbox requires Linux seccomp")
}
