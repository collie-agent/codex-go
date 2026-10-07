package agent

import (
	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
)

type LLMSchema interface {
	ReactResult | ObserveResult
}

type AgentProcessor[T LLMSchema] interface {
	Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (T, error)
}

// 编译期检查确保两种处理器始终满足统一的运行接口。
var (
	_ AgentProcessor[ReactResult]   = (*ReactProcess[ReactResult])(nil)
	_ AgentProcessor[ObserveResult] = (*ReactProcess[ObserveResult])(nil)
	_ AgentProcessor[ObserveResult] = (*ObserveProcess)(nil)
)
