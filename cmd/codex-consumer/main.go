// Command codex-consumer executes agent tasks queued in Redis.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/consumer"
	"github.com/collie-agent/codex-go/internal/logging"
)

func main() {
	var opts application.Options
	var concurrency int
	flag.StringVar(&opts.ExecNetwork, "exec-network", "", "exec network: none or host (overrides YAML; default none)")
	flag.Func("exec-env", "host environment variable name exposed to exec (repeatable; replaces YAML list)", func(name string) error { opts.ExecEnv = append(opts.ExecEnv, name); return nil })
	flag.BoolVar(&opts.DiscoverHostCommands, "discover-host-commands", false, "discover whitelisted exec runtimes from host PATH at startup")
	flag.Func("exec-command", "allowed exec entrypoint (repeatable; defaults to built-in whitelist)", func(name string) error { opts.ExecCommands = append(opts.ExecCommands, name); return nil })
	flag.StringVar(&opts.LogDir, "log-dir", "", "output log directory (overrides YAML log_dir; default ./logs)")
	flag.IntVar(&concurrency, "concurrency", 1, "maximum number of tasks running simultaneously")
	flag.StringVar(&opts.ConfigPath, "config", "", "YAML agent configuration with top-level redis (required)")
	flag.StringVar(&opts.Workspace, "workspace", "", "workspace directory shared by queued tasks (required)")
	flag.DurationVar(&opts.Timeout, "timeout", 5*time.Minute, "tool command timeout")
	flag.StringVar(&opts.Mode, "mode", "", "react, plan-execute or multi-agent (defaults to YAML name)")
	flag.StringVar(&opts.MCPCommand, "mcp-command", "", "external MCP executable (defaults to in-process tools)")
	flag.Func("runtime-dir", "project runtime directory allowed for exec (repeatable)", func(dir string) error {
		opts.RuntimeDirs = append(opts.RuntimeDirs, dir)
		return nil
	})
	// Go's flag parser accepts both spellings; show the preferred long form in help.
	flag.Usage = func() {
		output := flag.CommandLine.Output()
		fmt.Fprintf(output, "Usage of %s:\n", os.Args[0])
		var defaults bytes.Buffer
		flag.CommandLine.SetOutput(&defaults)
		flag.PrintDefaults()
		flag.CommandLine.SetOutput(output)
		fmt.Fprint(output, strings.TrimPrefix(strings.ReplaceAll("\n"+defaults.String(), "\n  -", "\n  --"), "\n"))
	}
	logging.ParseFlags()
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "discover-host-commands" {
			opts.DiscoverHostCommandsSet = true
		}
	})
	if err := logging.Setup(opts.LogDir, opts.ConfigPath); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := consumer.RunWithConcurrency(ctx, opts, concurrency); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}
