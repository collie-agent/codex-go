// Command codex-server provides task creation and event streaming APIs.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/server"
	"github.com/redis/go-redis/v9"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	redisURL := flag.String("redis-url", "redis://localhost:6379/0", "Redis URL")
	interval := flag.Duration("flush-interval", time.Second, "event persistence interval")
	flag.Parse()
	opts, err := redis.ParseURL(*redisURL)
	if err != nil {
		log.Fatal(err)
	}
	client := redis.NewClient(opts)
	defer client.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = client.Ping(ctx).Err(); err != nil {
		log.Fatal(err)
	}
	db, err := database.OpenFromEnv(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer database.Close(db)
	srv := &http.Server{Addr: *addr, Handler: server.New(rdb.NewRedisManagerWithClient(client), database.NewSSEEventStore(db), *interval), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = srv.Shutdown(shutdown)
	}()
	if err = srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
