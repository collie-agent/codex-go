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
	mode := flag.String("mode", "", "execution mode: react or plan-execute (skips task analyzer when set)")
	var runtimeDirs []string
	flag.Func("runtime-dir", "host-authorized read-only exec runtime directory (repeatable)", func(dir string) error {
		runtimeDirs = append(runtimeDirs, dir)
		return nil
	})
	flag.Parse()
	if strings.TrimSpace(*task) == "" || *configPath == "" || *workspace == "" || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "--config, --task, --workspace are required and --timeout must be positive")
		os.Exit(2)
	}
	ctx := context.Background()
	mcpArgs := []string{"--workspace", *workspace, "--timeout", timeout.String()}
	for _, dir := range runtimeDirs {
		mcpArgs = append(mcpArgs, "--runtime-dir", dir)
	}
	orchestrator := agent.NewProcessorOrchestratorWithCommand(ctx, *mcpCommand, mcpArgs...)
	if err := orchestrator.SetMode(*mode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	loaded, err := config.LoadAgentConfig(*configPath)
	if err == nil {
		durableState := &fileContext.DurableState{RunId: "cli-task", Goal: *task}
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
