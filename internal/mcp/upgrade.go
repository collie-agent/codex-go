package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SwitchToPlanExecuteArgs carries the remaining work across the execution boundary.
type SwitchToPlanExecuteArgs struct {
	Reason  string `json:"reason" jsonschema:"why this goal needs multiple dependent independently verifiable stages"`
	Summary string `json:"summary" jsonschema:"verified findings, completed actions, and remaining work; distinguish facts from assumptions"`
}

type SwitchToPlanExecuteResult struct {
	Mode    string `json:"mode"`
	Reason  string `json:"reason"`
	Summary string `json:"summary"`
}

// SwitchToPlanExecute 只返回切换请求；运行状态由宿主编排器更新，兼容 stdio MCP。
func SwitchToPlanExecute(ctx context.Context, args SwitchToPlanExecuteArgs) (SwitchToPlanExecuteResult, error) {
	if err := ctx.Err(); err != nil {
		return SwitchToPlanExecuteResult{}, err
	}
	if strings.TrimSpace(args.Reason) == "" || strings.TrimSpace(args.Summary) == "" {
		return SwitchToPlanExecuteResult{}, fmt.Errorf("reason and summary must be non-empty")
	}
	return SwitchToPlanExecuteResult{Mode: "plan-execute", Reason: args.Reason, Summary: args.Summary}, nil
}

func registerSwitchToPlanExecuteTool(server *sdk.Server) {
	sdk.AddTool(server, &sdk.Tool{
		Name:        string(config.SwitchToPlanExecuteMCPToolName),
		Description: "Switch the current ReAct run to plan-execute when remaining work needs multiple dependent, independently verifiable stages or explicit progress tracking. Inspect the workspace first when needed. Do not switch merely because several tool calls are needed. Call this tool alone; the host will hand control to the planner. Unavailable inside a plan step. Example arguments: {\"reason\":\"Dependent implementation and verification stages\",\"summary\":\"Inspected the API; implementation and tests remain\"}.",
		InputSchema: toolInputSchema[SwitchToPlanExecuteArgs](),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, args SwitchToPlanExecuteArgs) (*sdk.CallToolResult, SwitchToPlanExecuteResult, error) {
		result, err := SwitchToPlanExecute(ctx, args)
		return nil, result, err
	})
}
