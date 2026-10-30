package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// ToolCallOutput 保留调用信息；Arguments 是模型提供的原始 JSON 字符串。
type ToolCallOutput struct {
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
}

// UsageOutput 使用字符串保留 unknown 与显式 0 的区别，空计数展示为 unknown。
type UsageOutput struct {
	Model               string `json:"model"`
	ResponseID          string `json:"response_id,omitempty"`
	Status              string `json:"status"`
	InputTokens         string `json:"input_tokens"`
	OutputTokens        string `json:"output_tokens"`
	CachedInputTokens   string `json:"cached_input_tokens"`
	UncachedInputTokens string `json:"uncached_input_tokens"`
}

// TextOutput 用于完整文本和流式文本片段。
type TextOutput struct {
	Text string `json:"text"`
}

// ContextOutput 保存已经格式化的请求上下文。
type ContextOutput struct {
	Context string `json:"context"`
}

// RawResponseOutput 保留 Responses API 返回的原始 HTTP 正文。
type RawResponseOutput struct {
	Body string `json:"body"`
}

// StderrOutputer 将结构化事件呈现为可读日志；必须通过 NewStderrOutputer 创建。
// OutputSchema.Data 是 schema 本身；其余 Data 分别编码对应的 *Output 结构。
// 时间前缀使用本地时间，与标准 logger 的日期时间格式一致。
// 不进行日志级别过滤，也不关闭注入的 writer。
type StderrOutputer struct {
	writer io.Writer
	mu     sync.Mutex
}

// NewStderrOutputer 的 writer 为 nil 时使用 os.Stderr。
func NewStderrOutputer(writer io.Writer) *StderrOutputer {
	if writer == nil {
		writer = os.Stderr
	}
	return &StderrOutputer{writer: writer}
}

// Emit 同步写入完整事件。普通 io.Writer 的阻塞写入无法通过 ctx 中断。
func (o *StderrOutputer) Emit(ctx context.Context, event OutputEvent) error {
	prefix := "task " + event.TaskID + ": "
	if event.Label != "" {
		prefix += "[" + event.Label + "] "
	}
	var message string
	switch event.Kind {
	case OutputTaskInfo:
		var data TaskInfoOutput
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		message = fmt.Sprintf("run_id=%s conv_id=%s", data.RunID, data.ConvID)
	case OutputText:
		var data TextOutput
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		message = "LLM response text=" + data.Text
	case OutputTextDelta, OutputAnswerDelta:
		var data TextOutput
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		message = "LLM response text delta=" + data.Text
		if event.Kind == OutputAnswerDelta {
			message = "LLM answer delta=" + data.Text
		}
	case OutputContext:
		var data ContextOutput
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		message = "LLM context=\n" + data.Context
	case OutputRawResponse:
		var data RawResponseOutput
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		message = "LLM raw response=\n" + data.Body
	case OutputSchema:
		var data bytes.Buffer
		if err := json.Indent(&data, event.Data, "", "  "); err != nil {
			return err
		}
		message = "JSON schema=\n" + data.String()
	case OutputToolCall:
		var data ToolCallOutput
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		message = fmt.Sprintf("tool calling name=%s call_id=%s arguments=%s", data.Name, data.CallID, data.Arguments)
	case OutputToolResult, OutputToolError:
		var data ToolResultOutput
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		if event.Kind == OutputToolError {
			message = fmt.Sprintf("tool calling error: name=%s call_id=%s error=%s result=%s", data.Name, data.CallID, data.Error, data.Result)
		} else {
			message = fmt.Sprintf("tool calling result: name=%s call_id=%s result=%s", data.Name, data.CallID, data.Result)
		}
	case OutputUsage:
		var data UsageOutput
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return err
		}
		responseID := ""
		if data.ResponseID != "" {
			responseID = " response_id=" + data.ResponseID
		}
		count := func(value string) string {
			if value == "" {
				return "unknown"
			}
			return value
		}
		message = fmt.Sprintf("LLM usage model=%s%s status=%s input_tokens=%s output_tokens=%s cached_input_tokens=%s uncached_input_tokens=%s", data.Model, responseID, data.Status, count(data.InputTokens), count(data.OutputTokens), count(data.CachedInputTokens), count(data.UncachedInputTokens))
	default:
		return fmt.Errorf("unsupported output kind %q", event.Kind)
	}
	// 锁覆盖整条事件的写入，避免同一输出器的并发事件交错。
	o.mu.Lock()
	defer o.mu.Unlock()
	line := time.Now().Format("2006/01/02 15:04:05") + " " + prefix + message + "\n"
	if event.Kind == OutputTaskInfo {
		line = message + "\n"
	}
	n, err := io.WriteString(o.writer, line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
}
