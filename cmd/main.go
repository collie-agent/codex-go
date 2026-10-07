package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	workspace := flag.String("workspace", "", "workspace directory (required)")
	timeout := flag.Duration("timeout", 30*time.Second, "tool command timeout")
	configPath := flag.String("config", "", "YAML agent configuration")
	task := flag.String("task", "", "agent task")
	flag.Parse()
	var err error
	if *configPath != "" {
		// 指定配置才进入 Agent 模式；否则保留原有 MCP stdio 入口。
		err = runAgent(*configPath, *task, *workspace, *timeout)
	} else {
		err = run(*workspace, *timeout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runAgent(configPath, task, workspace string, timeout time.Duration) error {
	if strings.TrimSpace(task) == "" {
		return fmt.Errorf("--task is required when --config is specified")
	}
	if workspace == "" {
		return fmt.Errorf("--workspace is required")
	}
	if timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	loaded, err := config.LoadAgentConfig(configPath)
	if err != nil {
		return err
	}
	// --timeout 只限制单次工具调用；多轮模型决策不共享这个时限。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	durableState := &fileContext.DurableState{RunId: "cli-task", Goal: task}
	process, err := agent.NewReactProcess[agent.ReactResult](ctx,
		filemcp.Config{Workspace: workspace, Timeout: timeout})
	if err != nil {
		return err
	}
	result, _, err := process.Run(loaded, durableState)
	if err != nil {
		return err
	}
	fmt.Println(result.Answer)
	return nil
}

func run(workspace string, timeout time.Duration) error {
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
	if err := filemcp.RegisterTools(server, filemcp.Config{Workspace: workspace, Timeout: timeout}); err != nil {
		return err
	}
	err := server.Run(context.Background(), &sdk.StdioTransport{})
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
