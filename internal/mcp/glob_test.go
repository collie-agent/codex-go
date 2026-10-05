package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func callGlob(t *testing.T, workspace string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	registerGlobTool(server, workspace, 0)
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client"}, nil)
	clientTransport, serverTransport := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), serverTransport) }()
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "glob", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGlobFindsSortedFilesAndPaginates(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"z.txt", "a.txt", "nested/m.txt"} {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := callGlob(t, workspace, map[string]any{"pattern": "**/*.txt", "limit": 2})
	if result.IsError {
		t.Fatalf("glob failed: %#v", result.Content)
	}
	want := map[string]any{
		"items":     []any{map[string]any{"path": "a.txt"}, map[string]any{"path": "nested/m.txt"}},
		"truncated": true, "next_offset": float64(2),
	}
	if !reflect.DeepEqual(result.StructuredContent, want) {
		t.Fatalf("structured result = %#v, want %#v", result.StructuredContent, want)
	}
}

func TestGlobHonorsIgnoredHiddenAndGitRules(t *testing.T) {
	workspace := t.TempDir()
	for _, name := range []string{"visible.txt", ".hidden.txt", "ignored.txt", ".git/config"} {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := callGlob(t, workspace, map[string]any{"pattern": "*"})
	if result.IsError {
		t.Fatalf("glob failed: %#v", result.Content)
	}
	if got, want := result.StructuredContent.(map[string]any)["items"], []any{map[string]any{"path": ".gitignore"}, map[string]any{"path": ".hidden.txt"}, map[string]any{"path": "visible.txt"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %#v, want %#v", got, want)
	}
	result = callGlob(t, workspace, map[string]any{"pattern": "*", "include_ignored": true})
	if result.IsError {
		t.Fatalf("glob include_ignored failed: %#v", result.Content)
	}
	if got, want := result.StructuredContent.(map[string]any)["items"], []any{map[string]any{"path": ".gitignore"}, map[string]any{"path": ".hidden.txt"}, map[string]any{"path": "ignored.txt"}, map[string]any{"path": "visible.txt"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("items = %#v, want %#v", got, want)
	}
}

func TestGlobRejectsInvalidPathAndParameters(t *testing.T) {
	workspace := t.TempDir()
	for _, args := range []map[string]any{
		{"pattern": "*", "path": "missing"},
		{"pattern": "*", "path": "../"},
		{"pattern": "*", "offset": -1},
	} {
		result := callGlob(t, workspace, args)
		if !result.IsError {
			t.Fatalf("args %#v unexpectedly succeeded", args)
		}
	}
}

func TestGlobByteTruncationContinuesAtReturnedItemCount(t *testing.T) {
	workspace := t.TempDir()
	for i := 0; i < 3200; i++ {
		name := fmt.Sprintf("%04d-%s.txt", i, strings.Repeat("x", 190))
		if err := os.WriteFile(filepath.Join(workspace, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := callGlob(t, workspace, map[string]any{"pattern": "*", "limit": 3000})
	if result.IsError {
		t.Fatalf("glob failed: %#v", result.Content)
	}
	structured := result.StructuredContent.(map[string]any)
	items := structured["items"].([]any)
	if len(items) >= 3000 || structured["truncated"] != true {
		t.Fatalf("byte limit did not truncate the page: item count=%d result=%#v", len(items), structured)
	}
	if got := structured["next_offset"]; got != float64(len(items)) {
		t.Fatalf("next_offset = %#v, want returned item count %d", got, len(items))
	}
}
