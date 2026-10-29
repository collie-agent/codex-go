// Command codex-consumer executes agent tasks queued in Redis.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/consumer"
)

func main() {
	var opts application.Options
	var concurrency int
	flag.IntVar(&concurrency, "concurrency", 1, "maximum number of tasks running simultaneously")
	flag.StringVar(&opts.ConfigPath, "config", "", "YAML agent configuration with top-level redis (required)")
	flag.StringVar(&opts.Workspace, "workspace", "", "workspace directory shared by queued tasks (required)")
	flag.DurationVar(&opts.Timeout, "timeout", 30*time.Second, "tool command timeout")
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
	flag.Parse()
	opts.Stderr = os.Stderr
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := consumer.RunWithConcurrency(ctx, opts, concurrency); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
