// Command agent-server runs agents through the processor orchestrator.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/application"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Agent runs require CODEX_MYSQL_URL and an explicit migration (codex-migrate or go run ./cmd/migrate).")
		flag.PrintDefaults()
	}
	workspace := flag.String("workspace", "", "workspace directory (required for new runs; optional when resuming)")
	timeout := flag.Duration("timeout", 30*time.Second, "tool command timeout")
	configPath := flag.String("config", "", "YAML agent configuration (required)")
	convID := flag.String("conv-id", "", "existing conversation ID (creates a conversation when omitted)")
	task := flag.String("task", "", "agent task (required)")
	runID := flag.String("run-id", "", "resume an existing agent run")
	mcpCommand := flag.String("mcp-command", "codex-mcp", "path to MCP server executable")
	mode := flag.String("mode", "", "execution mode: react, plan-execute or multi-agent (inferred for multi-agent YAML)")
	var runtimeDirs []string
	flag.Func("runtime-dir", "host-authorized read-only exec runtime directory (repeatable)", func(dir string) error {
		runtimeDirs = append(runtimeDirs, dir)
		return nil
	})
	flag.Parse()
	if *configPath == "" || (*runID == "" && *workspace == "") || *timeout <= 0 || (*runID == "" && strings.TrimSpace(*task) == "") || (*runID != "" && strings.TrimSpace(*task) != "") {
		fmt.Fprintln(os.Stderr, "--config and either --task with --workspace or --run-id are required (--task and --run-id are mutually exclusive); --timeout must be positive")
		os.Exit(2)
	}
	if *mode != "" && *mode != "react" && *mode != "plan-execute" && *mode != "multi-agent" {
		fmt.Fprintf(os.Stderr, "unsupported mode %q: expected react, plan-execute or multi-agent\n", *mode)
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 启动时扫描一次；新建和恢复运行共用索引，不在每轮模型请求时读取磁盘。
	skills, err := agent.LoadSkills()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var results []agent.ReactResult
	if *runID != "" {
		results, err = application.Resume(ctx, application.Options{ConfigPath: *configPath, RunID: *runID, Workspace: *workspace, Mode: *mode, MCPCommand: *mcpCommand, Timeout: *timeout, RuntimeDirs: runtimeDirs, Skills: skills, Stderr: os.Stderr})
	} else {
		results, err = application.Run(ctx, application.Options{ConfigPath: *configPath, Task: *task, Workspace: *workspace, ConvID: *convID, Mode: *mode, MCPCommand: *mcpCommand, Timeout: *timeout, RuntimeDirs: runtimeDirs, Skills: skills, Stderr: os.Stderr})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, result := range results {
		fmt.Println(result.Answer)
	}
}
