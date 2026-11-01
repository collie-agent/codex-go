package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"slices"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	mcpSdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type ReactStatus = fileContext.ReactStatus

const (
	CompletedReactStatus = fileContext.CompletedReactStatus
	FailedReactStatus    = fileContext.FailedReactStatus
)

type ReactResult = fileContext.ReactResult

// BusinessFailureError distinguishes a terminal model decision from an execution fault.
// Keep the structured result on the error because orchestrators may discard failed outputs.
type BusinessFailureError struct {
	Result ReactResult
}

func (e *BusinessFailureError) Error() string {
	return fmt.Sprintf("ReAct reported failure: %s", e.Result.Answer)
}

// ReactSchema 保留普通 ReAct 的输出类型；多代理输出由 MultiAgentReactProcess 处理。
type ReactSchema interface{ ReactResult | ObserveResult }

// ReactProcess 负责普通 ReAct、计划步骤验收及向 plan-execute 的交接。
type ReactProcess[T ReactSchema] struct {
	BaseReactProcess[T]
	allowPlanSwitch bool
}

func NewReactProcess[T ReactSchema](ctx context.Context, toolConfig filemcp.Config) (*ReactProcess[T], error) {
	base, err := newBaseReactProcess[T](ctx, toolConfig)
	if err != nil {
		return nil, err
	}
	return &ReactProcess[T]{BaseReactProcess: *base}, nil
}

// NewReactProcessWithCommand connects to a separately built MCP server over stdio.
func NewReactProcessWithCommand[T ReactSchema](ctx context.Context, command *exec.Cmd) (*ReactProcess[T], error) {
	base, err := newBaseReactProcessWithCommand[T](ctx, command)
	if err != nil {
		return nil, err
	}
	return &ReactProcess[T]{BaseReactProcess: *base}, nil
}

func (r *ReactProcess[T]) Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (T, []AgentProcessorType, error) {
	var zero T
	if r == nil || r.ctx == nil || durableState == nil || agentConfig == nil || agentConfig.LLMConfig == nil {
		return zero, nil, fmt.Errorf("invalid ReAct process")
	}
	// Only a matching step instance can supply native tool history.
	if step := durableState.CurrentStep(); step != nil && (step.ExecutionID == "" || durableState.ExecutionID != step.ExecutionID) {
		durableState.ExecutionID = ""
		durableState.ExecutionResult = nil
		durableState.ToolExecutions = nil
	}
	agentConfig = agentConfig.ForAgent("react")
	rc := r.newContext(agentConfig, durableState)
	modelContext := fileContext.SingleAgentModelContext{ReactContext: rc}
	// 验收标准是运行级一次性能力；已有标准或计划步骤不再暴露更新工具。
	delete(agentConfig.AllowTools, config.UpdateCompletionCriteriaMCPToolName)
	if modelContext.NeedsCompletionCriteria() {
		agentConfig.AllowTools[config.UpdateCompletionCriteriaMCPToolName] = struct{}{}
	}
	if r.allowPlanSwitch && len(durableState.StepExecutions) == 0 {
		agentConfig.AllowTools[config.SwitchToPlanExecuteMCPToolName] = struct{}{}
	}
	validate := func(result T) error {
		reactResult, ok := any(result).(ReactResult)
		if !ok {
			return nil
		}
		// 格式解析不校验业务约束：仅明确完成且有答案时才允许成功退出。
		switch reactResult.Status {
		case FailedReactStatus:
			return &BusinessFailureError{Result: reactResult}
		case CompletedReactStatus:
			// 计划步骤必须提供结构化观察与验收证据，不能用 answer 代替。
			if step := durableState.CurrentStep(); step != nil && step.Status == fileContext.RunningExecutionStatus {
				if !hasNonEmptyItems(reactResult.Observation) || !hasNonEmptyItems(reactResult.CompletionEvidence) {
					return fmt.Errorf("ReAct completed step requires non-empty observation and CompletionEvidence")
				}
			}
			if strings.TrimSpace(reactResult.Answer) == "" {
				return fmt.Errorf("ReAct completed result requires a non-empty answer")
			}
		default:
			return fmt.Errorf("ReAct returned invalid status %q", reactResult.Status)
		}
		return nil
	}
	control := func(params []*mcpSdk.CallToolParams) ([]AgentProcessorType, bool, error) {
		if len(params) == 1 && params[0].Name == string(config.UpdateCompletionCriteriaMCPToolName) {
			if err := r.updateCompletionCriteria(params[0], rc, durableState, agentConfig); err != nil {
				return nil, true, err
			}
			return nil, true, nil
		}
		// 普通工具收集上下文；运行级验收标准由模型按需设置。
		if len(params) != 1 || params[0].Name != string(config.SwitchToPlanExecuteMCPToolName) {
			return nil, false, nil
		}
		switched, err := r.switchToPlanExecute(params[0], rc, durableState)
		if err != nil {
			return nil, true, err
		}
		if switched {
			return []AgentProcessorType{PlanAgentType}, true, nil
		}
		return nil, true, nil
	}
	return r.run(agentConfig, durableState, rc, modelContext, validate, control)
}

