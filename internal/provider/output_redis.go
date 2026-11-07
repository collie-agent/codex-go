package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/entity"
)

// TaskStreamWriter 将 Stream 协议和过期策略交给 rdb 管理。
type TaskStreamWriter interface {
	WriteTaskStream(context.Context, string, *entity.LLMResponse) (string, error)
}

// RedisOutputer 使用调用方持有的 Stream writer，不负责关闭连接。
type RedisOutputer struct{ writer TaskStreamWriter }

type taskStreamIDKey struct{}

// WithTaskStreamID groups all output from one queued task, including delegated
// agent runs, in the producer's response stream without changing run identities.
func WithTaskStreamID(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, taskStreamIDKey{}, taskID)
}

func NewRedisOutputer(writer TaskStreamWriter) *RedisOutputer {
	return &RedisOutputer{writer: writer}
}

// Emit 的 entry ID 由 Redis 生成，消费者从 XREAD/XRANGE 的 ID 字段读取。
func (o *RedisOutputer) Emit(ctx context.Context, event OutputEvent) error {
	if taskID, ok := ctx.Value(taskStreamIDKey{}).(string); ok && taskID != "" {
		event.TaskID = taskID
	}
	switch event.Kind {
	case OutputSchema, OutputContext, OutputRawResponse:
		return nil
	case OutputReset, OutputTaskInfo, OutputAnswerDelta, OutputText, OutputTextDelta, OutputToolCall, OutputToolResult, OutputToolError, OutputUsage, OutputAgentSwitch, OutputKind("agent_lifecycle"), OutputKind("task_completed"), OutputKind("task_interrupted"):
	default:
		return fmt.Errorf("redis output: unsupported event type %q", event.Kind)
	}
	if strings.TrimSpace(event.TaskID) == "" {
		return fmt.Errorf("redis output: task ID is required")
	}
	if !json.Valid(event.Data) {
		return fmt.Errorf("redis output: invalid JSON data for %s", event.Kind)
	}
	// JSON 作为 delta 原样传递，供 ReadTaskStream 和 SSE 消费者统一读取。
	_, err := o.writer.WriteTaskStream(ctx, event.TaskID, &entity.LLMResponse{
		Type: string(event.Kind), Delta: string(event.Data),
		RunID: event.RunID, AgentInstanceID: event.AgentInstanceID, ParentAgentInstanceID: event.ParentAgentInstanceID, AgentName: event.AgentName,
	})
	return err
}
