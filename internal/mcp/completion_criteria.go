package mcp

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type UpdateCompletionCriteriaArgs struct {
	CompletionCriteria []string `json:"completion_criteria" jsonschema:"non-empty list of verifiable criteria covering the original goal without expanding its scope"`
}

func registerUpdateCompletionCriteriaTool(server *sdk.Server) {
	sdk.AddTool(server, &sdk.Tool{
		Name:        string(config.UpdateCompletionCriteriaMCPToolName),
		Description: "Set the run completion criteria from the original goal and gathered context. You may call other tools to gather context first. Call alone before returning the final result. The host saves the criteria in context and removes this tool after success. Example arguments: {\"completion_criteria\":[\"Report verified source content\"]}.",
		InputSchema: toolInputSchema[UpdateCompletionCriteriaArgs](),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, args UpdateCompletionCriteriaArgs) (*sdk.CallToolResult, UpdateCompletionCriteriaArgs, error) {
		if err := ctx.Err(); err != nil {
			return nil, UpdateCompletionCriteriaArgs{}, err
		}
		if len(args.CompletionCriteria) == 0 {
			return nil, UpdateCompletionCriteriaArgs{}, fmt.Errorf("completion_criteria must be non-empty")
		}
		for _, criterion := range args.CompletionCriteria {
			if strings.TrimSpace(criterion) == "" {
				return nil, UpdateCompletionCriteriaArgs{}, fmt.Errorf("completion_criteria contains blank criterion")
			}
		}
		// MCP 仅验证并返回数据；宿主负责状态更新，兼容独立 stdio 服务。
		return nil, UpdateCompletionCriteriaArgs{CompletionCriteria: slices.Clone(args.CompletionCriteria)}, nil
	})
}
