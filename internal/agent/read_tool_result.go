package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolResultReader 按运行和执行实例隔离查询，避免相同 call_id 串读其他任务。
type ToolResultReader interface {
	ToolResult(context.Context, string, string, string) (*state.ToolResult, error)
}

type toolResultReaderKey struct{}

// 限制重复调用的缓存预览长度，避免大结果挤占上下文。
const duplicateResultPreviewLimit = 4096

// cachedDuplicateResult 用新的 call_id 配对回传历史结果，不登记或执行新的工具调用。
func (r *BaseReactProcess[T]) cachedDuplicateResult(durable *state.DurableState, original, id, name, arguments string) *state.ToolResult {
	result := &state.ToolResult{ToolCallId: id, ToolName: name, Arguments: arguments}
	stored := r.readToolResult(&sdk.CallToolParams{Arguments: map[string]any{"call_id": original, "offset": 0, "limit": duplicateResultPreviewLimit}}, durable)
	for _, content := range stored.Content {
		text, ok := content.(*sdk.TextContent)
		if !ok {
			continue
		}
		if stored.IsError {
			result.Error = text.Text
			return result
		}
		var preview struct {
			Content    string `json:"content"`
			Error      string `json:"error"`
			NextOffset int    `json:"next_offset"`
			HasMore    bool   `json:"has_more"`
		}
		if err := json.Unmarshal([]byte(text.Text), &preview); err != nil {
			result.Error = fmt.Sprintf("decode cached tool result: %v", err)
			return result
		}
		// 解开回读接口的 JSON 信封，原结果以工具输出回传，避免多层转义。
		result.Result, result.Error = preview.Content, preview.Error
		if preview.HasMore {
			result.Result += fmt.Sprintf("\n\nCached output truncated. Continue with read_tool_result using call_id=%q, offset=%d, limit=%d.", original, preview.NextOffset, duplicateResultPreviewLimit)
		}
		return result
	}
	result.Error = "cached tool result contained no text"
	return result
}

// WithToolResultReader 为本次运行及其子执行提供持久化结果读取能力。
func WithToolResultReader(ctx context.Context, reader ToolResultReader) context.Context {
	return context.WithValue(ctx, toolResultReaderKey{}, reader)
}

func readToolResultDefinition() *sdk.Tool {
	return &sdk.Tool{
		Name:        string(config.ReadToolResultMCPToolName),
		Description: `Read stored tool output from this execution instance by call_id, including records covered by the execution summary. Long exec outputs are stored as summaries; their raw output is discarded and cannot be recovered. offset is a zero-based Unicode character offset, limit is the maximum character count (1..65536). Continue with next_offset when has_more is true. Returns source call_id, actual offset and next_offset, eof, has_more, tool_name, status, arguments, stored content and error. Does not execute the original tool again.`,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"call_id": map[string]any{"type": "string", "minLength": 1},
			"offset":  map[string]any{"type": "integer", "minimum": 0},
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 65536},
		}, "required": []string{"call_id", "offset", "limit"}, "additionalProperties": false},
	}
}

// readToolResult 优先查询持久化结果；未接入存储的独立执行使用内存快照。
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
		calls := durable.ToolExecutions
		if reader, ok := r.ctx.Value(toolResultReaderKey{}).(ToolResultReader); ok {
			call, err := reader.ToolResult(r.ctx, durable.RunId, durable.ExecutionID, args.CallID)
			if err != nil {
				return fail(fmt.Errorf("read stored tool result: %w", err))
			}
			calls = []*state.ToolResult{call}
		}
		for _, call := range calls {
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
				Offset     int    `json:"offset"`
				EOF        bool   `json:"eof"`
			}{call.ToolCallId, call.ToolName, string(call.Status), call.Arguments, string(content[offset:end]), call.Error, end, end < len(content), offset, end == len(content)}
			encoded, err := json.Marshal(result)
			if err != nil {
				return fail(err)
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(encoded)}}, StructuredContent: &state.HistoryReadEvidence{SourceCallID: call.ToolCallId, Start: offset, End: end, EOF: end == len(content)}}
		}
	}
	return fail(fmt.Errorf("tool result not found in current execution: call_id=%q", args.CallID))
}
