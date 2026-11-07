package agent

import (
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
)

// transientResultsContext 仅裁剪决策视图，原始记录继续用于恢复、压缩和回读。
type transientResultsContext struct {
	state.Contexter
	minimumBatch int
}

func (c *transientResultsContext) ModelRequest() state.ModelRequest {
	request := c.Contexter.ModelRequest()
	kept := make([]*state.ToolResult, 0, len(request.ToolResults))
	for _, result := range request.ToolResults {
		switch config.McpToolName(result.ToolName) {
		case config.GlobMCPToolName, config.ListDirMCPToolName:
			if result.Batch < c.minimumBatch {
				continue
			}
		}
		kept = append(kept, result)
	}
	request.ToolResults = kept
	return request
}
