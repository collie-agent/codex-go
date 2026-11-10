// Command mcp exposes workspace tools as an MCP stdio server.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/logging"
)

func main() {
	logDir := flag.String("log-dir", "", "log directory (default ./logs)")
	workspace := flag.String("workspace", "", "workspace directory (required)")
	timeout := flag.Duration("timeout", 5*time.Minute, "tool command timeout")
	discoverHostCommands := flag.Bool("discover-host-commands", false, "discover whitelisted exec runtimes from host PATH at startup")
	execNetwork := flag.String("exec-network", "", "exec network: none or host (overrides YAML; default none)")
	var execGoProxy string
	var execCaches []config.ExecCache
	config.RegisterExecDependencyFlags(flag.CommandLine, &execGoProxy, &execCaches)
	var execEnv []string
	flag.Func("exec-env", "host environment variable name exposed to exec (repeatable; replaces YAML list)", func(name string) error { execEnv = append(execEnv, name); return nil })
	var execCommands []string
	flag.Func("exec-command", "allowed exec entrypoint (repeatable; defaults to built-in whitelist)", func(name string) error { execCommands = append(execCommands, name); return nil })
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
	cleanup, err := filemcp.RegisterToolsWithCleanup(server, filemcp.Config{Workspace: *workspace, Timeout: *timeout, RuntimeDirs: runtimeDirs, DiscoverHostCommands: *discoverHostCommands, ExecCommands: execCommands, ExecGoProxy: execGoProxy, ExecCaches: execCaches, ExecNetwork: *execNetwork, ExecEnv: execEnv})
	if err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = server.Run(ctx, &sdk.StdioTransport{})
	// Stdio EOF and agent cancellation both wait for sandbox teardown before
	// the MCP process exits; parent death remains the crash fallback.
	cleanup()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}
