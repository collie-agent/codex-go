package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/database"
)

func TestCLIInterruptSavesRun(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	db, err := database.Open(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex-cli")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	config := filepath.Join(dir, "agent.yaml")
	yaml := "name: react\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: executor\n"
	if err := os.WriteFile(config, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", config, "--workspace", dir, "--task", "finish task", "--mcp-command", "")
	cmd.Env = append(os.Environ(), "CODEX_MYSQL_URL="+raw)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("model did not start")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("CLI did not stop")
	}
	var run database.AgentRun
	if err := db.Where("workspace = ?", dir).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != "interrupted" || !strings.Contains(stderr.String(), "interrupted") {
		t.Fatalf("status=%q stderr=%s", run.Status, stderr.String())
	}
}
