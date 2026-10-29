package database_test

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/database"
	"github.com/collie-agent/codex-go/internal/entity"
)

func TestSSEEventsCanBeWrittenAndReadByTask(t *testing.T) {
	url := os.Getenv("CODEX_TEST_MYSQL_URL")
	if url == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL for persistence integration tests")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close(db) })
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := database.NewSSEEventStore(db)
	taskID := time.Now().UTC().Format("20060102150405.000000000")
	want := []*entity.StreamEvent{{ID: "1-0", Type: "delta", Delta: "hello"}}
	if err := store.WriteSSEEvent(ctx, taskID, want); err != nil {
		t.Fatal(err)
	}
	// Retrying the same batch must not duplicate the events returned to callers.
	if err := store.WriteSSEEvent(ctx, taskID, want); err != nil {
		t.Fatal(err)
	}
	got, lastID, err := store.ReadSSEEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || lastID != "1-0" {
		t.Fatalf("events=%+v, last ID=%q", got, lastID)
	}
	other, _, err := store.ReadSSEEvents(ctx, taskID+"-other")
	if err != nil || len(other) != 0 {
		t.Fatalf("another task returned events=%+v, error=%v", other, err)
	}
}
