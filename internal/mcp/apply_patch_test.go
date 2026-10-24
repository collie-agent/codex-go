package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestApplyPatchErrorsExplainHowToRepairArguments(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "file.txt", "current\n")
	session := fileToolSession(t, workspace)
	for _, tc := range []struct {
		name, patch string
		want        []string
	}{
		{"empty", "", []string{"[1201]", "non-empty string", `Pass {"patch":`, "actual newlines"}},
		{"wrong format", "*** Begin Patch\n*** End Patch\n", []string{"[1201]", "---/+++", "not *** Begin Patch"}},
		{"missing header", "--- file.txt\n", []string{"patch line 2", "expected +++ header", "<end of patch>"}},
		{"bad hunk", "--- file.txt\n+++ file.txt\n@@ wrong @@\n", []string{"patch line 3", "malformed hunk header", "@@ -oldStart,oldCount +newStart,newCount @@", `"@@ wrong @@\n"`}},
		{"bad body", "--- file.txt\n+++ file.txt\n@@ -1 +1 @@\ncurrent\n", []string{"patch line 4", "invalid hunk body line", "prefix context with a space", `"current\n"`}},
		{"empty hunk", "--- file.txt\n+++ file.txt\n@@ -1 +1 @@\n", []string{"patch line 3", "empty hunk"}},
		{"no hunks", "--- file.txt\n+++ file.txt\n", []string{"patch line 1", `no hunks for file "file.txt"`, "add an @@"}},
		{"conflict", "--- file.txt\n+++ file.txt\n@@ -1 +1 @@\n-stale\n+changed\n", []string{"[1203]", `"file.txt", hunk 1`, "searched forward from line 1", `"stale\n"`, "Re-read the file", "Changing line numbers alone"}},
		{"insertion", "--- file.txt\n+++ file.txt\n@@ -10,0 +11 @@\n+added\n", []string{"[1203]", "hunk 1", "requested position 10", "allowed positions 0..1", "correct the hunk start"}},
		{"rename", "--- file.txt\n+++ other.txt\n@@ -1 +1 @@\n-current\n+changed\n", []string{"renames are not supported", `"file.txt"`, `"other.txt"`, "same workspace-relative path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := fileToolCall(t, session, "apply_patch", map[string]any{"patch": tc.patch})
			if !result.IsError || len(result.Content) == 0 {
				t.Fatalf("expected readable tool error: %#v", result)
			}
			message := result.Content[0].(*sdk.TextContent).Text
			for _, want := range tc.want {
				if !strings.Contains(message, want) {
					t.Errorf("error %q missing %q", message, want)
				}
			}
			data, err := os.ReadFile(filepath.Join(workspace, "file.txt"))
			if err != nil || string(data) != "current\n" {
				t.Fatalf("failed patch changed file: %q, %v", data, err)
			}
		})
	}
}

func TestApplyPatchErrorExcerptsAreBounded(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "file.txt", "current\n")
	session := fileToolSession(t, workspace)
	longLine := strings.Repeat("界", 500000)
	for _, body := range []string{longLine + "\n", "-" + longLine + "\n+changed\n"} {
		result := fileToolCall(t, session, "apply_patch", map[string]any{"patch": "--- file.txt\n+++ file.txt\n@@ -1 +1 @@\n" + body})
		if !result.IsError {
			t.Fatal("expected tool error")
		}
		message := result.Content[0].(*sdk.TextContent).Text
		if len(message) > 4096 || !strings.Contains(message, "(truncated)") || strings.ContainsRune(message, '\uFFFD') {
			t.Fatalf("expected bounded UTF-8 diagnostic, got %d bytes: %.500s", len(message), message)
		}
	}
}
