package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	mcpconfig "github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type execToolArgs struct {
	Command string   `json:"command" jsonschema:"exact approved entrypoint name, never a path or shell command string"`
	Args    []string `json:"args,omitempty" jsonschema:"argument array of strings (default empty); no shell expansion is performed"`
}

// CommandEvidence keeps progress identity independent of output summarization.
type CommandEvidence struct {
	Purpose          string `json:"purpose"`
	WorkspaceVersion string `json:"workspace_version"`
	Fingerprint      string `json:"fingerprint"`
}

type execToolResult struct {
	Evidence  *CommandEvidence `json:"command_evidence,omitempty"`
	Stdout    string           `json:"stdout"`
	Stderr    string           `json:"stderr"`
	ExitCode  int              `json:"exit_code"`
	ErrorKind string           `json:"error_kind,omitempty"`
	Error     string           `json:"error,omitempty"`
}

func registerExecTool(server *sdk.Server, workspace string, config Config) func() {
	// Pin host-authorized runtime configuration at registration. A missing
	// sandbox makes only exec fail; existing file tools remain available.
	executor, startupErr := NewCommandExecutorWithConfig(config)
	cleanup := registerExecSessionTools(server, workspace, config, executor, startupErr)
	commands := execCommands(config.ExecCommands)
	network := config.ExecNetwork
	if network == "" {
		network = "none"
	}
	sdk.AddTool(server, &sdk.Tool{
		Name: string(mcpconfig.ExecMCPToolName), InputSchema: toolInputSchema[execToolArgs](),
		Description: `Execute a command synchronously at the workspace root inside a fresh sandbox using the host-configured network and environment policy. Waits for exit and returns final output; it does not return a process session. For a server that you need to inspect and verify in later calls, use exec_start, exec_poll and exec_stop instead when those tools are enabled. Example arguments: {"command":"git","args":["status","--short"]}. command must be in the host-configured entrypoint whitelist; executable paths and shell strings are rejected. args is an optional JSON array of strings, never a quoted array or null.

Available commands (each example is a complete tool arguments object):
- find: locate files and directories by path, name, or type. Example: {"command":"find","args":[".","-type","f","-name","*.go"]}. -type f selects files; -type d selects directories; -name matches a filename pattern. Pass patterns literally as individual args.
- go: run Go tools for the workspace. Examples: {"command":"go","args":["test","./..."]}, {"command":"go","args":["build","./..."]}.
- cargo: build and test Rust projects. Examples: {"command":"cargo","args":["test","--offline"]}, {"command":"cargo","args":["build","--offline"]}.
- rustc: compile Rust source files. Example: {"command":"rustc","args":["src/main.rs"]}.
- git: inspect and manage the workspace repository. Examples: {"command":"git","args":["status","--short"]}, {"command":"git","args":["diff","--","src/main.ts"]}, {"command":"git","args":["log","-5","--oneline"]}. Use -- before paths when supported. Mutating subcommands change the repository; remote operations require host network mode.
- grep: search file contents using regular expressions. Example: {"command":"grep","args":["-R","-n","-F","--","TODO","src"]}. -R searches recursively, -n prints line numbers, -F treats the pattern as literal text; exit code 1 means no matches.
- npm: run project package scripts and inspect package metadata. Examples: {"command":"npm","args":["run","build"]}, {"command":"npm","args":["test","--","--runInBand"]}. Script names and forwarded options depend on package.json and the test runner. Requires an installed Node/npm runtime and project dependencies; downloading packages is unavailable offline.
- poetry: run commands in a Python project environment or inspect its dependencies. Examples: {"command":"poetry","args":["run","pytest","-q"]}, {"command":"poetry","args":["show"]}. Requires an available Poetry runtime and dependencies; environments stored outside the workspace or authorized runtime directories are inaccessible, and network downloads require host network mode.
- python / python3: execute Python code or workspace scripts with the selected installed interpreter. Examples: {"command":"python3","args":["-c","from pathlib import Path; print(Path('README.md').read_text())"]}, {"command":"python","args":["scripts/check.py"]}, {"command":"python3","args":["-m","unittest","discover"]}. -c takes source code as one argument; -m runs an installed module. python and python3 may refer to different versions or may be absent.
- rg: search workspace contents with ripgrep. Examples: {"command":"rg","args":["-n","--glob","*.go","--","TODO","."]}, {"command":"rg","args":["--files","--glob","*.ts"]}. -n prints line numbers; --glob filters paths; -F uses literal patterns; --files lists files. Ignores hidden and ignored files by default; use --hidden or --no-ignore when needed. Exit code 1 means no matches.
- sed: print selected lines or transform text. Examples: {"command":"sed","args":["-n","1,120p","README.md"]}, {"command":"sed","args":["-i","s/old/new/g","example.txt"]}. -n with a line range prints that range; -i edits the file in place. Script text is one argument, without shell quoting characters around it.
- ts-node: execute a TypeScript workspace script using an installed ts-node/TypeScript runtime. Example: {"command":"ts-node","args":["scripts/check.ts"]}. --project tsconfig.json selects a TypeScript configuration; module behavior depends on the installed runtime and project configuration.
- tsx: execute a TypeScript or JavaScript workspace script using an installed tsx runtime. Example: {"command":"tsx","args":["scripts/check.ts"]}. Pass script arguments after the script path. Avoid watch mode because execution is bounded and background tasks are not retained.

Git commits use user.name and user.email from the host ~/.gitconfig as author and committer through GIT_AUTHOR_* and GIT_COMMITTER_* environment variables when configured. Preserve this identity for commit tasks; do not invent a replacement identity. The host configuration file itself is not exposed.

Entrypoint approval does not guarantee that a command, runtime, or dependency is installed. Prefer read_file, list_dir, glob, grep, and apply_patch for ordinary file operations when those tools are available. Arguments are passed directly, so shell pipes, redirects, &&, environment-variable expansion, and wildcard expansion are not interpreted. Command-specific patterns such as find -name and rg --glob still work. The entrypoint allowlist does not restrict subcommands or child processes; interpreters and project scripts can launch children under the same sandbox boundary.

 No cwd, environment, runtime mount, or stdin overrides are accepted. Commands and their children can read and write the whole workspace, including .git and deleting files; external user files are inaccessible; host network services are accessible only in host network mode. Host-configured runtimes are read-only. Host-authorized persistent caches are writable and shared across calls and process sessions. Named Unix sockets and io_uring are unavailable. stdin is EOF; all descendants are cleaned up after execution. The server tool timeout applies, with a maximum of 5 minutes. Returns complete stdout, stderr and exit_code; exit_code is -1 when no command ran. Ordinary nonzero exits are results, not tool errors. Parameter, sandbox startup, timeout and cancellation errors set isError and error_kind while retaining captured output. Output is returned in full without a capture or response size limit. Allowed entrypoints for this server: ` + strings.Join(commands, ", ") + ". Network mode: " + network + ". Authorized host environment names: " + strings.Join(config.ExecEnv, ", "),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, args execToolArgs) (*sdk.CallToolResult, execToolResult, error) {
		toolCtx, cancel := context.WithTimeout(ctx, config.Timeout)
		defer cancel()
		before := searchSnapshot(toolCtx, workspace, ".", true, true, false)
		result := CommandResult{ExitCode: -1}
		err := startupErr
		if err == nil {
			result, err = executor.Execute(toolCtx, workspace, args.Command, args.Args)
		}
		out := execToolResult{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
		if err != nil {
			out.Error = strings.ToValidUTF8(err.Error(), "\uFFFD")
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
		version := confirmedSearchVersion(before, searchSnapshot(toolCtx, workspace, ".", true, true, false))
		// Hash the original result, before any model-generated summary can vary.
		raw, _ := json.Marshal(out)
		out.Evidence = &CommandEvidence{Purpose: CommandPurpose(args.Command, args.Args), WorkspaceVersion: version, Fingerprint: fmt.Sprintf("%x", sha256.Sum256(raw))}
		return execToolResponse(out)
	})
	return cleanup
}

func execToolResponse(result execToolResult) (*sdk.CallToolResult, execToolResult, error) {
	result.Stdout = strings.ToValidUTF8(result.Stdout, "\uFFFD")
	result.Stderr = strings.ToValidUTF8(result.Stderr, "\uFFFD")
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, result, err
	}
	// Exec returns complete output, independently of the file tools' paginated response limit.
	return &sdk.CallToolResult{
		Content:           []sdk.Content{&sdk.TextContent{Text: string(encoded)}},
		StructuredContent: result,
		IsError:           result.ErrorKind != "",
	}, result, nil
}
