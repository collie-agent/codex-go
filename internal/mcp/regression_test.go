package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func fileToolSession(t *testing.T, workspace string) *sdk.ClientSession {
	t.Helper()
	server := sdk.NewServer(&sdk.Implementation{Name: "regression-server"}, nil)
	if err := RegisterTools(server, Config{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ct, st := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, st) }()
	client := sdk.NewClient(&sdk.Implementation{Name: "regression-client"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func fileToolCall(t *testing.T, session *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func writeFixture(t *testing.T, workspace, path, content string) {
	t.Helper()
	full := filepath.Join(workspace, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestToolsDistinguishMissingParametersFromZero(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "file.txt", "needle\n")
	session := fileToolSession(t, workspace)
	for _, tc := range []struct {
		name    string
		args    map[string]any
		invalid []string
	}{
		{"read_file", map[string]any{"path": "file.txt"}, []string{"offset", "limit"}},
		{"list_dir", map[string]any{}, []string{"depth", "limit"}},
		{"glob", map[string]any{"pattern": "*"}, []string{"limit"}},
		{"grep", map[string]any{"pattern": "needle"}, []string{"limit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if result := fileToolCall(t, session, tc.name, tc.args); result.IsError {
				t.Fatalf("omitted defaults failed: %#v", result.Content)
			}
			for _, field := range tc.invalid {
				for _, value := range []int{0, -1} {
					tc.args[field] = value
					if result := fileToolCall(t, session, tc.name, tc.args); !result.IsError {
						t.Fatalf("%s=%d succeeded", field, value)
					}
				}
				delete(tc.args, field)
			}
		})
	}
}

func TestReadFileEmptyResultIsAnArray(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "empty.txt", "")
	writeFixture(t, workspace, "file.txt", "line\n")
	session := fileToolSession(t, workspace)
	for _, args := range []map[string]any{
		{"path": "empty.txt"}, {"path": "file.txt", "offset": 20},
	} {
		result := fileToolCall(t, session, "read_file", args)
		if result.IsError {
			t.Fatalf("empty read failed: %#v", result.Content)
		}
		want := map[string]any{"lines": []any{}, "truncated": false, "next_offset": nil}
		if !reflect.DeepEqual(result.StructuredContent, want) {
			t.Fatalf("result = %#v, want %#v", result.StructuredContent, want)
		}
	}
}

func TestSearchesKeepGitAndIgnoreRulesWithUserGlobs(t *testing.T) {
	workspace := t.TempDir()
	for _, path := range []string{"visible.txt", "ignored.txt", ".git/secret.txt", "nested/.git/secret.txt", "nested/visible.txt"} {
		writeFixture(t, workspace, path, "needle\n")
	}
	writeFixture(t, workspace, ".gitignore", "ignored.txt\n")
	session := fileToolSession(t, workspace)
	for _, name := range []string{"glob", "grep"} {
		for _, includeIgnored := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ignored=%t", name, includeIgnored), func(t *testing.T) {
				args := map[string]any{"pattern": "**/*.txt", "include_ignored": includeIgnored}
				if name == "grep" {
					args["pattern"], args["glob"] = "needle", "**/*.txt"
				}
				result := fileToolCall(t, session, name, args)
				if result.IsError {
					t.Fatalf("search failed: %#v", result.Content)
				}
				items := result.StructuredContent.(map[string]any)["items"].([]any)
				var paths []string
				for _, item := range items {
					paths = append(paths, item.(map[string]any)["path"].(string))
				}
				want := []string{"nested/visible.txt", "visible.txt"}
				if includeIgnored {
					want = append([]string{"ignored.txt"}, want...)
				}
				if !reflect.DeepEqual(paths, want) {
					t.Fatalf("paths = %#v, want %#v", paths, want)
				}
			})
		}
		result := fileToolCall(t, session, name, map[string]any{"path": "nested/.git", "pattern": "needle", "include_ignored": true})
		if result.IsError || len(result.StructuredContent.(map[string]any)["items"].([]any)) != 0 {
			t.Fatalf("explicit .git search = %#v", result)
		}
	}
}

