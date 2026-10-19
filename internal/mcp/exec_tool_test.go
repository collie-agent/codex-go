package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func execToolSession(t *testing.T, config Config) *sdk.ClientSession {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "exec-test"}, nil)
	if err := RegisterTools(server, config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ct, st := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, st) }()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func decodeExecResult(t *testing.T, result *sdk.CallToolResult) execToolResult {
	t.Helper()
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out execToolResult
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("missing text fallback: %#v", result)
	}
	var text execToolResult
	if err := json.Unmarshal([]byte(result.Content[0].(*sdk.TextContent).Text), &text); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, text) {
		t.Fatal("structured result and text differ")
	}
	return out
}

func TestExecToolRunsAtWorkspaceAndReturnsNonzeroExit(t *testing.T) {
	workspace := t.TempDir()
	session := execToolSession(t, Config{Workspace: workspace})
	result := fileToolCall(t, session, "exec", map[string]any{"command": "python3", "args": []string{"-c", `import pathlib,sys
pathlib.Path('artifact').write_text('created')
print('out'); print('err',file=sys.stderr); sys.exit(7)`}})
	out := decodeExecResult(t, result)
	if result.IsError || out.ExitCode != 7 || out.Stdout != "out\n" || out.Stderr != "err\n" || out.Truncated || out.ErrorKind != "" {
		t.Fatalf("unexpected result: %+v isError=%v", out, result.IsError)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "artifact")); err != nil || string(data) != "created" {
		t.Fatalf("artifact=%q err=%v", data, err)
	}
	result = fileToolCall(t, session, "exec", map[string]any{"command": "python3"})
	if out := decodeExecResult(t, result); result.IsError || out.ExitCode != 0 || out.Stdout != "" {
		t.Fatalf("omitted args or stdin EOF failed: %+v", out)
	}
}

func TestExecToolRejectsInvalidInputs(t *testing.T) {
	session := execToolSession(t, Config{Workspace: t.TempDir()})
	for _, args := range []map[string]any{
		{}, {"command": 1}, {"command": "git", "args": "status"},
		{"command": "git", "args": nil}, {"command": "git", "args": []any{1}},
		{"command": "git", "cwd": "/"}, {"command": "git", "env": map[string]string{"HOME": "/"}},
		{"command": "git", "runtime_dirs": []string{"/"}},
	} {
		if result := fileToolCall(t, session, "exec", args); !result.IsError {
			t.Errorf("accepted invalid arguments: %#v", args)
		}
	}
	for _, command := range []string{"sh", "/usr/bin/git", "git status", ""} {
		result := fileToolCall(t, session, "exec", map[string]any{"command": command})
		out := decodeExecResult(t, result)
		if !result.IsError || out.ErrorKind != "parameter" || out.ExitCode != -1 || out.Error == "" {
			t.Errorf("command=%q unexpected result: %+v", command, out)
		}
	}
}

func TestExecToolUsesHostConfiguredRuntime(t *testing.T) {
	runtimeDir := t.TempDir()
	writeFixture(t, runtimeDir, "data", "runtime")
	session := execToolSession(t, Config{Workspace: t.TempDir(), RuntimeDirs: []string{runtimeDir}})
	result := fileToolCall(t, session, "exec", map[string]any{"command": "python3", "args": []string{"-c", `import pathlib,sys
p=pathlib.Path(sys.argv[1])
assert p.read_text() == 'runtime'
try: p.write_text('changed')
except OSError: pass
else: raise AssertionError('runtime writable')
print('ok')`, filepath.Join(runtimeDir, "data")}})
	if out := decodeExecResult(t, result); result.IsError || out.ExitCode != 0 || out.Stdout != "ok\n" {
		t.Fatalf("runtime unavailable: %+v", out)
	}
}

func TestExecToolStartupFailureLeavesFileToolsAvailable(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "file", "data")
	// Invalid host runtime configuration must not prevent tool discovery or
	// ordinary file operations, but exec must never fall back to host execution.
	session := execToolSession(t, Config{Workspace: workspace, RuntimeDirs: []string{filepath.Join(t.TempDir(), "missing")}})
	result := fileToolCall(t, session, "exec", map[string]any{"command": "python3", "args": []string{"-c", "print('must not run')"}})
	out := decodeExecResult(t, result)
	if !result.IsError || out.ErrorKind != "sandbox_startup" || out.ExitCode != -1 || out.Stdout != "" || out.Error == "" {
		t.Fatalf("unexpected startup failure: %+v", out)
	}
	if result := fileToolCall(t, session, "read_file", map[string]any{"path": "file"}); result.IsError {
		t.Fatalf("file tool unavailable: %#v", result)
	}
}

func TestExecToolTimeoutPreservesOutput(t *testing.T) {
	session := execToolSession(t, Config{Workspace: t.TempDir(), Timeout: 300 * time.Millisecond})
	result := fileToolCall(t, session, "exec", map[string]any{"command": "python3", "args": []string{"-u", "-c", "print('started'); import time; time.sleep(20)"}})
	out := decodeExecResult(t, result)
	if !result.IsError || out.ErrorKind != "timeout" || out.Stdout != "started\n" || out.Error == "" {
		t.Fatalf("unexpected timeout: %+v", out)
	}
}

func TestExecToolFitsEscapedOutputIntoWireLimit(t *testing.T) {
	session := execToolSession(t, Config{Workspace: t.TempDir()})
	result := fileToolCall(t, session, "exec", map[string]any{"command": "python3", "args": []string{"-c", `import os
os.write(1, ('界\x00"\\'*200000).encode())
os.write(2, b'err')`}})
	out := decodeExecResult(t, result)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || out.ExitCode != 0 || !out.Truncated || out.Stdout == "" || !utf8.ValidString(out.Stdout) || len(encoded) > maxResponseBytes {
		t.Fatalf("unexpected large output: isError=%v exit=%d truncated=%v stdout=%d bytes=%d", result.IsError, out.ExitCode, out.Truncated, len(out.Stdout), len(encoded))
	}
}
