package mcp

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// 保留子进程诊断而不只返回 exit status；包装原始错误以保留 errors.Is/As。
func commandError(operation string, err error, stderr string) error {
	var exitErr *exec.ExitError
	if stderr == "" && errors.As(err, &exitErr) {
		stderr = string(exitErr.Stderr)
	}
	detail := strings.TrimSpace(strings.ToValidUTF8(stderr, "\uFFFD"))
	if detail != "" {
		return fmt.Errorf("%s: %w; stderr: %s", operation, err, detail)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// rg 使用流式 stdout 时必须单独捕获 stderr，并限制诊断大小。
type diagnosticBuffer struct {
	text      strings.Builder
	truncated bool
}

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	const limit = 16 * 1024
	n := len(p)
	remaining := limit - b.text.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.text.Write(p)
	return n, nil
}

func (b *diagnosticBuffer) String() string {
	if b.truncated {
		return b.text.String() + "\n[stderr truncated]"
	}
	return b.text.String()
}
