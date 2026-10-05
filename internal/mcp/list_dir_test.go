package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func callListDir(t *testing.T, workspace string, args map[string]any) map[string]any {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	if err := registerListDirTool(server, workspace, 30_000_000_000); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client"}, nil)
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), serverTransport) }()
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "list_dir", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("list_dir failed: %#v", result.Content)
	}
	value, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured result = %#v", result.StructuredContent)
	}
	return value
}

func TestListDirReturnsSortedDirectChildrenByDefault(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"zeta", ".hidden", "alpha"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(workspace, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}

	got := callListDir(t, workspace, nil)
	want := map[string]any{
		"items": []any{
			map[string]any{"path": ".hidden", "type": "file"},
			map[string]any{"path": "alpha", "type": "file"},
			map[string]any{"path": "dir", "type": "directory"},
			map[string]any{"path": "zeta", "type": "file"},
		},
		"truncated":   false,
		"next_offset": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("structured result = %#v, want %#v", got, want)
	}
}

func TestListDirSupportsDepthPaginationAndSymlinks(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a/root.txt", "a/b/deep.txt", ".secret"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a/root.txt", filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}

	got := callListDir(t, workspace, map[string]any{"depth": 2, "limit": 2})
	want := map[string]any{
		"items": []any{
			map[string]any{"path": ".secret", "type": "file"},
			map[string]any{"path": "a", "type": "directory"},
		},
		"truncated":   true,
		"next_offset": float64(2),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("first page = %#v, want %#v", got, want)
	}

	got = callListDir(t, workspace, map[string]any{"depth": 2, "offset": 2, "limit": 2})
	want = map[string]any{
		"items": []any{
			map[string]any{"path": "a/b", "type": "directory"},
			map[string]any{"path": "a/root.txt", "type": "file"},
		},
		"truncated":   true,
		"next_offset": float64(4),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("second page = %#v, want %#v", got, want)
	}

	got = callListDir(t, workspace, map[string]any{"depth": 1, "offset": 3})
	if got["truncated"] != false || got["next_offset"] != nil {
		t.Fatalf("last page metadata = %#v", got)
	}
}
