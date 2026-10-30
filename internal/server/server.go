package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/collie-agent/codex-go/internal/entity"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/gin-gonic/gin"
)

type Backend interface {
	RPush(context.Context, string, string) (int64, error)
	ReadTaskStream(context.Context, string, string) ([]*entity.StreamEvent, error)
}
type Store interface {
	WriteSSEEvent(context.Context, string, []*entity.StreamEvent) error
}

func New(backend Backend, store Store, interval time.Duration) http.Handler {
	if interval <= 0 {
		interval = time.Second
	}
	router := gin.New()
	router.POST("/tasks", func(c *gin.Context) {
		var req struct {
			Input  string `json:"input"`
			ConvId string
		}
		if c.ShouldBindJSON(&req) != nil || strings.TrimSpace(req.Input) == "" {
			c.JSON(400, gin.H{"error": "input is required"})
			return
		}
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			c.JSON(500, gin.H{"error": "cannot generate task ID"})
			return
		}
		id := hex.EncodeToString(b)
		// 可选会话 ID 随任务入队，由消费者传给 application.Run。
		data, _ := json.Marshal(entity.TaskMessage{Input: req.Input, TaskId: id, ConvId: req.ConvId})
		if _, err := backend.RPush(c.Request.Context(), rdb.TASK_QUEUE_NAME, string(data)); err != nil {
			c.JSON(500, gin.H{"error": "cannot enqueue task"})
			return
		}
		c.JSON(201, gin.H{"task_id": id})
	})
	router.GET("/tasks/:task_id/events", func(c *gin.Context) { stream(c, backend, store, interval) })
	return router
}

func stream(c *gin.Context, backend Backend, store Store, interval time.Duration) {
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	id := c.Query("last_event_id")
	if id == "" {
		id = c.GetHeader("Last-Event-ID")
	}
	if id == "" {
		id = "0-0"
	}
	task := c.Param("task_id")
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)
	c.Writer.Flush()
	type result struct {
		events []*entity.StreamEvent
		err    error
	}
	results := make(chan result)
	go func() {
		for {
			events, err := backend.ReadTaskStream(ctx, task, id)
			select {
			case results <- result{events, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
			if len(events) > 0 {
				id = events[len(events)-1].ID
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var pending []*entity.StreamEvent
	flush := func() bool {
		if len(pending) == 0 {
			return true
		}
		writeCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := store.WriteSSEEvent(writeCtx, task, pending); err != nil {
			log.Printf("persist SSE events for task %s: %v", task, err)
			return false
		}
		pending = nil
		return true
	}
	defer flush()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !flush() {
				return
			}
		case r := <-results:
			if r.err != nil {
				return
			}
			for _, event := range r.events {
				data, _ := json.Marshal(event)
				if _, err := fmt.Fprintf(c.Writer, "id: %s\ndata: %s\n\n", event.ID, data); err != nil {
					return
				}
				pending = append(pending, event)
			}
			c.Writer.Flush()
		}
	}
}
