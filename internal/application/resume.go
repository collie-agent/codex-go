package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"gorm.io/gorm"
)

// Resume continues an existing run. It accepts current YAML and MCP settings
// while retaining the run identity, goal, workspace, and mode from the database.
func Resume(ctx context.Context, opts Options) ([]agent.ReactResult, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return nil, fmt.Errorf("--run-id is required")
	}
	if opts.ConfigPath == "" {
		return nil, fmt.Errorf("--config is required")
	}
	loaded, err := config.LoadAgentConfig(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("--timeout must be positive")
	}
	db, err := database.OpenFromEnv(ctx)
	if err != nil {
		return nil, err
	}
	defer database.Close(db)
	store := database.NewRunStore(db)
	lock, err := store.AcquireRunLock(ctx, opts.RunID)
	if err != nil {
		return nil, err
	}
	defer lock.Release(ctx)
	ctx = lock.Context()
	lockedStore := lock.Store()
	run, err := lockedStore.Get(ctx, opts.RunID)
	if err != nil {
		return nil, fmt.Errorf("load run %q: %w", opts.RunID, err)
	}
	// 主动中断已结束原运行；必须在输出或修改检查点前拒绝恢复。
	if run.Status == "interrupted" {
		return nil, fmt.Errorf("run %s was interrupted and cannot be resumed", run.RunID)
	}
	if run.Mode != "" && run.Mode != "react" {
		if run.Mode != "plan-execute" && run.Mode != "multi-agent" {
			return nil, fmt.Errorf("run %s has unsupported mode %q", run.RunID, run.Mode)
		}
	}
	if opts.Mode != "" && opts.Mode != run.Mode && !(opts.Mode == "react" && run.Mode == "") {
		return nil, fmt.Errorf("resume cannot change the original mode")
	}
	mode := run.Mode
	if mode == "" {
		mode = "react"
	}
	if (mode == "multi-agent") != (loaded.Name == "multi-agent") {
		return nil, fmt.Errorf("resume configuration must match the original multi-agent mode")
	}
	opts.Mode = mode
	if opts.Task != "" && opts.Task != run.Goal {
		return nil, fmt.Errorf("resume cannot change the original goal")
	}
	workspace := run.Workspace
	if opts.Workspace != "" {
		requested, err := filepath.Abs(opts.Workspace)
		if err != nil {
			return nil, err
		}
		if requested != workspace {
			return nil, fmt.Errorf("resume cannot change the original workspace")
		}
	}
	if run.Status == "completed" {
		return decodeSavedResults(run.Result)
	}
	if opts.ConvID != "" && opts.ConvID != run.ConvID {
		return nil, fmt.Errorf("resume cannot change the original conversation")
	}
	if run.Status == "failed" && run.FailureType == "business" {
		results, err := decodeSavedResults(run.Result)
		return results, errors.Join(errors.New(run.Error), err)
	}
	if run.Status != "pending" && run.Status != "running" && run.Status != "failed" {
		return nil, fmt.Errorf("run %s has unsupported status %q", run.RunID, run.Status)
	}
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
	ctx, outputCtx, opts, cleanupOutput := prepareOutput(ctx, opts, loaded, db)
	defer cleanupOutput()
	if err := emitTaskInfo(outputCtx, opts, loaded, run); err != nil {
		return nil, err
	}
	steps, err := lockedStore.Steps(ctx, run.RunID)
	if err != nil {
		return nil, fmt.Errorf("load plan steps: %w", err)
	}
	var durableSteps []state.StepExecution
	if mode == "plan-execute" {
		for _, step := range steps {
			durableSteps = append(durableSteps, state.StepExecution{
				StepId: step.StepID, ExecutionID: step.ExecutionID, Revision: step.Revision, Status: state.ExecutionStatus(step.Status),
				Position: step.Position,
				Intent:   step.Intent, CompletionCriteria: step.CompletionCriteria,
				CompletionEvidence: step.CompletionEvidence, Observation: step.Observation,
			})
		}
	}
	executionCtx, cancelExecution := executionContext(outputCtx, opts.Interrupt)
	defer cancelExecution()
	orchestrator := newOrchestrator(executionCtx, workspace, opts)
	// 恢复始终沿用已保存的模式，尚未生成计划时直接进入 planner。
	if err := orchestrator.SetMode(mode); err != nil {
		return nil, err
	}
	durable := &state.DurableState{RunId: run.RunID, Goal: run.Goal, PlanningContext: run.PlanningContext, CompletionCriteria: run.CompletionCriteria, StepExecutions: durableSteps, CurrentStepIndex: -1}
	if mode == "react" {
		saved, err := lockedStore.Execution(ctx, run.RunID)
		if err != nil {
			return nil, fmt.Errorf("load execution instance: %w", err)
		}
		durable.ExecutionID = saved.ExecutionID
		durable.ExecutionResult = saved.ExecutionResult
		durable.ToolExecutions = saved.ToolExecutions
		durable.ExecutionCompaction = saved.ExecutionCompaction
	}
	if mode == "multi-agent" {
		durable.MultiAgentCheckpoint = append([]byte(nil), run.MultiAgentCheckpoint...)
		saved, err := lockedStore.Execution(ctx, run.RunID)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("load entry execution instance: %w", err)
		}
		if err == nil {
			durable.ExecutionID = saved.ExecutionID
			durable.ExecutionResult = saved.ExecutionResult
			durable.ToolExecutions = saved.ToolExecutions
			durable.ExecutionCompaction = saved.ExecutionCompaction
		}
	}
	if mode == "plan-execute" {
		allComplete := len(durable.StepExecutions) > 0
		for i := range durable.StepExecutions {
			step := &durable.StepExecutions[i]
			switch step.Status {
			case state.CompletedExecutionStatus:
			case state.PendingExecutionStatus, state.RunningExecutionStatus, state.FailedExecutionStatus, state.InterruptedExecutionStatus:
				allComplete = false
				step.Status = state.PendingExecutionStatus
			default:
				return nil, fmt.Errorf("run %s has step %q with unsupported status %q", run.RunID, step.StepId, step.Status)
			}
		}
		// Restore the first unfinished step's instance before the planner selects it.
		for _, step := range durable.StepExecutions {
			if step.Status == state.PendingExecutionStatus && step.ExecutionID != "" {
				saved, err := lockedStore.ExecutionByID(ctx, run.RunID, step.ExecutionID)
				if err != nil {
					return nil, fmt.Errorf("load step execution instance: %w", err)
				}
				durable.ExecutionID, durable.ExecutionResult, durable.ToolExecutions = saved.ExecutionID, saved.ExecutionResult, saved.ToolExecutions
				durable.ExecutionCompaction = saved.ExecutionCompaction
				break
			}
			if step.Status == state.PendingExecutionStatus {
				break
			}
		}
		if allComplete {
			results, err := decodeSavedResults(run.Result)
			if err != nil {
				return nil, err
			}
			run.Status, run.Error = "completed", ""
			if err := lockedStore.Save(ctx, &run); err != nil {
				return nil, err
			}
			return results, nil
		}
	}
	run.Status, run.Error = "running", ""
	return executeRun(ctx, executionCtx, lockedStore, &run, durable, orchestrator, loaded, opts.Interrupt)
}

