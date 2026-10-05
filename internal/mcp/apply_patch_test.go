package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestApplyPatchAddsModifiesAndDeletesThroughMCP(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "old.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	registerApplyPatchTool(server, workspace, 0)
	client := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil)
	ct, st := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), st) }()
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	patch := "--- old.txt\n+++ old.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n--- /dev/null\n+++ new.txt\n@@ -0,0 +1 @@\n+created\n--- old.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-one\n-TWO\n"
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "apply_patch", Arguments: map[string]any{"patch": patch}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		if len(result.Content) > 0 {
			if msg, ok := result.Content[0].(*sdk.TextContent); ok {
				t.Fatalf("apply_patch failed: %s", msg.Text)
			}
		}
		t.Fatalf("apply_patch failed: %#v", result.Content)
	}
	if len(result.Content) == 0 || result.StructuredContent == nil {
		t.Fatalf("apply_patch omitted readable or structured output: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(workspace, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old.txt still exists: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "created\n" {
		t.Fatalf("new.txt = %q", data)
	}
}

func TestApplyPatchValidatesEverythingBeforeWriting(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "keep.txt")
	if err := os.WriteFile(path, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	registerApplyPatchTool(server, workspace, 0)
	client := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil)
	ct, st := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), st) }()
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	patch := "--- keep.txt\n+++ keep.txt\n@@ -1 +1 @@\n-keep\n+changed\n--- missing.txt\n+++ missing.txt\n@@ -1 +1 @@\n-nope\n+never\n"
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "apply_patch", Arguments: map[string]any{"patch": patch}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected conflict")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep\n" {
		t.Fatalf("keep.txt changed: %q", data)
	}
}

func TestApplyPatchRejectsSymlinkedParentOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
	registerApplyPatchTool(server, workspace, 0)
	client := sdk.NewClient(&sdk.Implementation{Name: "client"}, nil)
	ct, st := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), st) }()
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	patch := "--- linked/victim.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-safe\n"
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "apply_patch", Arguments: map[string]any{"patch": patch}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected symlinked parent to fail")
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "safe\n" {
		t.Fatalf("outside file changed: %q", data)
	}
}
