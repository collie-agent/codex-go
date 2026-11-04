package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/provider"
)

// agentOutput reuses the run's durable pipeline when one is available.
// 让同一次运行中的 agent 共用应用层创建的outputer
// context outputer是在run/resume入口创建的
// 如果使用独立的outputer,不共用,会绕过应用层的历史记录和失败处理
func agentOutput(ctx context.Context, cfg *config.AgentConfig) (provider.Outputer, func()) {
	if output := provider.OutputerFromContext(ctx); output != nil {
		return output, func() {}
	}
	return provider.NewConfiguredOutputer(cfg.RedisConfig(), cfg.LogLevel)
}

func emitLifecycle(ctx context.Context, output provider.Outputer, run string, agent provider.AgentParticipation, status provider.AgentStatus) error {
	if output == nil {
		return nil
	}
	data, err := json.Marshal(provider.AgentLifecycleOutput{Status: status})
	if err != nil {
		return err
	}
	return output.Emit(ctx, provider.OutputEvent{TaskID: run, RunID: run, AgentInstanceID: agent.ID, ParentAgentInstanceID: agent.ParentID, AgentName: agent.Name, Kind: provider.OutputAgentLifecycle, Data: data})
}

// A canceled execution context cannot write its terminal state; preserve its
// routing values while allowing a bounded attempt to report the outcome.
func finishLifecycle(ctx context.Context, output provider.Outputer, run string, agent provider.AgentParticipation, status provider.AgentStatus) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return emitLifecycle(writeCtx, output, run, agent, status)
}

func lifecycleFailure(ctx context.Context) provider.AgentStatus {
	if errors.Is(context.Cause(ctx), provider.ErrInterrupted) {
		return provider.AgentInterrupted
	}
	return provider.AgentFailed
}
