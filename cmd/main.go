package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Agent runs require CODEX_MYSQL_URL and an explicit migration (codex-migrate or go run ./cmd/migrate).")
		flag.PrintDefaults()
	}
	workspace := flag.String("workspace", "", "workspace directory (required for new runs; optional when resuming)")
	timeout := flag.Duration("timeout", 30*time.Second, "tool command timeout")
	configPath := flag.String("config", "", "YAML agent configuration")
	convID := flag.String("conv-id", "", "existing conversation ID (creates a conversation when omitted)")
	task := flag.String("task", "", "agent task")
	runID := flag.String("run-id", "", "resume an existing agent run")
	var runtimeDirs []string
	flag.Func("runtime-dir", "host-authorized read-only exec runtime directory (repeatable)", func(dir string) error {
		runtimeDirs = append(runtimeDirs, dir)
		return nil
	})
	flag.Parse()
	var err error
	if *runID != "" && *configPath == "" {
		err = fmt.Errorf("--config is required when --run-id is provided")
	} else if *configPath != "" {
		// 指定配置才进入 Agent 模式；否则保留原有 MCP stdio 入口。
		if *runID != "" {
			err = resumeAgent(*configPath, *runID, *workspace, *timeout, runtimeDirs...)
		} else {
			err = runAgentInConversation(*configPath, *task, *workspace, *convID, *timeout, runtimeDirs...)
		}
	} else {
		err = run(*workspace, *timeout, runtimeDirs...)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func resumeAgent(configPath, runID, workspace string, timeout time.Duration, runtimeDirs ...string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results, err := application.Resume(ctx, application.Options{ConfigPath: configPath, RunID: runID, Workspace: workspace, Timeout: timeout, RuntimeDirs: runtimeDirs, Stderr: os.Stderr})
	if err != nil {
		return err
	}
	for _, result := range results {
		fmt.Println(result.Answer)
	}
	return nil
}

func runAgent(configPath, task, workspace string, timeout time.Duration, runtimeDirs ...string) error {
	return runAgentInConversation(configPath, task, workspace, "", timeout, runtimeDirs...)
}

func runAgentInConversation(configPath, task, workspace, convID string, timeout time.Duration, runtimeDirs ...string) error {
	// 工具超时不限制整个模型运行；返回后取消异步观察器。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results, err := application.Run(ctx, application.Options{ConfigPath: configPath, Task: task, Workspace: workspace, ConvID: convID, Mode: "react", Timeout: timeout, RuntimeDirs: runtimeDirs, Stderr: os.Stderr})
	if err != nil {
		return err
	}
	for _, result := range results {
		fmt.Println(result.Answer)
	}
	return nil
}

func run(workspace string, timeout time.Duration, runtimeDirs ...string) error {
	if workspace == "" {
		return fmt.Errorf("--workspace is required")
	}
	if timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	for _, command := range []string{"find", "sed", "rg"} {
		if _, err := exec.LookPath(command); err != nil {
			return fmt.Errorf("required command %q not found: %w", command, err)
		}
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "codex-go", Version: "0.1.0"}, nil)
	if err := filemcp.RegisterTools(server, filemcp.Config{Workspace: workspace, Timeout: timeout, RuntimeDirs: runtimeDirs}); err != nil {
		return err
	}
	err := server.Run(context.Background(), &sdk.StdioTransport{})
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
