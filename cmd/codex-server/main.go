// Command codex-server provides task creation and event streaming APIs.
package main

import (
	"context"
	"flag"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"errors"
	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/logging"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/server"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

func main() {
	logDir := flag.String("log-dir", "", "log directory (default ./logs)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	redisURL := flag.String("redis-url", "redis://localhost:6379/0", "Redis URL")
	logging.ParseFlags()
	if err := logging.Setup(*logDir, ""); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	// Gin 自身的路由/恢复诊断也走文件日志，HTTP/SSE 正文仍写入响应。
	gin.DefaultWriter = logging.DiagnosticWriter{Logger: log.Default()}
	gin.DefaultErrorWriter = logging.DiagnosticWriter{Logger: log.Default()}
	opts, err := redis.ParseURL(*redisURL)
	if err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	client := redis.NewClient(opts)
	defer client.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = client.Ping(ctx).Err(); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	db, err := database.OpenFromEnv(ctx)
	if err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
	defer database.Close(db)
	backend := rdb.NewRedisManagerWithClient(client)
	runs := database.NewRunStore(db)
	backend.SetRunStatusLookup(func(ctx context.Context, runID string) (string, error) {
		run, err := runs.Get(ctx, runID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		// A recoverable failed run may currently be held by a resumed executor.
		if err == nil && run.Status == "failed" && run.FailureType != "business" {
			return "running", nil
		}
		return run.Status, err
	})
	srv := &http.Server{Addr: *addr, Handler: server.New(backend, runs), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(log.Writer(), "[ERROR] ", log.LstdFlags|log.Lmsgprefix)}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = srv.Shutdown(shutdown)
	}()
	if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}