func newOrchestrator(ctx context.Context, workspace string, opts Options) runOrchestrator {
	if opts.Mode == "multi-agent" {
		var multi *agent.MultiAgentProcessorOrchestrator
		if opts.MCPCommand == "" {
			multi = agent.NewMultiAgentProcessorOrchestrator(ctx, filemcp.Config{Workspace: workspace, Timeout: opts.Timeout, RuntimeDirs: opts.RuntimeDirs})
		} else {
			args := []string{"--workspace", workspace, "--timeout", opts.Timeout.String()}
			for _, dir := range opts.RuntimeDirs {
				args = append(args, "--runtime-dir", dir)
			}
			multi = agent.NewMultiAgentProcessorOrchestratorWithCommand(ctx, opts.MCPCommand, args...)
		}
		multi.SetSkills(opts.Skills)
		return multiAgentAdapter{multi}
	}
	var orchestrator *agent.ProcessorOrchestrator
	if opts.MCPCommand == "" {
		orchestrator = agent.NewProcessorOrchestrator(ctx, filemcp.Config{Workspace: workspace, Timeout: opts.Timeout, RuntimeDirs: opts.RuntimeDirs})
	} else {
		args := []string{"--workspace", workspace, "--timeout", opts.Timeout.String()}
		for _, dir := range opts.RuntimeDirs {
			args = append(args, "--runtime-dir", dir)
		}
		orchestrator = agent.NewProcessorOrchestratorWithCommand(ctx, opts.MCPCommand, args...)
	}
	orchestrator.SetSkills(opts.Skills)
	return orchestrator
}

func decodeSavedResults(raw json.RawMessage) ([]agent.ReactResult, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("completed run has no saved result")
	}
	var one agent.ReactResult
	if err := json.Unmarshal(raw, &one); err == nil && one.Status != "" {
		return []agent.ReactResult{one}, nil
	}
	var many []agent.ReactResult
	var multi []agent.MultiAgentResult
	if err := json.Unmarshal(raw, &multi); err == nil && len(multi) > 0 && multi[0].Result.Status != "" {
		return multiAgentAnswers(multi), nil
	}
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("decode completed run result: %w", err)
	}
	return many, nil
}
