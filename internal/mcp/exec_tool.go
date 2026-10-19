package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	mcpconfig "github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type execToolArgs struct {
	Command string   `json:"command" jsonschema:"exact approved entrypoint name, never a path or shell command string"`
	Args    []string `json:"args,omitempty" jsonschema:"argument array of strings (default empty); no shell expansion is performed"`
}

type execToolResult struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated"`
	ErrorKind string `json:"error_kind,omitempty"`
	Error     string `json:"error,omitempty"`
}

func registerExecTool(server *sdk.Server, workspace string, config Config) {
	// Pin host-authorized runtime configuration at registration. A missing
	// sandbox makes only exec fail; existing file tools remain available.
	executor, startupErr := NewCommandExecutor(config.RuntimeDirs...)
	sdk.AddTool(server, &sdk.Tool{
		Name: string(mcpconfig.ExecMCPToolName), InputSchema: toolInputSchema[execToolArgs](),
		Description: `Execute a command at the workspace root inside a fresh offline sandbox. Example arguments: {"command":"git","args":["status","--short"]}. command must be exactly find, git, grep, npm, poetry, python, python3, rg, sed, ts-node, or tsx; executable paths and shell strings are rejected. args is an optional JSON array of strings, never a quoted array or null. No cwd, environment, runtime mount, or stdin overrides are accepted. Commands and their children can read and write the whole workspace, including .git and deleting files; external user files and host network services are inaccessible. Host-configured runtimes are read-only. Named Unix sockets and io_uring are unavailable. stdin is EOF; all descendants are cleaned up after execution. The server tool timeout applies, with a maximum of 30 seconds. Returns stdout, stderr, exit_code and truncated; exit_code is -1 when no command ran. Ordinary nonzero exits are results, not tool errors. Parameter, sandbox startup, timeout and cancellation errors set isError and error_kind while retaining captured output. Combined captured output is at most 1 MiB, and may be shortened further to fit the complete 1 MiB MCP response; truncated output cannot be resumed.`,
	}, func(ctx context.Context, _ *sdk.CallToolRequest, args execToolArgs) (*sdk.CallToolResult, execToolResult, error) {
		result := CommandResult{ExitCode: -1}
		err := startupErr
		if err == nil {
			toolCtx, cancel := context.WithTimeout(ctx, config.Timeout)
			defer cancel()
			result, err = executor.Execute(toolCtx, workspace, args.Command, args.Args)
		}
		out := execToolResult{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode, Truncated: result.Truncated}
		if err != nil {
			out.Error = commandOutputPrefix(strings.ToValidUTF8(err.Error(), "\uFFFD"), 4096)
			switch {
			case errors.Is(err, ErrCommandParameter):
				out.ErrorKind, out.ExitCode = "parameter", -1
			case errors.Is(err, context.DeadlineExceeded):
				out.ErrorKind = "timeout"
			case errors.Is(err, context.Canceled):
				out.ErrorKind = "canceled"
			default:
				out.ErrorKind, out.ExitCode = "sandbox_startup", -1
			}
		}
		return execToolResponse(out)
	})
}

func execToolResponse(result execToolResult) (*sdk.CallToolResult, execToolResult, error) {
	// Both JSON representations and escaping count toward the MCP limit, unlike
	// the executor's raw capture budget. Fit the wire response without rerunning
	// a command that may already have modified Workspace.
	result.Stdout = strings.ToValidUTF8(result.Stdout, "\uFFFD")
	result.Stderr = strings.ToValidUTF8(result.Stderr, "\uFFFD")
	total := len(result.Stdout) + len(result.Stderr)
	build := func(size int) (*sdk.CallToolResult, execToolResult, error) {
		out := result
		stdoutSize := 0
		if total != 0 {
			// Share the wire budget proportionally so a large stdout does not
			// erase a small, already captured stderr diagnostic.
			stdoutSize = int(int64(size) * int64(len(result.Stdout)) / int64(total))
		}
		out.Stdout = commandOutputPrefix(result.Stdout, stdoutSize)
		out.Stderr = commandOutputPrefix(result.Stderr, size-stdoutSize)
		out.Truncated = result.Truncated || size < total
		encoded, err := json.Marshal(out)
		if err != nil {
			return nil, out, err
		}
		response, err := toolResponse(out, string(encoded), out.ErrorKind != "")
		if err == nil {
			// The SDK adds resultType and serverInfo metadata after the handler.
			// Reserve space for that envelope for the repository's MCP servers.
			wire, marshalErr := json.Marshal(response)
			err = marshalErr
			if err == nil && len(wire) > maxResponseBytes-1024 {
				err = errors.New("exec response needs room for MCP metadata")
			}
		}
		return response, out, err
	}
	response, out, err := build(total)
	if err == nil {
		return response, out, nil
	}
	low, high := 0, total-1
	var best *sdk.CallToolResult
	var bestOut execToolResult
	for low <= high {
		mid := low + (high-low)/2
		candidate, value, candidateErr := build(mid)
		if candidateErr != nil {
			high = mid - 1
		} else {
			best, bestOut = candidate, value
			low = mid + 1
		}
	}
	if best == nil {
		return nil, out, err
	}
	return best, bestOut, nil
}

func commandOutputPrefix(value string, size int) string {
	if size >= len(value) {
		return value
	}
	// Keep text valid when a byte budget ends within a multibyte character.
	for size > 0 && !utf8.RuneStart(value[size]) {
		size--
	}
	return value[:size]
}
