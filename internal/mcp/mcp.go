package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
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
	ExecGoProxy string
	ExecCaches  []mcpconfig.ExecCache
	// ExecNetwork is none (default) or host; tool arguments cannot change it.
	ExecNetwork string
	// ExecEnv authorizes host variable names, resolved once when exec is registered.
	ExecEnv   []string
	Workspace string
	Timeout   time.Duration
	// RuntimeDirs are host-authorized read-only directories for exec; tool
	// arguments cannot add mounts or change the controlled executable PATH.
	RuntimeDirs []string
	// DiscoverHostCommands opts into initialization-time discovery from host PATH.
	DiscoverHostCommands bool
	// ExecCommands adds entrypoints to the built-in whitelist.
	ExecCommands []string
}

type ReadFileLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// FileEvidence describes exactly the snapshot and lines returned to the caller.
type FileEvidence struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type ReadFileResult struct {
	Evidence   *FileEvidence  `json:"evidence,omitempty"`
	Lines      []ReadFileLine `json:"lines"`
	Truncated  bool           `json:"truncated"`
	NextOffset *int           `json:"next_offset"`
}

// RegisterTools registers workspace tools on server. The workspace is resolved
// once so all relative paths share the same root.
func RegisterTools(server *sdk.Server, config Config) error {
	_, err := RegisterToolsWithCleanup(server, config)
	return err
}

// RegisterToolsWithCleanup lets the execution owner await process cleanup
// before returning a final result. Disconnect also triggers cleanup.
func RegisterToolsWithCleanup(server *sdk.Server, config Config) (func(), error) {
	workspace, err := resolveWorkspace(config.Workspace)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Minute
	}
	// MCP server 提供全部内置工具；Agent 的允许列表在调用方控制。
	type readFileArgs struct {
		Path   string `json:"path" jsonschema:"existing UTF-8 text file relative to the workspace; absolute paths are rejected"`
		Offset *int   `json:"offset,omitempty" jsonschema:"positive one-based starting line (default 1); use next_offset to continue; use a JSON integer such as 1, never a quoted numeric string"`
		Limit  *int   `json:"limit,omitempty" jsonschema:"positive maximum number of lines (default 200); response size may reduce the page; use a JSON integer such as 200, never a quoted numeric string"`
	}
	sdk.AddTool(server, &sdk.Tool{Name: string(mcpconfig.ReadFileMCPToolName), InputSchema: toolInputSchema[readFileArgs](), Description: `Read a page of lines from an existing UTF-8 text file (NUL bytes are rejected). Arguments must match the declared JSON types: strings stay strings, integers are unquoted numbers, and booleans are true or false, never quoted strings. Example arguments: {"path":"README.md","offset":1,"limit":200}. path is relative to the workspace; absolute paths and paths resolving outside it are rejected. Symlinks are allowed only when their resolved target stays inside the workspace. offset is the one-based starting line (default 1); limit is a positive maximum line count (default 200). Returns lines with their original one-based line numbers and text, plus truncated and next_offset. An offset past EOF returns an empty page. Responses may contain fewer than limit lines to fit the 1 MiB response cap; when truncated is true, pass next_offset as offset with the same path to continue.`},
		func(ctx context.Context, req *sdk.CallToolRequest, args readFileArgs) (*sdk.CallToolResult, ReadFileResult, error) {
			toolCtx, cancel := context.WithTimeout(ctx, config.Timeout)
			defer cancel()
			links := resolveSymlinks
			if req.Params.Meta[HandoffTargetFileMetaKey] == true {
				links = rejectSymlinks
			}
			result, err := readFile(toolCtx, workspace, args.Path, intDefault(args.Offset, 1), intDefault(args.Limit, 200), links)
			if err != nil {
				return nil, ReadFileResult{}, err
			}
			return pageResponse(result.Lines, intDefault(args.Offset, 1), result.Truncated, result.NextOffset,
				func(lines []ReadFileLine, truncated bool, next *int) ReadFileResult {
					evidence := *result.Evidence
					evidence.StartLine, evidence.EndLine = 0, 0
					if len(lines) > 0 {
						evidence.StartLine = lines[0].Line
						evidence.EndLine = lines[len(lines)-1].Line
					}
					return ReadFileResult{Lines: lines, Truncated: truncated, NextOffset: next, Evidence: &evidence}
				},
				func(line ReadFileLine) string { return fmt.Sprintf("%d: %s", line.Line, line.Text) })
		})
	if err := registerListDirTool(server, workspace, config.Timeout); err != nil {
		return nil, err
	}
	registerGlobTool(server, workspace, config.Timeout)
	registerGrepTool(server, workspace, config.Timeout)
	registerApplyPatchTool(server, workspace, config.Timeout)
	cleanup := registerExecTool(server, workspace, config)
	registerSwitchToPlanExecuteTool(server)
	registerLoadSkillTool(server)
	registerUpdateCompletionCriteriaTool(server)
	return cleanup, nil
}

func ReadFile(ctx context.Context, workspace, path string, offset, limit int) (ReadFileResult, error) {
	return readFile(ctx, workspace, path, offset, limit, resolveSymlinks)
}

func readFile(ctx context.Context, workspace, path string, offset, limit int, links symlinkPolicy) (ReadFileResult, error) {
	if offset < 1 || limit < 1 {
		return ReadFileResult{}, fmt.Errorf("offset and limit must be positive")
	}
	resolved, _, err := resolveWorkspacePath(workspace, path, links, false)
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
	if err := ctx.Err(); err != nil {
		return ReadFileResult{}, err
	}
	// 内容、版本和分页都来自同一次读取，避免文件并发变化造成错误的重复判断。
	relative, err := filepath.Rel(workspace, resolved)
	if err != nil {
		return ReadFileResult{}, err
	}
	evidence := &FileEvidence{Path: filepath.ToSlash(relative), Version: fmt.Sprintf("%x", sha256.Sum256(contents))}
	values := []string{}
	if len(contents) > 0 {
		values = strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n")
	}
	page, truncated, next := selectPage(values, offset-1, limit)
	lines := make([]ReadFileLine, 0, len(page))
	for i, value := range page {
		lines = append(lines, ReadFileLine{Line: offset + i, Text: value})
	}
	if len(lines) > 0 {
		evidence.StartLine = lines[0].Line
		evidence.EndLine = lines[len(lines)-1].Line
	}
	if next != nil {
		value := *next + 1
		next = &value
	}
	return ReadFileResult{Lines: lines, Truncated: truncated, NextOffset: next, Evidence: evidence}, nil
}
