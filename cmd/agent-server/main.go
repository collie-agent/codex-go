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
	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
)

func main() {
	workspace := flag.String("workspace", "", "workspace directory (required)")
	timeout := flag.Duration("timeout", 30*time.Second, "tool command timeout")
	configPath := flag.String("config", "", "YAML agent configuration (required)")
	task := flag.String("task", "", "agent task (required)")
	mcpCommand := flag.String("mcp-command", "codex-mcp", "path to MCP server executable")
	flag.Parse()
	if strings.TrimSpace(*task) == "" || *configPath == "" || *workspace == "" || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "--config, --task, --workspace are required and --timeout must be positive")
		os.Exit(2)
	}
	loaded, err := config.LoadAgentConfig(*configPath)
	if err == nil {
		ctx := context.Background()
		durableState := &fileContext.DurableState{RunId: "cli-task", Goal: *task}
		// 构造函数以 TaskAnalyzerAgentType 为根节点，后续流程由处理器决定。
		orchestrator := agent.NewProcessorOrchestratorWithCommand(ctx, *mcpCommand,
			"--workspace", *workspace, "--timeout", timeout.String())
		results, runErr := orchestrator.Run(loaded, durableState)
		err = runErr
		if runErr == nil {
			for _, result := range results {
				fmt.Println(result.Answer)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
