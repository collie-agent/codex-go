package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSearchToolReportsStableQueryEvidenceAcrossLimits(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "needle\n")
	session := fileToolSession(t, workspace)
	first := fileToolCall(t, session, "grep", map[string]any{"pattern": "needle", "limit": 10})
	second := fileToolCall(t, session, "grep", map[string]any{"pattern": "needle", "path": "./", "limit": 20})
	a, ok := first.StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	if !ok {
		t.Fatal("grep response lacks execution-time search evidence")
	}
	b := second.StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	if a["query"] != b["query"] || a["version"] == "" || a["version"] != b["version"] {
		t.Fatalf("unstable query evidence: %#v %#v", a, b)
	}
	positions := a["positions"].([]any)
	if len(positions) != 1 || positions[0] != "a.txt:1" || a["offset"] != float64(0) || a["eof"] != true {
		t.Fatalf("wrong actual page evidence: %#v", a)
	}
}

func TestGlobToolEvidenceTracksActualPagesAndSourceChanges(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "a\n")
	writeFixture(t, workspace, "b.txt", "b\n")
	session := fileToolSession(t, workspace)
	first := fileToolCall(t, session, "glob", map[string]any{"pattern": "*.txt", "limit": 1})
	a, ok := first.StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	if !ok {
		t.Fatal("glob response lacks execution-time search evidence")
	}
	if a["eof"] != false || a["positions"].([]any)[0] != "a.txt" {
		t.Fatalf("wrong first page: %#v", a)
	}
	next := fileToolCall(t, session, "glob", map[string]any{"pattern": "*.txt", "offset": 1, "limit": 1}).StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	if next["query"] != a["query"] || next["version"] != a["version"] || next["eof"] != true || next["positions"].([]any)[0] != "b.txt" {
		t.Fatalf("wrong next page: %#v", next)
	}
	writeFixture(t, workspace, "c.txt", "c\n")
	changed := fileToolCall(t, session, "glob", map[string]any{"pattern": "*.txt"}).StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	if changed["version"] == a["version"] {
		t.Fatal("new path did not update source version")
	}
}

func TestGlobSourceVersionIgnoresFileContents(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, "a.txt", "before\n")
	session := fileToolSession(t, workspace)
	first := fileToolCall(t, session, "glob", map[string]any{"pattern": "*.txt"}).StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	writeFixture(t, workspace, "a.txt", "after\n")
	second := fileToolCall(t, session, "glob", map[string]any{"pattern": "*.txt"}).StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	if first["version"] != second["version"] {
		t.Fatal("glob path evidence changed after content-only mutation")
	}
}

func TestSearchSnapshotSkipsIgnoredFilesAndBoundsExtraReads(t *testing.T) {
	workspace := t.TempDir()
	writeFixture(t, workspace, ".gitignore", "vendor/\n")
	writeFixture(t, workspace, "a.txt", "needle\n")
	writeFixture(t, workspace, "vendor/large.txt", "")
	large, err := os.OpenFile(filepath.Join(workspace, "vendor/large.txt"), os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(17 << 20); err != nil {
		t.Fatal(err)
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}
	session := fileToolSession(t, workspace)
	regular := fileToolCall(t, session, "grep", map[string]any{"pattern": "needle"}).StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	if regular["version"] == "" {
		t.Fatal("ignored large file consumed snapshot budget")
	}
	included := fileToolCall(t, session, "grep", map[string]any{"pattern": "needle", "include_ignored": true}).StructuredContent.(map[string]any)["search_evidence"].(map[string]any)
	if included["version"] != "" {
		t.Fatal("oversized scope must explicitly report unknown version")
	}
}
