package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func callGrep(t *testing.T, workspace string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	registerGrepTool(server, workspace, time.Second)
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client"}, nil)
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), serverTransport) }()
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "grep", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGrepSearchesFileAndReturnsSortedMatches(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("Beta\nalpha beta\nalpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callGrep(t, workspace, map[string]any{"pattern": "alpha", "path": "notes.txt"})
	if result.IsError {
		t.Fatalf("grep failed: %#v", result.Content)
	}
	got := result.StructuredContent.(map[string]any)
	items := got["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %#v, want two matches", items)
	}
	first := items[0].(map[string]any)
	if first["path"] != "notes.txt" || first["line"] != float64(2) || first["text"] != "alpha beta" {
		t.Fatalf("first item = %#v", first)
	}
}

func TestGrepOptionsAndPagination(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "a.go"), []byte("Alpha\nalpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "b.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callGrep(t, workspace, map[string]any{"pattern": "ALPHA", "ignore_case": true, "glob": "*.go", "offset": 0, "limit": 1})
	if result.IsError {
		t.Fatalf("grep failed: %#v", result.Content)
	}
	got := result.StructuredContent.(map[string]any)
	if got["truncated"] != true || got["next_offset"] != float64(1) {
		t.Fatalf("pagination = %#v", got)
	}
	items := got["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["line"] != float64(1) {
		t.Fatalf("items = %#v", items)
	}
}

func TestGrepHonorsIgnoreAndHiddenRules(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "ignored.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".hidden"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callGrep(t, workspace, map[string]any{"pattern": "needle"})
	if result.IsError {
		t.Fatalf("grep failed: %#v", result.Content)
	}
	items := result.StructuredContent.(map[string]any)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["path"] != ".hidden" {
		t.Fatalf("default ignored/hidden result = %#v", items)
	}
	result = callGrep(t, workspace, map[string]any{"pattern": "needle", "include_ignored": true})
	if result.IsError {
		t.Fatalf("grep include_ignored failed: %#v", result.Content)
	}
	if got := len(result.StructuredContent.(map[string]any)["items"].([]any)); got != 2 {
		t.Fatalf("include_ignored count = %d, want 2", got)
	}
}

func TestGrepRejectsSymlinkAndInvalidPattern(t *testing.T) {
	workspace := t.TempDir()
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(workspace, "link.txt")); err != nil {
		t.Fatal(err)
	}
	result := callGrep(t, workspace, map[string]any{"pattern": "needle", "path": "link.txt"})
	if !result.IsError {
		t.Fatal("expected symlink path to fail")
	}
	result = callGrep(t, workspace, map[string]any{"pattern": "["})
	if !result.IsError {
		t.Fatal("expected invalid pattern to fail")
	}
}