// switchToPlanExecute handles the MCP control tool and records the handoff context.
func (r *ReactProcess[T]) switchToPlanExecute(toolParam *mcpSdk.CallToolParams, rc *fileContext.ReactContext, durableState *fileContext.DurableState) (bool, error) {
	response, err := r.mcpClientSess.CallTool(r.ctx, toolParam)
	r.publishControlToolResult(toolParam, rc, response, err)
	if err != nil {
		if durableState.ExecutionID != "" {
			r.updateContext(response, toolParam, rc, err, nil)
		}
		return false, fmt.Errorf("switch to plan-execute: %w", err)
	}
	if response == nil {
		return false, fmt.Errorf("switch to plan-execute: missing MCP response")
	}
	if response.IsError {
		if durableState.ExecutionID != "" {
			r.updateContext(response, toolParam, rc, nil, nil)
		}
		encoded, _ := json.Marshal(response.Content)
		rc.AppendReportedErrors("switch_to_plan_execute rejected: " + string(encoded))
		return false, nil
	}
	// 保存原始决策上下文与模型交接摘要，避免规划阶段丢失已经完成的工作。
	args, _ := json.Marshal(toolParam.Arguments)
	handoff, _ := rc.FormatForModel()
	durableState.PlanningContext = handoff + "\n\n# Switch request\n" + string(args)
	// The accepted control result and mode handoff share one checkpoint.
	if durableState.ExecutionID != "" {
		r.updateContext(response, toolParam, rc, nil, nil)
		if r.executionErr != nil {
			return false, r.executionErr
		}
	}
	log.Printf("task %s: [react] switching to plan-execute", durableState.RunId)
	return true, nil
}

// updateCompletionCriteria 通过 MCP 校验输入，在下一次决策前保存运行级标准。
func (r *ReactProcess[T]) updateCompletionCriteria(param *mcpSdk.CallToolParams, rc *fileContext.ReactContext, durable *fileContext.DurableState, cfg *config.AgentConfig) error {
	response, err := r.mcpClientSess.CallTool(r.ctx, param)
	// isError 分支由 updateContext 发布，避免同一调用重复写入 Stream。
	if err != nil || response == nil || !response.IsError {
		r.publishControlToolResult(param, rc, response, err)
	}
	if err != nil {
		return fmt.Errorf("update completion criteria: %w", err)
	}
	if response == nil {
		return fmt.Errorf("update completion criteria: missing MCP response")
	}
	if response.IsError {
		r.updateContext(response, param, rc, nil, cfg)
		return nil
	}
	encoded, err := json.Marshal(response.StructuredContent)
	if err != nil {
		return fmt.Errorf("encode completion criteria: %w", err)
	}
	var result filemcp.UpdateCompletionCriteriaArgs
	if err := json.Unmarshal(encoded, &result); err != nil {
		return fmt.Errorf("decode completion criteria: %w", err)
	}
	if !hasNonEmptyItems(result.CompletionCriteria) {
		return fmt.Errorf("MCP returned empty completion_criteria")
	}
	for _, criterion := range result.CompletionCriteria {
		if strings.TrimSpace(criterion) == "" {
			return fmt.Errorf("MCP returned blank completion_criteria")
		}
	}
	previous := durable.CompletionCriteria
	durable.CompletionCriteria = slices.Clone(result.CompletionCriteria)
	// Commit criteria with the confirmed control result in the same checkpoint.
	if durable.ExecutionID != "" {
		r.updateContext(response, param, rc, nil, cfg)
		if r.executionErr != nil {
			durable.CompletionCriteria = previous
			return r.executionErr
		}
	} else if r.checkpoint != nil {
		if err := r.checkpoint(); err != nil {
			durable.CompletionCriteria = previous
			return err
		}
	}
	delete(cfg.AllowTools, config.UpdateCompletionCriteriaMCPToolName)

	return nil
}
