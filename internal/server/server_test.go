package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/entity"
)

type fakeBackend struct{ task entity.TaskMessage }

func (f *fakeBackend) RPush(_ context.Context, _, value string) (int64, error) {
	return 1, json.Unmarshal([]byte(value), &f.task)
}
func (f *fakeBackend) ReadTaskStream(ctx context.Context, _, _ string) ([]*entity.StreamEvent, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type fakeStore struct{}

func (fakeStore) WriteSSEEvent(context.Context, string, []*entity.StreamEvent) error { return nil }
func TestCreateTask(t *testing.T) {
	f := &fakeBackend{}
	w := httptest.NewRecorder()
	New(f, fakeStore{}, 0).ServeHTTP(w, httptest.NewRequest("POST", "/tasks", strings.NewReader(`{"input":"hello"}`)))
	var result struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 201 || result.TaskID == "" || f.task.TaskId != result.TaskID || f.task.Input != "hello" {
		t.Fatalf("status=%d body=%s task=%+v", w.Code, w.Body, f.task)
	}
}

type streamBackend struct{ ids chan string }

func (s *streamBackend) RPush(context.Context, string, string) (int64, error) { return 1, nil }
func (s *streamBackend) ReadTaskStream(ctx context.Context, _ string, id string) ([]*entity.StreamEvent, error) {
	s.ids <- id
	if id == "0-0" {
		return []*entity.StreamEvent{{ID: "1-0", Type: "delta", Delta: "hello"}}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type recordingStore struct{ writes chan []*entity.StreamEvent }

func (s recordingStore) WriteSSEEvent(_ context.Context, _ string, events []*entity.StreamEvent) error {
	s.writes <- events
	return nil
}
func TestStreamReplaysAndPersists(t *testing.T) {
	backend := &streamBackend{ids: make(chan string, 2)}
	store := recordingStore{writes: make(chan []*entity.StreamEvent, 2)}
	srv := httptest.NewServer(New(backend, store, 10*time.Millisecond))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/tasks/task/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "id: 1-0\n" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	if id := <-backend.ids; id != "0-0" {
		t.Fatalf("initial id %q", id)
	}
	select {
	case events := <-store.writes:
		if len(events) != 1 || events[0].ID != "1-0" {
			t.Fatalf("events=%v", events)
		}
	case <-time.After(time.Second):
		t.Fatal("events not persisted")
	}
	cancel()
}
