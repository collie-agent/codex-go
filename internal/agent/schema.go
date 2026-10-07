package agent

import (
	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
)

type LLMSchema interface {
	ReactResult | ObserveResult | TaskAnalyzerResult
}

type AgentProcessor[T LLMSchema] interface {
	// Run 返回处理结果及后续处理器类型(可以有多个,多个时并发执行)；nil 类型列表表示未选择后续节点。
	/*
		task analyzer -> react -> end
		plan1 -> react1 -> plan2 -> react2(新实例) -> plan3 -> end
		plan后续:react/end
		plan1,2,3共享durable state
	*/
	Run(agentConfig *config.AgentConfig, durableState *fileContext.DurableState) (T, []AgentProcessorType, error)
}

// 编译期检查确保各处理器始终满足统一的运行接口。
var (
	_ AgentProcessor[ReactResult]        = (*ReactProcess[ReactResult])(nil)
	_ AgentProcessor[ObserveResult]      = (*ReactProcess[ObserveResult])(nil)
	_ AgentProcessor[ObserveResult]      = (*ObserveProcess[ObserveResult])(nil)
	_ AgentProcessor[TaskAnalyzerResult] = (*TaskAnalyzerProcess[TaskAnalyzerResult])(nil)
)
