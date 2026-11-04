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
	"github.com/collie-agent/codex-go/internal/provider"
	"gorm.io/gorm"
)

type Options struct {
	ConfigPath  string
	Task        string
	Workspace   string
	ConvID      string
	RunID       string
	Mode        string
	MCPCommand  string
	Timeout     time.Duration
	RuntimeDirs []string
	Skills      []agent.Skill
	Stderr      io.Writer
	Output      provider.Outputer
	Interrupt   *Interrupt
	// OnRunLocked registers external interruption while this execution owns the run lock.
	OnRunLocked func(context.Context, string, *Interrupt) (func(), error)
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
	if opts.Mode != "" && opts.Mode != "react" && opts.Mode != "plan-execute" && opts.Mode != "multi-agent" {
		return nil, fmt.Errorf("unsupported mode %q: expected react, plan-execute or multi-agent", opts.Mode)
	}
	mode := opts.Mode
	if mode == "" {
		mode = "react"
		if loaded.Name == "multi-agent" {
			mode = "multi-agent"
		}
	}
	if (mode == "multi-agent") != (loaded.Name == "multi-agent") {
		return nil, fmt.Errorf("multi-agent mode requires a multi-agent configuration and vice versa")
	}
	opts.Mode = mode
	db, err := database.OpenFromEnv(ctx)
	if err != nil {
		return nil, err
	}
	defer database.Close(db)
	store := database.NewRunStore(db)
	// 普通输入始终创建新运行，是否承接历史目标由模型结合上下文判断。
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
	if opts.OnRunLocked != nil {
		if opts.Interrupt == nil {
			opts.Interrupt = NewInterrupt()
		}
		cleanup, err := opts.OnRunLocked(ctx, run.RunID, opts.Interrupt)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		defer opts.Interrupt.Finish()
	}
	lockedStore := runLock.Store()
	ctx, outputCtx, opts, cleanupOutput := prepareOutput(ctx, opts, loaded, db)
	defer cleanupOutput()
	executionCtx, cancelExecution := executionContext(outputCtx, opts.Interrupt)
	defer cancelExecution()
	orchestrator := newOrchestrator(executionCtx, workspace, opts)
	if err := orchestrator.SetMode(opts.Mode); err != nil {
		return nil, err
	}
	// ID 只在 pending checkpoint 提交后输出，stdout 留给最终答案。
	if err := emitTaskInfo(outputCtx, opts, loaded, run); err != nil {
		return nil, err
	}
	run.Status = "running"
	if err := lockedStore.Save(ctx, &run); err != nil {
		return nil, err
	}
	durable := &state.DurableState{RunId: run.RunID, Goal: run.Goal}
	if mode == "react" {
		durable.ExecutionID = run.RunID
	}
	return executeRun(ctx, executionCtx, lockedStore, &run, durable, orchestrator, loaded, opts.Interrupt)
}

func prepareOutput(ctx context.Context, opts Options, loaded *config.AgentConfig, db *gorm.DB) (context.Context, context.Context, Options, func()) {
	live := opts.Output
	cleanup := func() {}
	if live == nil {
		live, cleanup = provider.NewTaskOutputer(loaded.RedisConfig(), loaded.LogLevel, opts.Stderr)
	}
	outputCtx, cancel := context.WithCancelCause(ctx)
	opts.Output = &cancelOnOutputFailure{next: live, cancel: cancel}
	return ctx, provider.WithOutputer(outputCtx, opts.Output), opts, func() { cancel(nil); cleanup() }
}

// Stop execution after output delivery fails to avoid silently losing later events.
type cancelOnOutputFailure struct {
	mu     sync.Mutex
	next   provider.Outputer
	cancel context.CancelCauseFunc
	err    error
}

func (o *cancelOnOutputFailure) Emit(ctx context.Context, event provider.OutputEvent) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return o.err
	}
	if err := o.next.Emit(ctx, event); err != nil {
		// Expected execution cancellation must not poison terminal-state reporting.
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return err
		}
		o.err = err
		o.cancel(err)
		return err
	}
	return nil
}

func executionContext(lockCtx context.Context, interrupt *Interrupt) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(lockCtx)
	if interrupt != nil {
		go func() {
			select {
			case <-interrupt.done:
				cancel(ErrInterrupted)
			case <-ctx.Done():
			}
		}()
	}
	return ctx, func() { cancel(nil) }
}

