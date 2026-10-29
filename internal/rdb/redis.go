package rdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/collie-agent/codex-go/internal/entity"
	"github.com/redis/go-redis/v9"
)

const (
	TASK_QUEUE_NAME = "codex-go-task-queue"
	STREAM_NAME     = "codex-go-resp-stream"
)

type RedisManager struct {
	client redis.UniversalClient
}

// NewRedisManagerWithClient 复用调用方配置的连接；连接生命周期由调用方管理。
func NewRedisManagerWithClient(client redis.UniversalClient) *RedisManager {
	return &RedisManager{client: client}
}

func (r *RedisManager) WriteTaskStream(ctx context.Context, taskId string, resp *entity.LLMResponse) (string, error) {
	stream := fmt.Sprintf("%s-%s", STREAM_NAME, taskId)
	id, err := r.client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{
			"type":  resp.Type,
			"delta": resp.Delta,
		},
	}).Result()
	if err != nil {
		return "", err
	}
	if err := r.client.Expire(ctx, stream, 24*time.Hour).Err(); err != nil {
		return "", err
	}
	return id, nil
}

// ReadTaskStream reads response events for taskID after lastID. Redis Stream
// IDs are exclusive, so passing the ID from the last received SSE event makes
// reconnects resume without duplicating that event. A zero block keeps the
// request open until new data arrives or the request context is cancelled.
func (r *RedisManager) ReadTaskStream(ctx context.Context, taskID, lastID string) ([]*entity.StreamEvent, error) {
	if r == nil || r.client == nil {
		return nil, errors.New("redis: manager is not configured")
	}
	if taskID == "" {
		return nil, errors.New("redis: task ID is empty")
	}
	if lastID == "" {
		lastID = "0-0"
	}

	result, err := r.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{fmt.Sprintf("%s-%s", STREAM_NAME, taskID), lastID},
		Count:   10,
		Block:   0,
	}).Result()
	if err != nil {
		return nil, err
	}

	events := make([]*entity.StreamEvent, 0)
	for _, stream := range result {
		for _, message := range stream.Messages {
			event := &entity.StreamEvent{ID: message.ID}
			if value, ok := message.Values["type"].(string); ok {
				event.Type = value
			}
			if value, ok := message.Values["delta"].(string); ok {
				event.Delta = value
			}
			events = append(events, event)
		}
	}
	return events, nil
}

func (r *RedisManager) BLPop(ctx context.Context, queueName string) ([]string, error) {
	values, err := r.client.BLPop(ctx, 0, queueName).Result()
	if err != nil {
		return nil, err
	}
	if len(values) < 2 {
		return nil, errors.New("mq: invalid BLPOP response")
	}
	return values, nil
}

func (r *RedisManager) RPush(ctx context.Context, queueName, value string) (int64, error) {
	return r.client.RPush(ctx, queueName, value).Result()
}

func (r *RedisManager) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

func (r *RedisManager) Del(ctx context.Context, keys ...string) error {
	return r.client.Del(ctx, keys...).Err()
}

func (r *RedisManager) Close() error {
	return r.client.Close()
}
