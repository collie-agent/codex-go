// Command mcp exposes workspace tools as an MCP stdio server.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/exec"
	"time"

	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/collie-agent/codex-go/internal/logging"
)

func main() {
	logDir := flag.String("log-dir", "", "log directory (default ./logs)")
	workspace := flag.String("workspace", "", "workspace directory (required)")
	timeout := flag.Duration("timeout", 5*time.Minute, "tool command timeout")
	var runtimeDirs []string
	flag.Func("runtime-dir", "host-authorized read-only exec runtime directory (repeatable)", func(dir string) error {
		runtimeDirs = append(runtimeDirs, dir)
		return nil
	})
	logging.ParseFlags()
	if err := logging.Setup(*logDir, ""); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	if *workspace == "" || *timeout <= 0 {
		log.Print("[ERROR] --workspace is required and --timeout must be positive")
		os.Exit(2)
	}
	for _, command := range []string{"find", "sed", "rg"} {
		if _, err := exec.LookPath(command); err != nil {
			log.Printf("[ERROR] required command %q not found: %v", command, err)
			os.Exit(1)
		}
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "codex-go-mcp", Version: "0.1.0"}, nil)
	if err := filemcp.RegisterTools(server, filemcp.Config{Workspace: *workspace, Timeout: *timeout, RuntimeDirs: runtimeDirs}); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	err := server.Run(context.Background(), &sdk.StdioTransport{})
	if err != nil && !errors.Is(err, os.ErrClosed) {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}
