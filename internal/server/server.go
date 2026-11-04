package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/collie-agent/codex-go/internal/database"
	"gorm.io/gorm"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/collie-agent/codex-go/internal/entity"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/gin-gonic/gin"
)

type Backend interface {
	RPush(context.Context, string, string) (int64, error)
	ReadTaskStream(context.Context, string, string) ([]*entity.StreamEvent, error)
	RequestInterrupt(context.Context, string) (string, error)
}
type Store interface {
	ConversationRuns(context.Context, string) ([]database.ConversationRun, error)
}

func New(backend Backend, store Store) http.Handler {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		start := time.Now()
		// SSE 请求可能长时间保持连接，因此先记录请求到达，再记录结束状态。
		if c.Request.Method == http.MethodPost && c.Request.Body != nil {
			body, err := io.ReadAll(c.Request.Body)
			// 恢复请求体，供后续处理器读取；读取失败时保留已读取的数据。
			if err == nil {
				c.Request.Body = io.NopCloser(bytes.NewReader(body))
			} else {
				c.Request.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), c.Request.Body))
			}
			log.Printf("HTTP request started: method=%s path=%s body=%q read_error=%v", c.Request.Method, c.Request.URL.Path, body, err)
		} else {
			log.Printf("HTTP request started: method=%s path=%s", c.Request.Method, c.Request.URL.Path)
		}
		defer func() {
			log.Printf("HTTP request finished: method=%s path=%s status=%d duration=%s", c.Request.Method, c.Request.URL.Path, c.Writer.Status(), time.Since(start))
		}()
		c.Next()
	})
	router.GET("/conversations/:conv_id/runs", func(c *gin.Context) {
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "conversation history unavailable"})
			return
		}
		runs, err := store.ConversationRuns(c.Request.Context(), c.Param("conv_id"))
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "conversation not found"})
			return
		}
		if err != nil {
			log.Printf("read conversation runs: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot read conversation runs"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"conv_id": c.Param("conv_id"), "runs": runs})
	})
	router.POST("/runs/:run_id/interrupt", func(c *gin.Context) {
		result, err := backend.RequestInterrupt(c.Request.Context(), c.Param("run_id"))
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}
		status := http.StatusAccepted
		switch result {
		case "not_found":
			status = http.StatusNotFound
		case "terminal", "already_requested":
			status = http.StatusConflict
		case "unavailable":
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, gin.H{"status": result})
	})
	router.POST("/tasks", func(c *gin.Context) {
		var req struct {
			Input  string `json:"input"`
			RunId  string `json:"run_id"`
			ConvId string `json:"conv_id"`
		}
		if c.ShouldBindJSON(&req) != nil || !(entity.TaskMessage{Input: req.Input, RunId: req.RunId}).ValidInput() {
			c.JSON(400, gin.H{"error": "provide either input or run_id"})
			return
		}
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			c.JSON(500, gin.H{"error": "cannot generate task ID"})
			return
		}
		id := hex.EncodeToString(b)
		// 可选会话 ID 随任务入队，由消费者传给 application.Run。
		data, _ := json.Marshal(entity.TaskMessage{Input: req.Input, TaskId: id, ConvId: req.ConvId, RunId: req.RunId})
		if _, err := backend.RPush(c.Request.Context(), rdb.TASK_QUEUE_NAME, string(data)); err != nil {
			c.JSON(500, gin.H{"error": "cannot enqueue task"})
			return
		}
		c.JSON(201, gin.H{"task_id": id})
	})
	router.GET("/tasks/:task_id/events", func(c *gin.Context) { stream(c, backend) })
	return router
}

func stream(c *gin.Context, backend Backend) {
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
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-results:
			if r.err != nil {
				return
			}
			for _, event := range r.events {
				// HTTP 事件使用独立的 JSON 协议，不改变内部事件或队列格式。
				data, _ := json.Marshal(struct {
					RunID                 string `json:"run_id"`
					AgentInstanceID       string `json:"agent_instance_id"`
					ParentAgentInstanceID string `json:"parent_agent_instance_id"`
					AgentName             string `json:"agent_name"`
					ID                    string `json:"id"`
					Type                  string `json:"type"`
					Delta                 string `json:"delta"`
				}{ID: event.ID, Type: event.Type, Delta: event.Delta, RunID: event.RunID, AgentInstanceID: event.AgentInstanceID, ParentAgentInstanceID: event.ParentAgentInstanceID, AgentName: event.AgentName})
				if _, err := fmt.Fprintf(c.Writer, "id: %s\ndata: %s\n\n", event.ID, data); err != nil {
					return
				}
			}
			c.Writer.Flush()
		}
	}
}
