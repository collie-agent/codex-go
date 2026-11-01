package provider

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/redis/go-redis/v9"
)

// NewConfiguredOutputer 在 Redis 启用时组合 stderr 和 Stream 输出。
// 返回的 cleanup 由创建者在运行结束后调用，复用期间 Emit 支持并发。
// 未配置 Redis 时返回 nil，保留 provider 原有的默认输出行为。
func NewConfiguredOutputer(cfg *config.RedisConfig, level config.LogLevel) (Outputer, func()) {
	if cfg == nil {
		return nil, func() {}
	}
	client := redis.NewClient(&redis.Options{
		Addr: cfg.Addr, Username: cfg.Username, Password: cfg.Password, DB: cfg.DB,
		// 输出失败不重发，避免重试 XADD 写出重复事件；限制旁路故障的等待时间。
		MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		ContextTimeoutEnabled: true,
	})
	return &configuredOutputer{stderr: NewStderrOutputer(nil), stream: NewRedisOutputer(rdb.NewRedisManagerWithClient(client)), level: level}, func() { _ = client.Close() }
}

type configuredOutputer struct {
	stderr Outputer
	stream Outputer
	level  config.LogLevel
}

func (o *configuredOutputer) Emit(ctx context.Context, event OutputEvent) error {
	var logErr error
	// 工具结果/错误保留 agent 的现有日志路径，避免在 stderr 重复打印；
	// 正文增量只在 debug 级别回显，但始终写入 Stream 供消费者使用。
	if event.Kind != OutputToolResult && event.Kind != OutputToolError && !(event.Kind == OutputText && o.level.InfoEnabled()) && o.stderrVisible(event.Kind) {
		logErr = o.stderr.Emit(ctx, event)
	}
	return errors.Join(logErr, o.stream.Emit(ctx, event))
}

// stderrVisible 与默认输出器保持同一套日志级别规则，避免两条路径行为不一致。
func (o *configuredOutputer) stderrVisible(kind OutputKind) bool {
	return !isDeltaOutput(kind) || o.level == config.DebugLogLevel
}

// NewTaskOutputer directs task metadata to the supplied log writer and configured stream.
func NewTaskOutputer(cfg *config.RedisConfig, level config.LogLevel, writer io.Writer) (Outputer, func()) {
	output, cleanup := NewConfiguredOutputer(cfg, level)
	if output == nil {
		return NewStderrOutputer(writer), cleanup
	}
	output.(*configuredOutputer).stderr = NewStderrOutputer(writer)
	return output, cleanup
}
