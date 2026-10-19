package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	mcpconfig "github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ListDirItem struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

type ListDirResult struct {
	Items      []ListDirItem `json:"items"`
	Truncated  bool          `json:"truncated"`
	NextOffset *int          `json:"next_offset"`
}

type listDirArgs struct {
	Path   string `json:"path,omitempty" jsonschema:"existing directory relative to the workspace (default .); absolute paths are rejected"`
	Depth  *int   `json:"depth,omitempty" jsonschema:"positive traversal depth from the requested directory (default 1: immediate children); use a JSON integer such as 1, never a quoted numeric string"`
	Offset int    `json:"offset,omitempty" jsonschema:"non-negative zero-based entry index (default 0); use next_offset to continue; use a JSON integer such as 0, never a quoted numeric string"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"positive maximum number of entries (default 200); use a JSON integer such as 200, never a quoted numeric string; response size may reduce the page"`
}

// registerListDirTool registers list_dir against a shared workspace root.
func registerListDirTool(server *sdk.Server, workspace string, timeout time.Duration) error {
	workspace, err := resolveWorkspace(workspace)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	sdk.AddTool(server, &sdk.Tool{Name: string(mcpconfig.ListDirMCPToolName), InputSchema: toolInputSchema[listDirArgs](), Description: `List entries under an existing workspace directory, excluding the directory itself. path defaults to . and must be workspace-relative; absolute paths and paths resolving outside the workspace are rejected. The input directory may resolve through symlinks within the workspace, but traversal does not follow descendant symlinks. depth is a positive traversal depth (default 1: immediate children only). Numeric arguments must be JSON integers, never quoted numeric strings. Example arguments: {"path":".","depth":1,"offset":0,"limit":200}. Includes hidden and ignored entries, but excludes entries named .git and their descendants. Returns workspace-relative paths sorted lexicographically and types file, directory, symlink, or other. offset is a zero-based entry index (default 0); limit is a positive maximum entry count (default 200). truncated and next_offset indicate pagination; reuse the same path and depth with offset=next_offset to continue. The 1 MiB response cap may shorten a page; an offset beyond the results returns an empty page.`},
		func(ctx context.Context, _ *sdk.CallToolRequest, args listDirArgs) (*sdk.CallToolResult, ListDirResult, error) {
			toolCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			result, err := ListDir(toolCtx, workspace, args.Path, intDefault(args.Depth, 1), args.Offset, intDefault(args.Limit, 200))
			if err != nil {
				return nil, ListDirResult{}, err
			}
			return pageResponse(result.Items, args.Offset, result.Truncated, result.NextOffset,
				func(items []ListDirItem, truncated bool, next *int) ListDirResult {
					return ListDirResult{Items: items, Truncated: truncated, NextOffset: next}
				},
				func(item ListDirItem) string { return fmt.Sprintf("%s\t%s", item.Path, item.Type) })
		})
	return nil
}

func ListDir(ctx context.Context, workspace, path string, depth, offset, limit int) (ListDirResult, error) {
	if path == "" {
		path = "."
	}
	if depth < 1 {
		return ListDirResult{}, fmt.Errorf("depth must be at least 1")
	}
	if offset < 0 || limit < 1 {
		return ListDirResult{}, fmt.Errorf("offset must be non-negative and limit must be positive")
	}
	base, _, err := resolveWorkspacePath(workspace, path, resolveSymlinks, false)
	if err != nil {
		return ListDirResult{}, err
	}
	info, err := os.Stat(base)
	if err != nil {
		return ListDirResult{}, err
	}
	if !info.IsDir() {
		return ListDirResult{}, fmt.Errorf("path is not a directory")
	}

	args := []string{base, "-name", ".git", "-prune", "-o", "-mindepth", "1", "-maxdepth", strconv.Itoa(depth), "-print0"}
	output, err := exec.CommandContext(ctx, "find", args...).Output()
	if err != nil {
		return ListDirResult{}, fmt.Errorf("find list failed: %w", err)
	}
	paths := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(paths) == 1 && paths[0] == "" {
		paths = nil
	}
	sort.Strings(paths)
	items := make([]ListDirItem, 0, len(paths))
	for _, found := range paths {
		foundRel, err := relativeWorkspacePath(workspace, found)
		if err != nil {
			return ListDirResult{}, err
		}
		itemInfo, err := os.Lstat(found)
		if err != nil {
			return ListDirResult{}, err
		}
		itemType := "other"
		if itemInfo.Mode()&os.ModeSymlink != 0 {
			itemType = "symlink"
		} else if itemInfo.IsDir() {
			itemType = "directory"
		} else if itemInfo.Mode().IsRegular() {
			itemType = "file"
		}
		items = append(items, ListDirItem{Path: filepath.ToSlash(foundRel), Type: itemType})
	}
	selected, truncated, next := selectPage(items, offset, limit)
	return ListDirResult{Items: selected, Truncated: truncated, NextOffset: next}, nil
}
