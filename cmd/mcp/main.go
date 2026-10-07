// Command mcp exposes workspace tools as an MCP stdio server.
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

	"github.com/collie-agent/codex-go/internal/config"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	workspace := flag.String("workspace", "", "workspace directory (required)")
	timeout := flag.Duration("timeout", 30*time.Second, "tool command timeout")
	var allowed string
	flag.StringVar(&allowed, "allow-tools", "", "comma-separated tool allow-list")
	flag.Parse()
	if *workspace == "" || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "--workspace is required and --timeout must be positive")
		os.Exit(2)
	}
	for _, command := range []string{"find", "sed", "rg"} {
		if _, err := exec.LookPath(command); err != nil {
			fmt.Fprintf(os.Stderr, "required command %q not found: %v\n", command, err)
			os.Exit(1)
		}
	}
	var allow map[config.McpToolName]struct{}
	if allowed != "" {
		allow = make(map[config.McpToolName]struct{})
		for _, name := range strings.Split(allowed, ",") {
			allow[config.McpToolName(strings.TrimSpace(name))] = struct{}{}
		}
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "codex-go-mcp", Version: "0.1.0"}, nil)
	if err := filemcp.RegisterTools(server, filemcp.Config{Workspace: *workspace, Timeout: *timeout, AllowTools: allow}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	err := server.Run(context.Background(), &sdk.StdioTransport{})
	if err != nil && !errors.Is(err, os.ErrClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
