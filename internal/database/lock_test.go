package database_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/database"
)

func TestFreshMigrationCreatesAllForeignKeys(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	name := fmt.Sprintf("codex_migration_%d", time.Now().UnixNano())
	if err := db.Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP DATABASE " + name)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	fresh, err := database.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(fresh)
	if err := database.Migrate(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	for _, constraint := range []struct {
		model any
		name  string
	}{
		{&database.AgentRun{}, "fk_agent_run_conversation"},
		{&database.StepExecution{}, "fk_step_execution_agent_run"},
	} {
		if !fresh.Migrator().HasConstraint(constraint.model, constraint.name) {
			t.Errorf("first migration omitted %s", constraint.name)
		}
	}
	// 验证外键实际生效，而不只是元数据中存在名称。
	err = fresh.Create(&database.StepExecution{RunID: "missing", StepID: "orphan", Status: "pending"}).Error
	if err == nil || !strings.Contains(err.Error(), "1452") {
		t.Fatalf("orphan step accepted or wrong error: %v", err)
	}
}

func TestRunLockReleaseAfterCallerCancellation(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lock, err := store.AcquireRunLock(ctx, "cancel-release-test")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := lock.Release(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := store.AcquireRunLock(context.Background(), "cancel-release-test")
	if err != nil {
		t.Fatalf("canceled caller left lock held: %v", err)
	}
	if err := next.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}
