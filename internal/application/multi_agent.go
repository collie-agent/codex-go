package application

import (
	"fmt"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
)

// runOrchestrator 让持久化流程复用两种编排器，不承担具体任务调度。
type runOrchestrator interface {
	SetMode(string) error
	SetSkills([]agent.Skill)
	SetCheckpoint(func(*state.DurableState, string) error)
	Run(*config.AgentConfig, *state.DurableState) ([]agent.ReactResult, error)
}

// 仅在命令输出边界投影最终答案；durable.Result 保留完整 MultiAgentResult。
type multiAgentAdapter struct {
	*agent.MultiAgentProcessorOrchestrator
}

func (m multiAgentAdapter) SetMode(mode string) error {
	if mode != "multi-agent" {
		return fmt.Errorf("multi-agent orchestrator requires multi-agent mode")
	}
	return nil
}

func (m multiAgentAdapter) Run(cfg *config.AgentConfig, durable *state.DurableState) ([]agent.ReactResult, error) {
	results, err := m.MultiAgentProcessorOrchestrator.Run(cfg, durable)
	if err != nil {
		return nil, err
	}
	return multiAgentAnswers(results), nil
}

func multiAgentAnswers(results []agent.MultiAgentResult) []agent.ReactResult {
	answers := make([]agent.ReactResult, len(results))
	for i, result := range results {
		answers[i] = result.Result
	}
	return answers
}
