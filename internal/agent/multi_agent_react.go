package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

// MultiAgentReactProcess 执行一个代理的 ReAct；委派调度仍由多代理编排器负责。
type MultiAgentReactProcess struct {
	BaseReactProcess[MultiAgentResult]
	multiAgentContext               string
	generateEntryCompletionCriteria bool
}

func NewMultiAgentReactProcess(ctx context.Context, toolConfig filemcp.Config) (*MultiAgentReactProcess, error) {
	base, err := newBaseReactProcess[MultiAgentResult](ctx, toolConfig)
	if err != nil {
		return nil, err
	}
	return &MultiAgentReactProcess{BaseReactProcess: *base}, nil
}

// NewMultiAgentReactProcessWithCommand connects one agent to an MCP server over stdio.
func NewMultiAgentReactProcessWithCommand(ctx context.Context, command *exec.Cmd) (*MultiAgentReactProcess, error) {
	base, err := newBaseReactProcessWithCommand[MultiAgentResult](ctx, command)
	if err != nil {
		return nil, err
	}
	return &MultiAgentReactProcess{BaseReactProcess: *base}, nil
}

// SetDelegationContext 提供本次委派的上下文；仅入口缺少验收标准时请求模型生成标准。
func (r *MultiAgentReactProcess) SetDelegationContext(delegation string, generateEntryCompletionCriteria bool) {
	r.multiAgentContext = delegation
	r.generateEntryCompletionCriteria = generateEntryCompletionCriteria
}

func (r *MultiAgentReactProcess) Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (MultiAgentResult, []AgentProcessorType, error) {
	if r == nil || r.ctx == nil || durableState == nil || agentConfig == nil || agentConfig.LLMConfig == nil {
		return MultiAgentResult{}, nil, fmt.Errorf("invalid ReAct process")
	}
	// 配置已经由编排器按代理名称解析，不能重新选择 react 配置。
	agentConfig = agentConfig.Copy()
	rc := r.newContext(agentConfig, durableState)
	modelContext := fileContext.MultiAgentModelContext{ReactContext: rc, Delegation: r.multiAgentContext, GenerateEntryCompletionCriteria: r.generateEntryCompletionCriteria}
	validate := func(result MultiAgentResult) error {
		// failed 是业务结果，保留原因交给 supervisor；基础设施错误才返回 error。
		if result.Result.Status != CompletedReactStatus && result.Result.Status != FailedReactStatus {
			return fmt.Errorf("ReAct returned invalid status %q", result.Result.Status)
		}
		if strings.TrimSpace(result.Result.Answer) == "" {
			return fmt.Errorf("ReAct result requires a non-empty answer")
		}
		return nil
	}
	return r.run(agentConfig, durableState, rc, modelContext, validate, nil)
}
