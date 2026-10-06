package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	mcpconfig "github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type grepItem struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type grepResult struct {
	Items      []grepItem `json:"items"`
	Truncated  bool       `json:"truncated"`
	NextOffset *int       `json:"next_offset"`
}

type grepArgs struct {
	Pattern        string `json:"pattern" jsonschema:"non-empty ripgrep regular expression; interpreted as fixed text when literal=true"`
	Path           string `json:"path,omitempty" jsonschema:"existing file or directory relative to the workspace (default .); directories are searched recursively"`
	Glob           string `json:"glob,omitempty" jsonschema:"optional ripgrep file-path glob evaluated from the workspace root; leading ! excludes matches"`
	Literal        bool   `json:"literal,omitempty" jsonschema:"treat pattern as fixed text rather than a regular expression (default false)"`
	IgnoreCase     bool   `json:"ignore_case,omitempty" jsonschema:"use case-insensitive matching (default false)"`
	IncludeIgnored bool   `json:"include_ignored,omitempty" jsonschema:"disable ignore-file filtering during traversal (default false); .git entries and symlinks remain excluded"`
	Offset         int    `json:"offset,omitempty" jsonschema:"non-negative zero-based matching-line index (default 0); use next_offset to continue"`
	Limit          *int   `json:"limit,omitempty" jsonschema:"positive maximum number of matching lines (default 200); response size may reduce the page"`
}

// registerGrepTool registers the grep tool against an already resolved workspace.
// It is kept separate so startup registration can compose the individual tools.
func registerGrepTool(server *sdk.Server, workspace string, timeout time.Duration) {
	sdk.AddTool(server, &sdk.Tool{Name: string(mcpconfig.GrepMCPToolName), Description: `Search file contents for a non-empty pattern using ripgrep. path is an existing workspace-relative file or directory (default .); directory searches are recursive. Absolute paths, paths outside the workspace, and symlink path components are rejected. pattern is a ripgrep regular expression by default; set literal=true for fixed text. Matching is case-sensitive unless ignore_case=true. glob optionally filters file paths using ripgrep glob syntax evaluated from the workspace root; it is not a content pattern. Includes hidden files, does not follow symlinks, and excludes .git entries. Directory traversal respects ignore files unless include_ignored=true; an explicitly named file can be searched even when ignored. Returns one item per matching line with workspace-relative path, one-based line number, and line text, sorted by path then line; no matches returns an empty page. offset is a zero-based matching-line index (default 0), not a source line number; limit is a positive maximum item count (default 200). If truncated is true, repeat the same search with offset=next_offset. The 1 MiB response cap may shorten a page.`},
		func(ctx context.Context, _ *sdk.CallToolRequest, args grepArgs) (*sdk.CallToolResult, grepResult, error) {
			if args.Pattern == "" {
				return nil, grepResult{}, fmt.Errorf("pattern is required")
			}
			if args.Path == "" {
				args.Path = "."
			}
			toolCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			result, err := Grep(toolCtx, workspace, args)
			if err != nil {
				return nil, grepResult{}, err
			}
			return pageResponse(result.Items, args.Offset, result.Truncated, result.NextOffset,
				func(items []grepItem, truncated bool, next *int) grepResult {
					return grepResult{Items: items, Truncated: truncated, NextOffset: next}
				},
				func(item grepItem) string { return fmt.Sprintf("%s:%d: %s", item.Path, item.Line, item.Text) })
		})
}

func Grep(ctx context.Context, workspace string, args grepArgs) (grepResult, error) {
	limit := intDefault(args.Limit, 200)
	if args.Offset < 0 || limit < 1 {
		return grepResult{}, fmt.Errorf("offset must be non-negative and limit must be positive")
	}
	rel, err := grepTarget(workspace, args.Path)
	if err != nil {
		return grepResult{}, err
	}
	if isGitPath(rel) {
		return grepResult{Items: []grepItem{}}, nil
	}
	var eligible map[string]bool
	if args.Glob != "" && !args.IncludeIgnored {
		eligible, err = searchableFiles(ctx, workspace, rel)
		if err != nil {
			return grepResult{}, err
		}
	}

	commandArgs := []string{"--json", "--hidden", "--no-follow", "--no-require-git", "--sort", "path"}
	if args.IncludeIgnored {
		commandArgs = append(commandArgs, "--no-ignore")
	}
	if args.Literal {
		commandArgs = append(commandArgs, "--fixed-strings")
	}
	if args.IgnoreCase {
		commandArgs = append(commandArgs, "--ignore-case")
	}
	if args.Glob != "" {
		commandArgs = append(commandArgs, "--glob", args.Glob)
	}
	commandArgs = append(commandArgs, "--glob", "!.git", "--glob", "!**/.git/**", "--", args.Pattern, rel)

	cmd := exec.CommandContext(ctx, "rg", commandArgs...)
	cmd.Dir = workspace
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return grepResult{}, fmt.Errorf("rg search failed: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return grepResult{}, fmt.Errorf("rg search failed: %w", err)
	}

	items := make([]grepItem, 0)
	scanner := bufio.NewScanner(stdout)
	// A matching line may itself be larger than Scanner's default token limit.
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		var event struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				Lines struct {
					Text string `json:"text"`
				} `json:"lines"`
				LineNumber int `json:"line_number"`
			} `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			_ = cmd.Wait()
			return grepResult{}, fmt.Errorf("rg returned invalid JSON: %w", err)
		}
		if event.Type != "match" {
			continue
		}
		path := filepath.ToSlash(filepath.Clean(event.Data.Path.Text))
		if isGitPath(path) || (eligible != nil && !eligible[filepath.Clean(path)]) {
			continue
		}
		items = append(items, grepItem{Path: path, Line: event.Data.LineNumber, Text: strings.TrimSuffix(strings.TrimSuffix(event.Data.Lines.Text, "\n"), "\r")})
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Wait()
		return grepResult{}, fmt.Errorf("rg output failed: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			return grepResult{}, fmt.Errorf("rg search failed: %w", err)
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		return items[i].Line < items[j].Line
	})
	selected, truncated, next := selectPage(items, args.Offset, limit)
	return grepResult{Items: selected, Truncated: truncated, NextOffset: next}, nil
}

func grepTarget(workspace, path string) (string, error) {
	resolved, rel, err := resolveWorkspacePath(workspace, path, rejectSymlinks, false)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", fmt.Errorf("path is not a regular file or directory")
	}
	return filepath.ToSlash(rel), nil
}