// executeRun serializes snapshots from concurrent processor branches before
// committing them through the shared run record and lock-holding connection.
func executeRun(ctx, executionCtx context.Context, store *database.RunStore, run *database.AgentRun, durable *state.DurableState, orchestrator runOrchestrator, loaded *config.AgentConfig, interrupt *Interrupt) ([]agent.ReactResult, error) {
	// 新建与恢复共用历史加载入口；历史工具消息只供参考，不恢复旧运行调度。
	previous, err := store.PreviousRuns(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("load previous agent runs: %w", err)
	}
	durable.PreviousRunOutcomes = nil
	for _, prior := range previous {
		switch prior.Status {
		case "completed":
			durable.PreviousRunOutcomes = append(durable.PreviousRunOutcomes, state.RunOutcome{RunID: prior.RunID, Goal: prior.Goal, Status: prior.Status, Result: string(prior.Result), Error: prior.Error})
		case "failed", "interrupted":
			history, err := interruptedHistory(ctx, store, prior)
			if err != nil {
				return nil, fmt.Errorf("load historical run %s: %w", prior.RunID, err)
			}
			durable.PreviousRunOutcomes = append(durable.PreviousRunOutcomes, history)
		}
	}
	var checkpointMu sync.Mutex
	orchestrator.SetCheckpoint(func(current *state.DurableState, mode string) error {
		checkpointMu.Lock()
		defer checkpointMu.Unlock()
		return store.SaveDurableCheckpoint(ctx, run, current, mode)
	})
	results, runErr := orchestrator.Run(loaded, durable)
	runErr = errors.Join(runErr, context.Cause(executionCtx))
	if interrupt != nil {
		interrupt.Finish()
	}
	if runErr != nil && errors.Is(context.Cause(executionCtx), ErrInterrupted) {
		runErr = errors.Join(runErr, ErrInterrupted)
	}
	return finishRun(ctx, store, run, durable, results, runErr)
}

// finishRun shares final checkpoint semantics between new and resumed runs.
// An answer is returned only after its checkpoint commits successfully.
func finishRun(ctx context.Context, store *database.RunStore, run *database.AgentRun, durable *state.DurableState, results []agent.ReactResult, runErr error) ([]agent.ReactResult, error) {
	runErr = errors.Join(runErr, context.Cause(ctx))
	run.CompletionCriteria = durable.CompletionCriteria
	if runErr != nil {
		run.Status = "failed"
		if errors.Is(runErr, ErrInterrupted) {
			run.Status = "interrupted"
		}
		run.FailureType = "execution"
		var businessFailure *agent.BusinessFailureError
		if errors.As(runErr, &businessFailure) {
			results = []agent.ReactResult{businessFailure.Result}
			// A failed plan step supersedes the previous successful step's result.
			durable.Result = businessFailure.Result
		}
		if len(results) > 0 && results[0].Status == agent.FailedReactStatus {
			run.FailureType = "business"
		}
		if durable.Result != nil {
			encoded, err := json.Marshal(durable.Result)
			if err != nil {
				return nil, err
			}
			run.Result = encoded
		} else if len(durable.ExecutionResult) > 0 {
			run.Result = append(json.RawMessage(nil), durable.ExecutionResult...)
		}
		run.Error = runErr.Error()
	} else {
		var err error
		run.Status, run.Error, run.FailureType = "completed", "", ""
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
		if errors.Is(runErr, ErrInterrupted) {
			return nil, errors.Join(runErr, &InterruptPersistenceError{Err: err})
		}
		return nil, errors.Join(runErr, err)
	}
	if runErr != nil {
		if run.FailureType == "business" {
			return results, runErr
		}
		return nil, runErr
	}
	return results, nil
}

func emitTaskInfo(ctx context.Context, opts Options, loaded *config.AgentConfig, run database.AgentRun) error {
	output := opts.Output
	if output == nil {
		var cleanup func()
		output, cleanup = provider.NewTaskOutputer(loaded.RedisConfig(), loaded.LogLevel, opts.Stderr)
		defer cleanup()
	}
	modelName := ""
	// Multi-agent configurations may not define a single shared model.
	if loaded.LLMConfig != nil {
		modelName = loaded.LLMConfig.Model
	}
	data, err := json.Marshal(provider.TaskInfoOutput{RunID: run.RunID, ConvID: run.ConvID, ModelName: modelName})
	if err != nil {
		return err
	}
	return output.Emit(ctx, provider.OutputEvent{TaskID: run.RunID, RunID: run.RunID, Kind: provider.OutputTaskInfo, Data: data})
}
