package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"time"

	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	workspace := flag.String("workspace", "", "workspace directory (required)")
	timeout := flag.Duration("timeout", 30*time.Second, "tool command timeout")
	flag.Parse()
	if err := run(*workspace, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
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
