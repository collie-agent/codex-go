package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/logging"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var discoverHostCommands bool
var discoverHostCommandsSet bool
var execCommands []string
var execGoProxy string
var execCaches []config.ExecCache
var execNetwork string
var execEnv []string

var logDirectory = flag.String("log-dir", "", "output log directory (overrides YAML log_dir; default ./logs)")

func main() {
	config.RegisterExecDependencyFlags(flag.CommandLine, &execGoProxy, &execCaches)
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Agent runs require CODEX_MYSQL_URL and an explicit migration (codex-migrate or go run ./cmd/migrate).")
		flag.PrintDefaults()
	}
	workspace := flag.String("workspace", "", "workspace directory (required for new runs; optional when resuming)")
	timeout := flag.Duration("timeout", 5*time.Minute, "tool command timeout")
	configPath := flag.String("config", "", "YAML agent configuration")
	convID := flag.String("conv-id", "", "existing conversation ID (creates a conversation when omitted)")
	task := flag.String("task", "", "agent task")
	runID := flag.String("run-id", "", "resume an existing agent run")
	flag.BoolVar(&discoverHostCommands, "discover-host-commands", false, "discover whitelisted exec runtimes from host PATH at startup")
	flag.Func("exec-command", "allowed exec entrypoint (repeatable; defaults to built-in whitelist)", func(name string) error { execCommands = append(execCommands, name); return nil })
	flag.StringVar(&execNetwork, "exec-network", "", "exec network: none or host (overrides YAML; default none)")
	flag.Func("exec-env", "host environment variable name exposed to exec (repeatable; replaces YAML list)", func(name string) error { execEnv = append(execEnv, name); return nil })
	var runtimeDirs []string
	flag.Func("runtime-dir", "host-authorized read-only exec runtime directory (repeatable)", func(dir string) error {
		runtimeDirs = append(runtimeDirs, dir)
		return nil
	})
	logging.ParseFlags()
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "discover-host-commands" {
			discoverHostCommandsSet = true
		}
	})
	if err := logging.Setup(*logDirectory, *configPath); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
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
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}

func resumeAgent(configPath, runID, workspace string, timeout time.Duration, runtimeDirs ...string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results, err := application.Resume(ctx, application.Options{ConfigPath: configPath, RunID: runID, Workspace: workspace, Timeout: timeout, RuntimeDirs: runtimeDirs, DiscoverHostCommands: discoverHostCommands, DiscoverHostCommandsSet: discoverHostCommandsSet, ExecCommands: execCommands, ExecGoProxy: execGoProxy, ExecCaches: execCaches, ExecNetwork: execNetwork, ExecEnv: execEnv, LogDir: *logDirectory})
	if err != nil {
		return err
	}
	for _, result := range results {
		log.Printf("[INFO] final answer: %s", result.Answer)
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
	results, err := application.Run(ctx, application.Options{ConfigPath: configPath, Task: task, Workspace: workspace, ConvID: convID, Timeout: timeout, RuntimeDirs: runtimeDirs, DiscoverHostCommands: discoverHostCommands, DiscoverHostCommandsSet: discoverHostCommandsSet, ExecCommands: execCommands, ExecGoProxy: execGoProxy, ExecCaches: execCaches, ExecNetwork: execNetwork, ExecEnv: execEnv, LogDir: *logDirectory})
	if err != nil {
		return err
	}
	for _, result := range results {
		log.Printf("[INFO] final answer: %s", result.Answer)
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
	cleanup, err := filemcp.RegisterToolsWithCleanup(server, filemcp.Config{Workspace: workspace, Timeout: timeout, RuntimeDirs: runtimeDirs, DiscoverHostCommands: discoverHostCommands, ExecCommands: execCommands, ExecGoProxy: execGoProxy, ExecCaches: execCaches, ExecNetwork: execNetwork, ExecEnv: execEnv})
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = server.Run(ctx, &sdk.StdioTransport{})
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
