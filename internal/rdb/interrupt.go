package rdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const interruptPrefix = "codex-go-interrupt:"

// SetRunStatusLookup configures the HTTP-side read-only run lookup.
func (r *RedisManager) SetRunStatusLookup(lookup func(context.Context, string) (string, error)) {
	r.runStatus = lookup
}

// RequestInterrupt only reports acceptance after the lock holder acknowledges it.
func (r *RedisManager) RequestInterrupt(ctx context.Context, runID string) (string, error) {
	if r.runStatus == nil {
		return "", errors.New("run lookup unavailable")
	}
	status, err := r.runStatus(ctx, runID)
	if err != nil {
		return "", err
	}
	if status == "" {
		return "not_found", nil
	}
	if status == "completed" || status == "interrupted" || status == "failed" {
		return "terminal", nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	reply := interruptPrefix + "reply:" + hex.EncodeToString(nonce[:])
	defer r.client.Del(context.Background(), reply)
	count, err := r.client.Publish(ctx, interruptPrefix+runID, reply).Result()
	if err != nil {
		return "", err
	}
	if count == 0 {
		return "unavailable", nil
	}
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := r.client.BLPop(wait, time.Second*2, reply).Result()
	if errors.Is(err, redis.Nil) || errors.Is(err, context.DeadlineExceeded) {
		return "unavailable", nil
	}
	if err != nil {
		return "", err
	}
	if len(result) != 2 {
		return "", fmt.Errorf("invalid interrupt acknowledgement")
	}
	return result[1], nil
}

// ListenInterrupt subscribes before publishing the run identity, and only the
// execution holding the database run lock installs this listener.
func ListenInterrupt(ctx context.Context, client redis.UniversalClient, runID string, interrupt interface{ TryInterrupt() string }) (func(), error) {
	sub := client.Subscribe(ctx, interruptPrefix+runID)
	if _, err := sub.Receive(ctx); err != nil {
		sub.Close()
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for msg := range sub.Channel() {
			result := interrupt.TryInterrupt()
			ackCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			client.Eval(ackCtx, `redis.call('RPUSH', KEYS[1], ARGV[1]); return redis.call('EXPIRE', KEYS[1], 5)`, []string{msg.Payload}, result)
			cancel()
		}
	}()
	return func() { sub.Close(); <-done }, nil
}
