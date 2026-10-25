package mcp

import (
	"errors"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"os/exec"
	"strings"
	"testing"
)

func TestGrepReturnsRegexDiagnostic(t *testing.T) {
	result := callGrep(t, t.TempDir(), map[string]any{"pattern": "[", "path": "."})
	var text string
	for _, content := range result.Content {
		if c, ok := content.(*sdk.TextContent); ok {
			text += c.Text
		}
	}
	if !result.IsError || !strings.Contains(text, "regex parse error") || !strings.Contains(text, "unclosed character class") {
		t.Fatalf("missing corrective diagnostic: isError=%v text=%s", result.IsError, text)
	}
}

func TestCommandErrorPreservesCauseAndStderr(t *testing.T) {
	cause := &exec.ExitError{Stderr: []byte("invalid option: --bad\n")}
	err := commandError("find list failed", cause, "")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr != cause || !strings.Contains(err.Error(), "invalid option: --bad") {
		t.Fatalf("lost subprocess diagnostic or cause: %v", err)
	}
}

func TestDiagnosticBufferBoundsOutput(t *testing.T) {
	var buf diagnosticBuffer
	input := strings.Repeat("x", 32*1024)
	n, err := buf.Write([]byte(input))
	if err != nil || n != len(input) || buf.text.Len() != 16*1024 || !strings.HasSuffix(buf.String(), "[stderr truncated]") {
		t.Fatalf("unexpected bounded diagnostic: n=%d len=%d err=%v", n, buf.text.Len(), err)
	}
	if n, err := buf.Write([]byte("more")); n != 4 || err != nil || buf.text.Len() != 16*1024 {
		t.Fatalf("write after limit: n=%d err=%v", n, err)
	}
}
