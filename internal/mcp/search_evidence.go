package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SearchEvidence describes the query and the actual page observed during execution.
// An empty Version explicitly means the source snapshot could not be confirmed.
type SearchEvidence struct {
	Query     string   `json:"query"`
	Version   string   `json:"version"`
	Offset    int      `json:"offset"`
	Positions []string `json:"positions"`
	EOF       bool     `json:"eof"`
}

func searchQuery(tool, path string, args any) string {
	data, _ := json.Marshal(args)
	var query map[string]any
	_ = json.Unmarshal(data, &query)
	delete(query, "limit")
	delete(query, "offset")
	query["path"] = filepath.ToSlash(filepath.Clean(path))
	query["tool"] = tool
	data, _ = json.Marshal(query)
	return string(data)
}

// Snapshot before searching and verify afterwards. Never label a post-search
// read alone as the version searched by rg; a changing/unreadable tree is unknown.
func searchSnapshot(ctx context.Context, workspace, scope string, includeIgnored, contents bool) string {
	hash := sha256.New()
	info, err := os.Stat(filepath.Join(workspace, scope))
	if err != nil {
		return ""
	}
	paths := []string{scope}
	if info.IsDir() {
		args := []string{"--files", "--null", "--hidden", "--no-follow", "--no-require-git", "--glob", "!.git", "--glob", "!**/.git/**"}
		if includeIgnored {
			args = append(args, "--no-ignore")
		}
		args = append(args, "--", scope)
		paths, err = rgFiles(ctx, workspace, args)
		if err != nil {
			return ""
		}
	}
	sort.Strings(paths)
	// Bound the extra I/O. Large or changing scopes remain conservatively unknown.
	remaining := int64(16 << 20)
	for _, path := range paths {
		if ctx.Err() != nil {
			return ""
		}
		fmt.Fprintf(hash, "%s\x00", filepath.ToSlash(filepath.Clean(path)))
		if !contents {
			continue
		}
		file, err := os.Open(filepath.Join(workspace, path))
		if err != nil {
			return ""
		}
		n, err := io.Copy(hash, io.LimitReader(file, remaining+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil || n > remaining {
			return ""
		}
		remaining -= n
		_, _ = io.WriteString(hash, "\x00")
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func confirmedSearchVersion(before, after string) string {
	if before != "" && before == after {
		return before
	}
	return ""
}

func grepPageEvidence(base *SearchEvidence, items []grepItem, truncated bool) *SearchEvidence {
	if base == nil {
		return nil
	}
	page := *base
	page.EOF = !truncated
	page.Positions = make([]string, 0, len(items))
	for _, item := range items {
		page.Positions = append(page.Positions, fmt.Sprintf("%s:%d", item.Path, item.Line))
	}
	return &page
}

func globPageEvidence(base *SearchEvidence, items []GlobItem, truncated bool) *SearchEvidence {
	if base == nil {
		return nil
	}
	page := *base
	page.EOF = !truncated
	page.Positions = make([]string, 0, len(items))
	for _, item := range items {
		page.Positions = append(page.Positions, strings.TrimPrefix(item.Path, "./"))
	}
	return &page
}
