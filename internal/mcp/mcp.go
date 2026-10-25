package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	mcpconfig "github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolCallStatus string

const (
	ToolCallSuceessStatus = "success"
	ToolCallFailedStatus  = "failed"
)

// Config contains settings shared by the tools registered on an MCP server.
type Config struct {
	Workspace string
	Timeout   time.Duration
	// RuntimeDirs are host-authorized read-only directories for exec; tool
	// arguments cannot add mounts or change the controlled executable PATH.
	RuntimeDirs []string
}

type ReadFileLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type ReadFileResult struct {
	Lines      []ReadFileLine `json:"lines"`
	Truncated  bool           `json:"truncated"`
	NextOffset *int           `json:"next_offset"`
}

// RegisterTools registers workspace tools on server. The workspace is resolved
// once so all relative paths share the same root.
func RegisterTools(server *sdk.Server, config Config) error {
	workspace, err := resolveWorkspace(config.Workspace)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	// MCP server 提供全部内置工具；Agent 的允许列表在调用方控制。
	type readFileArgs struct {
		Path   string `json:"path" jsonschema:"existing UTF-8 text file relative to the workspace; absolute paths are rejected"`
		Offset *int   `json:"offset,omitempty" jsonschema:"positive one-based starting line (default 1); use next_offset to continue; use a JSON integer such as 1, never a quoted numeric string"`
		Limit  *int   `json:"limit,omitempty" jsonschema:"positive maximum number of lines (default 200); response size may reduce the page; use a JSON integer such as 200, never a quoted numeric string"`
	}
	sdk.AddTool(server, &sdk.Tool{Name: string(mcpconfig.ReadFileMCPToolName), InputSchema: toolInputSchema[readFileArgs](), Description: `Read a page of lines from an existing UTF-8 text file (NUL bytes are rejected). Arguments must match the declared JSON types: strings stay strings, integers are unquoted numbers, and booleans are true or false, never quoted strings. Example arguments: {"path":"README.md","offset":1,"limit":200}. path is relative to the workspace; absolute paths and paths resolving outside it are rejected. Symlinks are allowed only when their resolved target stays inside the workspace. offset is the one-based starting line (default 1); limit is a positive maximum line count (default 200). Returns lines with their original one-based line numbers and text, plus truncated and next_offset. An offset past EOF returns an empty page. Responses may contain fewer than limit lines to fit the 1 MiB response cap; when truncated is true, pass next_offset as offset with the same path to continue.`},
		func(ctx context.Context, _ *sdk.CallToolRequest, args readFileArgs) (*sdk.CallToolResult, ReadFileResult, error) {
			toolCtx, cancel := context.WithTimeout(ctx, config.Timeout)
			defer cancel()
			result, err := ReadFile(toolCtx, workspace, args.Path, intDefault(args.Offset, 1), intDefault(args.Limit, 200))
			if err != nil {
				return nil, ReadFileResult{}, err
			}
			return pageResponse(result.Lines, intDefault(args.Offset, 1), result.Truncated, result.NextOffset,
				func(lines []ReadFileLine, truncated bool, next *int) ReadFileResult {
					return ReadFileResult{Lines: lines, Truncated: truncated, NextOffset: next}
				},
				func(line ReadFileLine) string { return fmt.Sprintf("%d: %s", line.Line, line.Text) })
		})
	if err := registerListDirTool(server, workspace, config.Timeout); err != nil {
		return err
	}
	registerGlobTool(server, workspace, config.Timeout)
	registerGrepTool(server, workspace, config.Timeout)
	registerApplyPatchTool(server, workspace, config.Timeout)
	registerExecTool(server, workspace, config)
	registerSwitchToPlanExecuteTool(server)
	return nil
}

func ReadFile(ctx context.Context, workspace, path string, offset, limit int) (ReadFileResult, error) {
	if offset < 1 || limit < 1 {
		return ReadFileResult{}, fmt.Errorf("offset and limit must be positive")
	}
	resolved, _, err := resolveWorkspacePath(workspace, path, resolveSymlinks, false)
	if err != nil {
		return ReadFileResult{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return ReadFileResult{}, err
	}
	if !info.Mode().IsRegular() {
		return ReadFileResult{}, fmt.Errorf("path is not a regular file")
	}
	contents, err := os.ReadFile(resolved)
	if err != nil {
		return ReadFileResult{}, err
	}
	if bytes.IndexByte(contents, 0) >= 0 || !utf8.Valid(contents) {
		return ReadFileResult{}, fmt.Errorf("file is not UTF-8 text")
	}
	// sed is invoked with an argument vector, so the path is never interpreted
	// by a shell. Its output is split into lines and numbered from offset.
	cmd := exec.CommandContext(ctx, "sed", "-n", fmt.Sprintf("%d,%dp", offset, offset+limit), "--", resolved)
	output, err := cmd.Output()
	if err != nil {
		return ReadFileResult{}, commandError("sed read failed", err, "")
	}
	text := strings.TrimSuffix(string(output), "\n")
	lines := make([]ReadFileLine, 0)
	if len(output) > 0 {
		values := strings.Split(text, "\n")
		if len(values) > limit {
			values = values[:limit]
		}
		for i, value := range values {
			lines = append(lines, ReadFileLine{Line: offset + i, Text: value})
		}
	}
	result := ReadFileResult{Lines: lines}
	if len(lines) == limit {
		lookahead := exec.CommandContext(ctx, "sed", "-n", fmt.Sprintf("%dp", offset+limit), "--", resolved)
		following, lookErr := lookahead.Output()
		if lookErr != nil {
			return ReadFileResult{}, commandError("sed read failed", lookErr, "")
		}
		if len(following) > 0 {
			next := offset + len(lines)
			result.Truncated = true
			result.NextOffset = &next
		}
	}
	return result, nil
}
