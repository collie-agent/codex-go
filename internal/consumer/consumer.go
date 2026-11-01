// Package consumer executes agent tasks from the shared Redis List.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/config"
	"github.com/collie-agent/codex-go/internal/entity"
	"github.com/collie-agent/codex-go/internal/provider"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/redis/go-redis/v9"
)

// Run consumes tasks serially until ctx is cancelled.
func Run(ctx context.Context, opts application.Options) error {
	return RunWithConcurrency(ctx, opts, 1)
}

// RunWithConcurrency runs up to concurrency tasks at once.
func RunWithConcurrency(ctx context.Context, opts application.Options, concurrency int) error {
	if concurrency < 1 {
		return fmt.Errorf("--concurrency must be positive")
	}
	if strings.TrimSpace(opts.Workspace) == "" {
		return fmt.Errorf("--workspace is required")
	}
	if opts.Timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	loaded, err := config.LoadAgentConfig(opts.ConfigPath)
	if err != nil {
		return err
	}
	cfg := loaded.RedisConfig()
	if cfg == nil {
		return fmt.Errorf("consumer requires top-level redis configuration")
	}
	if opts.Mode == "" {
		opts.Mode = loaded.Name
	}
	// 启动参数错误必须在出队前暴露，避免每条有效消息都被取走后失败。
	if opts.Mode != "react" && opts.Mode != "plan-execute" && opts.Mode != "multi-agent" {
		return fmt.Errorf("unsupported mode %q: expected react, plan-execute or multi-agent", opts.Mode)
	}
	if (opts.Mode == "multi-agent") != (loaded.Name == "multi-agent") {
		return fmt.Errorf("multi-agent mode requires a multi-agent configuration and vice versa")
	}
	opts.Stderr = &lockedWriter{writer: opts.Stderr}
	client := redis.NewClient(&redis.Options{
		Addr: cfg.Addr, Username: cfg.Username, Password: cfg.Password, DB: cfg.DB,
		ContextTimeoutEnabled: true,
		// 完成事件使用 XADD，禁用自动重试以免响应丢失时重复发布。
		MaxRetries: -1,
	})
	defer client.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := consume(ctx, client, opts); err != nil {
				errs <- err
				cancel()
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return nil
	}
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func consume(ctx context.Context, client *redis.Client, opts application.Options) error {
	for ctx.Err() == nil {
		// 有限阻塞让空队列在取消后也能及时退出，避免 BLPOP 0 永久占用连接。
		values, err := client.BLPop(ctx, time.Second, rdb.TASK_QUEUE_NAME).Result()
		if err != nil && ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return fmt.Errorf("consume task: %w", err)
		}
		var message entity.TaskMessage
		if err := json.Unmarshal([]byte(values[1]), &message); err != nil {
			fmt.Fprintf(opts.Stderr, "consumer: invalid task message: %v\n", err)
			continue
		}
		if !message.ValidInput() || strings.TrimSpace(message.TaskId) == "" {
			fmt.Fprintln(opts.Stderr, "consumer: invalid task message: TaskId and either Input or RunId are required")
			continue
		}
		taskOpts := opts
		taskOpts.Interrupt = application.NewInterrupt()
		taskOpts.OnRunLocked = func(ctx context.Context, runID string, interrupt *application.Interrupt) (func(), error) {
			return rdb.ListenInterrupt(ctx, client, runID, interrupt)
		}
		taskOpts.Task = message.Input
		// 会话归属按消息指定；省略时由 application 为本任务创建新会话。
		taskOpts.ConvID = message.ConvId
		// 退出信号只停止领取；已经出队的任务必须运行完成，避免丢失。
		// 队列任务 ID 仅关联输出；持久化 run ID 仍由 application 生成。
		taskCtx := provider.WithTaskStreamID(context.WithoutCancel(ctx), message.TaskId)
		var results []agent.ReactResult
		var executeErr error
		if message.RunId != "" {
			taskOpts.RunID = message.RunId
			// 恢复采用原运行保存的工作目录及模式，不套用消费者的新任务默认值。
			taskOpts.Workspace = ""
			taskOpts.Mode = ""
			results, executeErr = application.Resume(taskCtx, taskOpts)
		} else {
			results, executeErr = application.Run(taskCtx, taskOpts)
		}
		var saveErr *application.InterruptPersistenceError
		interrupted := errors.Is(executeErr, application.ErrInterrupted) && !errors.As(executeErr, &saveErr)
		if executeErr != nil {
			err := executeErr
			fmt.Fprintf(opts.Stderr, "consumer: task_id=%q failed: %v\n", message.TaskId, err)
		}
		// 应用层仅在业务失败结果成功保存后返回结果；恢复终态失败也遵循此约定。
		businessFailed := hasBusinessFailure(results)
		if executeErr == nil || interrupted || businessFailed {
			eventType := "task_completed"
			if interrupted {
				eventType = "task_interrupted"
			}
			// 只在最终状态保存后发布终态，避免误报任务完成或中断完成。
			// 使用独立限时上下文，退出信号不应丢弃已领取任务的完成通知。
			publishCtx, cancel := context.WithTimeout(taskCtx, 5*time.Second)
			_, err := rdb.NewRedisManagerWithClient(client).WriteTaskStream(publishCtx, message.TaskId, &entity.LLMResponse{
				Type: eventType, Delta: "{}",
			})
			cancel()
			if err != nil {
				fmt.Fprintf(opts.Stderr, "consumer: task_id=%q publish %s failed: %v\n", message.TaskId, eventType, err)
			}
			if executeErr == nil {
				fmt.Fprintf(opts.Stderr, "consumer: task_id=%q completed\n", message.TaskId)
			}
		}
	}
	return nil
}

// 仅已保存的业务失败会由 application 返回对应结果。
func hasBusinessFailure(results []agent.ReactResult) bool {
	return len(results) > 0 && results[0].Status == agent.FailedReactStatus
}
