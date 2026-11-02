package provider

import (
	"context"
	"encoding/json"
)

type LLMOutputParser[T any] interface {
	// model must be a non-nil pointer to the destination.
	Parse(text string, model T) error
}

type OutputKind string

const (
	OutputSchema      OutputKind = "schema"
	OutputContext     OutputKind = "context"
	OutputAnswerDelta OutputKind = "answer_delta"
	OutputTextDelta   OutputKind = "text_delta"
	OutputText        OutputKind = "text"
	OutputToolCall    OutputKind = "tool_call"
	OutputToolResult  OutputKind = "tool_result"
	OutputToolError   OutputKind = "tool_error"
	OutputUsage       OutputKind = "usage"
	OutputRawResponse OutputKind = "raw_response"
	OutputTaskInfo    OutputKind = "task_info"
)

// isDeltaOutput 判断事件是否为流式正文增量。
// 这类事件量大且属于临时内容，只有 debug 级别才回显到 stderr，避免刷屏。
func isDeltaOutput(kind OutputKind) bool {
	return kind == OutputTextDelta || kind == OutputAnswerDelta
}

// ToolResultOutput 用 call_id 关联工具调用；Result 保存原生 MCP content JSON。
// 失败事件同时保留能获取到的结果及错误详情。
type ToolResultOutput struct {
	Name   string          `json:"name"`
	CallID string          `json:"call_id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type TaskInfoOutput struct {
	ModelName string `json:"model_name"`
	RunID     string `json:"run_id"`
	ConvID    string `json:"conv_id"`
}

type OutputEvent struct {
	TaskID string     `json:"task_id"`
	Label  string     `json:"label,omitempty"`
	Kind   OutputKind `json:"kind"`

	// Data 是对应 Kind 的 JSON 数据，不包含 stderr 的展示格式。
	Data json.RawMessage `json:"data"`
}

type Outputer interface {
	// Emit 同步提交事件，返回输出目标的写入错误。
	// 同一调用链按调用顺序提交；共享实现应支持并发调用。
	Emit(ctx context.Context, event OutputEvent) error
}
