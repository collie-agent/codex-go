package database

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func TestMySQLDisconnectReleasesRunLock(t *testing.T) {
	url := os.Getenv("CODEX_TEST_MYSQL_URL")
	if url == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for run lock integration tests")
	}
	ctx := context.Background()
	db, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Close(db) })
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRunStore(db)
	run, err := store.Create(ctx, "", "recover after connection loss", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.AcquireRunLock(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var connectionID uint64
	if err := lock.conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connectionID); err != nil {
		t.Fatal(err)
	}
	// KILL releases connection-scoped GET_LOCK exactly as a process crash does.
	if err := db.Exec(fmt.Sprintf("KILL %d", connectionID)).Error; err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(ctx); err == nil {
		t.Fatal("release unexpectedly succeeded after the server killed its connection")
	}
	second, err := store.AcquireRunLock(ctx, run.RunID)
	if err != nil {
		t.Fatalf("database did not release the disconnected owner's lock: %v", err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