func TestGlobUsesRipgrepPatternsRelativeToSearchDirectory(t *testing.T) {
	workspace := t.TempDir()
	for _, path := range []string{"a.go", "b.py", "c.txt", "nested/a.go", "nested/b.py", "nested/c.txt", "nested/deep/a.go", "nested/中文.go"} {
		writeFixture(t, workspace, path, "text\n")
	}
	session := fileToolSession(t, workspace)
	for _, tc := range []struct {
		pattern, path string
		want          []string
	}{
		{"*.{go,py}", ".", []string{"a.go", "b.py"}},
		{"[!a]*.py", "nested", []string{"nested/b.py"}},
		{"**/*.go", "nested", []string{"nested/a.go", "nested/deep/a.go", "nested/中文.go"}},
		{"中文.go", "nested", []string{"nested/中文.go"}},
	} {
		t.Run(tc.pattern+"/"+tc.path, func(t *testing.T) {
			result := fileToolCall(t, session, "glob", map[string]any{"pattern": tc.pattern, "path": tc.path})
			if result.IsError {
				t.Fatalf("glob failed: %#v", result.Content)
			}
			var got []string
			for _, item := range result.StructuredContent.(map[string]any)["items"].([]any) {
				got = append(got, item.(map[string]any)["path"].(string))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("paths = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestSearchesHonorGitignoreWithoutAGitDirectory(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, ".gitignore", "ignored.txt\n")
	writeFixture(t, workspace, "ignored.txt", "needle\n")
	writeFixture(t, workspace, "visible.txt", "needle\n")
	session := fileToolSession(t, workspace)
	for _, name := range []string{"glob", "grep"} {
		args := map[string]any{"pattern": "*.txt"}
		if name == "grep" {
			args["pattern"] = "needle"
		}
		result := fileToolCall(t, session, name, args)
		if result.IsError {
			t.Fatalf("%s failed: %#v", name, result.Content)
		}
		items := result.StructuredContent.(map[string]any)["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["path"] != "visible.txt" {
			t.Fatalf("%s returned ignored files: %#v", name, items)
		}
	}
}

func TestApplyPatchParsesGitBoundariesAndLiteralHeaders(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "-- old\ntail\n")
	writeFixture(t, workspace, "b.txt", "old\n")
	patch := "diff --git a/a.txt b/a.txt\nindex 123..456 100644\n--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n--- old\n+++ new\n tail\n" +
		"diff --git a/b.txt b/b.txt\nindex 123..456 100644\n--- a/b.txt\n+++ b/b.txt\n@@ -1 +1 @@\n-old\n+new\n"
	result := fileToolCall(t, fileToolSession(t, workspace), "apply_patch", map[string]any{"patch": patch})
	if result.IsError {
		t.Fatalf("multi-file Git patch failed: %#v", result.Content)
	}
	for path, want := range map[string]string{"a.txt": "++ new\ntail\n", "b.txt": "new\n"} {
		got, err := os.ReadFile(filepath.Join(workspace, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestApplyPatchDistinguishesTransportAndFileNewlines(t *testing.T) {
	for _, tc := range []struct {
		name, original, patch, want string
	}{
		{"add", "", "--- /dev/null\n+++ file.txt\n@@ -0,0 +1 @@\n+new", "new\n"},
		{"modify", "old\n", "--- file.txt\n+++ file.txt\n@@ -1 +1 @@\n-old\n+new", "new\n"},
		{"explicit marker", "old", "--- file.txt\n+++ file.txt\n@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file", "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if tc.original != "" {
				writeFixture(t, workspace, "file.txt", tc.original)
			}
			result := fileToolCall(t, fileToolSession(t, workspace), "apply_patch", map[string]any{"patch": tc.patch})
			if result.IsError {
				t.Fatalf("patch failed: %#v", result.Content)
			}
			got, err := os.ReadFile(filepath.Join(workspace, "file.txt"))
			if err != nil || string(got) != tc.want {
				t.Fatalf("file = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestApplyPatchRejectsBinaryOutputBeforeWritingAnyFile(t *testing.T) {
	for _, modify := range []bool{false, true} {
		t.Run(fmt.Sprintf("modify=%t", modify), func(t *testing.T) {
			workspace := t.TempDir()
			writeFixture(t, workspace, "keep.txt", "keep\n")
			oldPath, body, header := "/dev/null", "", "@@ -0,0 +1 @@\n"
			if modify {
				writeFixture(t, workspace, "binary.txt", "old\n")
				oldPath, body, header = "binary.txt", "-old\n", "@@ -1 +1 @@\n"
			}
			patch := "--- keep.txt\n+++ keep.txt\n@@ -1 +1 @@\n-keep\n+changed\n" +
				"--- " + oldPath + "\n+++ binary.txt\n" + header + body + "+bad\x00content\n"
			result := fileToolCall(t, fileToolSession(t, workspace), "apply_patch", map[string]any{"patch": patch})
			if !result.IsError || !strings.Contains(result.Content[0].(*sdk.TextContent).Text, "[1201]") {
				t.Fatalf("binary output was accepted: %#v", result)
			}
			data, err := os.ReadFile(filepath.Join(workspace, "keep.txt"))
			if err != nil || string(data) != "keep\n" {
				t.Fatalf("precheck changed first file: %q, %v", data, err)
			}
			data, err = os.ReadFile(filepath.Join(workspace, "binary.txt"))
			if modify {
				if err != nil || string(data) != "old\n" {
					t.Fatalf("precheck changed binary target: %q, %v", data, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("binary target created: %q, %v", data, err)
			}
		})
	}
}

func TestApplyPatchPreservesLimitedMatchingProfiles(t *testing.T) {
	for _, tc := range []struct {
		original, expected string
		accept             bool
	}{
		{" \told\t \n", "old\n", true},
		{"old\u2003\n", "old\n", false},
		{"\u3000old\n", "old\n", false},
		{"old\u00a0\n", "old\n", false},
		{"left\u00a0right\n", "left right\n", true},
		{"old\n", "old", false},
	} {
		t.Run(fmt.Sprintf("%q/%q", tc.original, tc.expected), func(t *testing.T) {
			workspace := t.TempDir()
			writeFixture(t, workspace, "file.txt", tc.original)
			patch := "--- file.txt\n+++ file.txt\n@@ -99 +99 @@\n-" + tc.expected
			if !strings.HasSuffix(tc.expected, "\n") {
				patch += "\n\\ No newline at end of file\n"
			}
			patch += "+changed\n"
			_, err := ApplyPatch(context.Background(), workspace, patch)
			if (err == nil) != tc.accept {
				t.Fatalf("accept=%t, err=%v", tc.accept, err)
			}
			got, err := os.ReadFile(filepath.Join(workspace, "file.txt"))
			want := tc.original
			if tc.accept {
				want = "changed\n"
			}
			if err != nil || string(got) != want {
				t.Fatalf("file = %q, %v; want %q", got, err, want)
			}
		})
	}
	for _, punctuation := range []string{"‘", "’", "‚", "‛", "“", "”", "„", "‟", "‐", "‑", "‒", "–", "—", "―", "…"} {
		t.Run(punctuation, func(t *testing.T) {
			workspace := t.TempDir()
			writeFixture(t, workspace, "file.txt", "left"+punctuation+"right\n")
			replacement := "-"
			if strings.Contains("‘’‚‛", punctuation) {
				replacement = "'"
			}
			if strings.Contains("“”„‟", punctuation) {
				replacement = "\""
			}
			if punctuation == "…" {
				replacement = "..."
			}
			patch := "--- file.txt\n+++ file.txt\n@@ -1 +1 @@\n-left" + replacement + "right\n+new-side text\n"
			if _, err := ApplyPatch(context.Background(), workspace, patch); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResponsesTruncateWholeRecordsAndContinue(t *testing.T) {
	workspace := t.TempDir()
	line := strings.Repeat("x", 180_000)
	writeFixture(t, workspace, "large.txt", strings.Repeat(line+"\n", 4))
	session := fileToolSession(t, workspace)
	for _, name := range []string{"read_file", "grep"} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"path": "large.txt"}
			key := "lines"
			if name == "grep" {
				args["pattern"], key = "x+", "items"
			}
			result := fileToolCall(t, session, name, args)
			if result.IsError {
				t.Fatalf("large page failed: %#v", result.Content)
			}
			encoded, err := json.Marshal(result)
			if err != nil || len(encoded) > maxResponseBytes {
				t.Fatalf("encoded length=%d, err=%v", len(encoded), err)
			}
			structured := result.StructuredContent.(map[string]any)
			items := structured[key].([]any)
			if len(items) == 0 || len(items) >= 4 || structured["truncated"] != true {
				t.Fatalf("truncation metadata = %#v", structured["truncated"])
			}
			next := int(structured["next_offset"].(float64))
			want := len(items)
			if name == "read_file" {
				want++
			}
			if next != want {
				t.Fatalf("next=%d, want=%d", next, want)
			}
			args["offset"] = next
			following := fileToolCall(t, session, name, args)
			if following.IsError {
				t.Fatalf("continuation failed: %#v", following.Content)
			}
			first := following.StructuredContent.(map[string]any)[key].([]any)[0].(map[string]any)
			wantLine := next
			if name == "grep" {
				wantLine++
			}
			if first["line"] != float64(wantLine) || first["text"] != line {
				t.Fatalf("continuation starts at line %v, want %d", first["line"], wantLine)
			}
		})
	}
	writeFixture(t, workspace, "oversized.txt", strings.Repeat("x", maxResponseBytes)+"\n")
	for _, name := range []string{"read_file", "grep"} {
		args := map[string]any{"path": "oversized.txt"}
		if name == "grep" {
			args["pattern"] = "x+"
		}
		if result := fileToolCall(t, session, name, args); !result.IsError {
			t.Fatalf("%s accepted an oversized record", name)
		}
	}
}
