// Command agent-server runs an agent and delegates tool calls to a child MCP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
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
		args := []string{"--workspace", *workspace, "--timeout", timeout.String()}
		if loaded.AllowTools != nil {
			names := make([]string, 0, len(loaded.AllowTools))
			for name := range loaded.AllowTools {
				names = append(names, string(name))
			}
			args = append(args, "--allow-tools", strings.Join(names, ","))
		}
		process, procErr := agent.NewReactProcessWithCommand[agent.ReactResult](ctx,
			&fileContext.DurableState{RunId: "cli-task", Goal: *task}, loaded,
			exec.Command(*mcpCommand, args...))
		if procErr != nil {
			err = procErr
		} else {
			result, runErr := process.Run()
			err = runErr
			if runErr == nil {
				fmt.Println(result.Answer)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
