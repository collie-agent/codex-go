package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	mcpconfig "github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type globArgs struct {
	Pattern        string `json:"pattern" jsonschema:"non-empty ripgrep glob relative to path; *.go matches immediate files and **/*.go matches recursively; leading ! excludes matches"`
	Path           string `json:"path,omitempty" jsonschema:"existing directory relative to the workspace (default .); symlink components are rejected"`
	IncludeIgnored bool   `json:"include_ignored,omitempty" jsonschema:"disable ignore-file filtering (default false); .git entries and symlinks remain excluded"`
	Offset         int    `json:"offset,omitempty" jsonschema:"non-negative zero-based result index (default 0); use next_offset to continue"`
	Limit          *int   `json:"limit,omitempty" jsonschema:"positive maximum number of results (default 200); response size may reduce the page"`
}

type GlobItem struct {
	Path string `json:"path"`
}

type GlobResult struct {
	Items      []GlobItem `json:"items"`
	Truncated  bool       `json:"truncated"`
	NextOffset *int       `json:"next_offset"`
}

// registerGlobTool registers the glob search tool against a workspace root.
func registerGlobTool(server *sdk.Server, workspace string, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	sdk.AddTool(server, &sdk.Tool{Name: string(mcpconfig.GlobMCPToolName), Description: `Find file paths, rather than file contents, using a ripgrep glob under an existing directory. path defaults to . and must be workspace-relative; absolute paths, paths outside the workspace, and symlink path components are rejected. Positive patterns are anchored to the search directory: *.go matches its immediate files, while **/*.go matches recursively. A leading ! excludes matching paths instead of selecting them. Includes hidden files, skips symlinks and .git entries, and respects ignore files unless include_ignored=true. Returns lexicographically sorted workspace-relative file paths, not directories. offset is a zero-based result index (default 0); limit is a positive maximum result count (default 200). No matches returns an empty page. If truncated is true, repeat the same search with offset=next_offset; the 1 MiB response cap may shorten a page.`},
		func(ctx context.Context, _ *sdk.CallToolRequest, args globArgs) (*sdk.CallToolResult, GlobResult, error) {
			toolCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			result, err := Glob(toolCtx, workspace, args)
			if err != nil {
				return nil, GlobResult{}, err
			}
			return pageResponse(result.Items, args.Offset, result.Truncated, result.NextOffset,
				func(items []GlobItem, truncated bool, next *int) GlobResult {
					return GlobResult{Items: items, Truncated: truncated, NextOffset: next}
				},
				func(item GlobItem) string { return item.Path })
		})
}

func Glob(ctx context.Context, workspace string, args globArgs) (GlobResult, error) {
	if args.Pattern == "" {
		return GlobResult{}, errors.New("pattern is required")
	}
	if args.Path == "" {
		args.Path = "."
	}
	limit := intDefault(args.Limit, 200)
	if args.Offset < 0 || limit < 1 {
		return GlobResult{}, errors.New("offset must be non-negative and limit must be positive")
	}
	base, relDir, err := resolveWorkspacePath(workspace, args.Path, rejectSymlinks, false)
	if err != nil {
		return GlobResult{}, err
	}
	info, err := os.Stat(base)
	if err != nil {
		return GlobResult{}, err
	}
	if !info.IsDir() {
		return GlobResult{}, errors.New("path is not a directory")
	}
	if isGitPath(relDir) {
		return GlobResult{Items: []GlobItem{}}, nil
	}
	cmdArgs := []string{"--files", "--null", "--hidden", "--no-follow", "--no-require-git"}
	if args.IncludeIgnored {
		cmdArgs = append(cmdArgs, "--no-ignore")
	}
	// Run from the search directory so rg evaluates the pattern relative to it.
	// Anchor positive patterns to preserve directory-relative matching for '*'.
	pattern := args.Pattern
	if !strings.HasPrefix(pattern, "!") && !strings.HasPrefix(pattern, "/") {
		pattern = "/" + pattern
	}
	var eligible map[string]bool
	if !args.IncludeIgnored {
		// Positive rg globs override ignore files. Intersect their matches with
		// an ordinary traversal to preserve the default ignore contract.
		eligible, err = searchableFiles(ctx, base, ".")
		if err != nil {
			return GlobResult{}, err
		}
	}
	cmdArgs = append(cmdArgs, "--glob", pattern, "--glob", "!.git", "--glob", "!**/.git/**", "--", ".")
	paths, err := rgFiles(ctx, base, cmdArgs)
	if err != nil {
		return GlobResult{}, err
	}
	items := make([]GlobItem, 0, len(paths))
	for _, path := range paths {
		if eligible != nil && !eligible[filepath.Clean(path)] {
			continue
		}
		path = filepath.ToSlash(filepath.Join(relDir, path))
		if isGitPath(path) {
			continue
		}
		items = append(items, GlobItem{Path: path})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	selected, truncated, next := selectPage(items, args.Offset, limit)
	return GlobResult{Items: selected, Truncated: truncated, NextOffset: next}, nil
}
