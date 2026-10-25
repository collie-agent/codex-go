// Package application coordinates persistent agent runs for both command entries.
package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
)

type Options struct {
	ConfigPath, Task, Workspace, ConvID, RunID, Mode, MCPCommand string
	Timeout                                                      time.Duration
	RuntimeDirs                                                  []string
	Stderr                                                       io.Writer
}

// Run creates the recovery identity before starting any model or tool execution.
func Run(ctx context.Context, opts Options) ([]agent.ReactResult, error) {
	if strings.TrimSpace(opts.Task) == "" {
		return nil, fmt.Errorf("--task is required")
	}
	if opts.Workspace == "" {
		return nil, fmt.Errorf("--workspace is required")
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("--timeout must be positive")
	}
	workspace, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return nil, err
	}
	loaded, err := config.LoadAgentConfig(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	if opts.Mode != "" && opts.Mode != "react" && opts.Mode != "plan-execute" {
		return nil, fmt.Errorf("unsupported mode %q: expected react or plan-execute", opts.Mode)
	}
	mode := opts.Mode
	if mode == "" {
		mode = "react"
	}
	db, err := database.OpenFromEnv(ctx)
	if err != nil {
		return nil, err
	}
	defer database.Close(db)
	store := database.NewRunStore(db)
	run, err := store.Create(ctx, opts.ConvID, opts.Task, workspace, mode)
	if err != nil {
		return nil, err
	}
	// New runs hold the same per-run connection lock as resumed runs. Every
	// running/final checkpoint below must use this connection until execution ends.
	runLock, err := store.AcquireRunLock(ctx, run.RunID)
	if err != nil {
		return nil, err
	}
	defer runLock.Release(ctx)
	ctx = runLock.Context()
	lockedStore := runLock.Store()
	orchestrator := newOrchestrator(ctx, workspace, opts)
	if err := orchestrator.SetMode(opts.Mode); err != nil {
		return nil, err
	}
	// ID 只在 pending checkpoint 提交后输出，stdout 留给最终答案。
	if _, err := fmt.Fprintf(opts.Stderr, "run_id=%s conv_id=%s\n", run.RunID, run.ConvID); err != nil {
		return nil, err
	}
	run.Status = "running"
	if err := lockedStore.Save(ctx, &run); err != nil {
		return nil, err
	}
	durable := &state.DurableState{RunId: run.RunID, Goal: run.Goal}
	return executeRun(ctx, lockedStore, &run, durable, orchestrator, loaded)
}

// executeRun serializes snapshots from concurrent processor branches before
// committing them through the shared run record and lock-holding connection.
func executeRun(ctx context.Context, store *database.RunStore, run *database.AgentRun, durable *state.DurableState, orchestrator *agent.ProcessorOrchestrator, loaded *config.AgentConfig) ([]agent.ReactResult, error) {
	var checkpointMu sync.Mutex
	orchestrator.SetCheckpoint(func(current *state.DurableState, mode string) error {
		checkpointMu.Lock()
		defer checkpointMu.Unlock()
		return store.SaveDurableCheckpoint(ctx, run, current, mode)
	})
	results, runErr := orchestrator.Run(loaded, durable)
	return finishRun(ctx, store, run, durable, results, runErr)
}

// finishRun shares final checkpoint semantics between new and resumed runs.
// An answer is returned only after its checkpoint commits successfully.
func finishRun(ctx context.Context, store *database.RunStore, run *database.AgentRun, durable *state.DurableState, results []agent.ReactResult, runErr error) ([]agent.ReactResult, error) {
	runErr = errors.Join(runErr, context.Cause(ctx))
	run.CompletionCriteria = durable.CompletionCriteria
	if runErr != nil {
		run.Status = "failed"
		run.Error = runErr.Error()
	} else {
		var err error
		run.Status, run.Error = "completed", ""
		// 当前入口为单执行分支；保留全部结构化字段，不能只保存答案。
		if durable.Result != nil {
			run.Result, err = json.Marshal(durable.Result)
		} else if len(results) == 1 {
			run.Result, err = json.Marshal(results[0])
		} else {
			run.Result, err = json.Marshal(results)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := store.Save(ctx, run); err != nil {
		return nil, errors.Join(runErr, err)
	}
	if runErr != nil {
		return nil, runErr
	}
	return results, nil
}
