package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReadFileReturnsRequestedLinesWithLineNumbers(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	if err := RegisterTools(server, Config{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client"}, nil)
	clientSession, serverSession := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), serverSession) }()
	session, err := client.Connect(context.Background(), clientSession, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "read_file",
		Arguments: map[string]any{"path": "notes.txt", "offset": 2, "limit": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("read_file failed: %#v", result.Content)
	}
	if got, want := result.StructuredContent, map[string]any{
		"lines": []any{
			map[string]any{"line": float64(2), "text": "beta"},
			map[string]any{"line": float64(3), "text": "gamma"},
		},
		"truncated":   false,
		"next_offset": nil,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("structured result = %#v, want %#v", got, want)
	}
}

func TestReadFileRejectsWorkspaceEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	if err := RegisterTools(server, Config{Workspace: workspace}); err != nil {
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

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "read_file", Arguments: map[string]any{"path": "../" + filepath.Base(filepath.Dir(outside)) + "/secret.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("expected workspace escape to fail, got %#v", result.StructuredContent)
	}
}

func TestReadFileRejectsBinaryContent(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "binary.bin"), []byte{'a', 0, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	if err := RegisterTools(server, Config{Workspace: workspace}); err != nil {
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
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "read_file", Arguments: map[string]any{"path": "binary.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("expected binary file to fail, got %#v", result.StructuredContent)
	}
}
