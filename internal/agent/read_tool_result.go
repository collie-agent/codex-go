package agent

import (
	"encoding/json"
	"fmt"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func readToolResultDefinition() *sdk.Tool {
	return &sdk.Tool{
		Name:        string(config.ReadToolResultMCPToolName),
		Description: `Read original tool output from this execution instance by call_id, including records covered by the execution summary. offset is a zero-based Unicode character offset, limit is the maximum character count (1..65536). Continue with next_offset when has_more is true. Returns original tool_name, status, arguments, content and error. Does not execute the original tool again.`,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"call_id": map[string]any{"type": "string", "minLength": 1},
			"offset":  map[string]any{"type": "integer", "minimum": 0},
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 65536},
		}, "required": []string{"call_id", "offset", "limit"}, "additionalProperties": false},
	}
}

// readToolResult 在执行记录锁内读取快照，避免与同批其他工具的完成写入竞争。
func (r *BaseReactProcess[T]) readToolResult(param *sdk.CallToolParams, durable *state.DurableState) *sdk.CallToolResult {
	fail := func(err error) *sdk.CallToolResult {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}}
	}
	data, err := json.Marshal(param.Arguments)
	if err != nil {
		return fail(err)
	}
	var args struct {
		CallID string `json:"call_id"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(data, &args); err != nil {
		return fail(err)
	}
	if args.Offset < 0 || args.Limit < 1 || args.Limit > 65536 {
		return fail(fmt.Errorf("offset must be nonnegative and limit must be between 1 and 65536"))
	}
	r.executionMu.Lock()
	defer r.executionMu.Unlock()
	if durable != nil {
		for _, call := range durable.ToolExecutions {
			if call == nil || call.ToolCallId != args.CallID {
				continue
			}
			if call.Status == "" {
				return fail(fmt.Errorf("unknown tool outcome: call_id=%q", args.CallID))
			}
			// 使用字符偏移，保证中文和其他多字节字符不会在分页边界被截断。
			content := []rune(call.Result)
			offset := min(args.Offset, len(content))
			end := offset + min(args.Limit, len(content)-offset)
			result := struct {
				CallID     string `json:"call_id"`
				ToolName   string `json:"tool_name"`
				Status     string `json:"status"`
				Arguments  string `json:"arguments"`
				Content    string `json:"content"`
				Error      string `json:"error,omitempty"`
				NextOffset int    `json:"next_offset"`
				HasMore    bool   `json:"has_more"`
			}{call.ToolCallId, call.ToolName, string(call.Status), call.Arguments, string(content[offset:end]), call.Error, end, end < len(content)}
			encoded, err := json.Marshal(result)
			if err != nil {
				return fail(err)
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(encoded)}}}
		}
	}
	return fail(fmt.Errorf("tool result not found in current execution: call_id=%q", args.CallID))
}
