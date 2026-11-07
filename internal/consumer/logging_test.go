package consumer_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/consumer"
	"github.com/collie-agent/codex-go/internal/testutil"
)

func TestConsumerRejectsUnwritableLogDirectoryBeforeConsuming(t *testing.T) {
	_, addr := testutil.StartRedis(t)
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := consumer.Run(ctx, application.Options{ConfigPath: configuration(t, addr, "http://127.0.0.1:1"), Workspace: t.TempDir(), Timeout: time.Second, LogDir: path})
	if err == nil || !strings.Contains(err.Error(), "log") {
		t.Fatalf("expected log destination error, got %v", err)
	}
}
