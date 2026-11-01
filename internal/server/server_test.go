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

	"github.com/collie-agent/codex-go/internal/application"
	"github.com/collie-agent/codex-go/internal/entity"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/testutil"
	"github.com/redis/go-redis/v9"
)

type fakeBackend struct{ task entity.TaskMessage }

func (*fakeBackend) RequestInterrupt(context.Context, string) (string, error) {
	return "unavailable", nil
}

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

func TestCreateTaskWithConversation(t *testing.T) {
	f := &fakeBackend{}
	w := httptest.NewRecorder()
	New(f, fakeStore{}, 0).ServeHTTP(w, httptest.NewRequest("POST", "/tasks", strings.NewReader(`{"input":"continue task","conv_id":"existing-conversation"}`)))
	if w.Code != http.StatusCreated || f.task.ConvId != "existing-conversation" {
		t.Fatalf("status=%d body=%s task=%+v", w.Code, w.Body, f.task)
	}
}

func TestCreateTaskWithoutConversation(t *testing.T) {
	for _, body := range []string{`{"input":"new task"}`, `{"input":"new task","conv_id":""}`} {
		t.Run(body, func(t *testing.T) {
			f := &fakeBackend{}
			w := httptest.NewRecorder()
			New(f, fakeStore{}, 0).ServeHTTP(w, httptest.NewRequest("POST", "/tasks", strings.NewReader(body)))
			if w.Code != http.StatusCreated || f.task.ConvId != "" || f.task.Input != "new task" || f.task.TaskId == "" {
				t.Fatalf("status=%d body=%s task=%+v", w.Code, w.Body, f.task)
			}
		})
	}
}

type streamBackend struct{ ids chan string }

func (*streamBackend) RequestInterrupt(context.Context, string) (string, error) {
	return "unavailable", nil
}

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

// Exercise the wire protocol against Redis so replay uses real, exclusive Stream IDs.
func TestStreamDeliversAndResumesTaskCompletion(t *testing.T) {
	for _, mode := range []string{"live", "replay", "header", "query"} {
		t.Run(mode, func(t *testing.T) {
			client, _ := testutil.StartRedis(t)
			backend := rdb.NewRedisManagerWithClient(client)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			textID, err := backend.WriteTaskStream(ctx, "task", &entity.LLMResponse{Type: "text", Delta: `{"text":"done"}`})
			if err != nil {
				t.Fatal(err)
			}
			publish := func() string {
				t.Helper()
				id, err := backend.WriteTaskStream(ctx, "task", &entity.LLMResponse{Type: "task_completed", Delta: `{}`})
				if err != nil {
					t.Fatal(err)
				}
				return id
			}
			completionID := ""
			if mode != "live" {
				completionID = publish()
			}
			srv := httptest.NewServer(New(backend, fakeStore{}, time.Second))
			defer srv.Close()
			url := srv.URL + "/tasks/task/events"
			if mode == "query" {
				url += "?last_event_id=" + textID
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "header" {
				req.Header.Set("Last-Event-ID", textID)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("unexpected response: %s %v", resp.Status, resp.Header)
			}
			if mode == "live" {
				completionID = publish()
			}
			reader := bufio.NewReader(resp.Body)
			readEvent := func(wantID, wantType, wantDelta string) {
				t.Helper()
				id, err := reader.ReadString('\n')
				if err != nil || id != "id: "+wantID+"\n" {
					t.Fatalf("SSE id=%q err=%v, want %s", id, err, wantID)
				}
				data, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(data, "data: ") {
					t.Fatalf("SSE data=%q err=%v", data, err)
				}
				var event struct {
					ID    string `json:"id"`
					Type  string `json:"type"`
					Delta string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &fields); err != nil || len(fields) != 3 || fields["id"] == nil || fields["type"] == nil || fields["delta"] == nil {
					t.Fatalf("unexpected SSE fields: %s", data)
				}
				if event.ID != wantID || event.Type != wantType || event.Delta != wantDelta {
					t.Fatalf("unexpected event: %+v", event)
				}
				if line, err := reader.ReadString('\n'); err != nil || line != "\n" {
					t.Fatalf("SSE separator=%q err=%v", line, err)
				}
			}
			if mode == "live" || mode == "replay" {
				readEvent(textID, "text", `{"text":"done"}`)
			}
			readEvent(completionID, "task_completed", `{}`)
		})
	}
}

func TestResumeTaskEnqueuesOriginalRun(t *testing.T) {
	backend := &fakeBackend{}
	response := httptest.NewRecorder()
	New(backend, fakeStore{}, 0).ServeHTTP(response, httptest.NewRequest("POST", "/tasks", strings.NewReader(`{"run_id":"original-run"}`)))
	if response.Code != http.StatusCreated || backend.task.RunId != "original-run" || backend.task.Input != "" || backend.task.TaskId == "" {
		t.Fatalf("status=%d task=%+v", response.Code, backend.task)
	}
}

type interruptBackend struct{ fakeBackend }

func (*interruptBackend) RequestInterrupt(context.Context, string) (string, error) {
	return "accepted", nil
}

func TestInterruptRequestAcknowledgesDelivery(t *testing.T) {
	w := httptest.NewRecorder()
	New(&interruptBackend{}, fakeStore{}, 0).ServeHTTP(w, httptest.NewRequest("POST", "/runs/run-1/interrupt", nil))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "accepted") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}

func TestInterruptDeliveryAcrossRedisClients(t *testing.T) {
	client, addr := testutil.StartRedis(t)
	other := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	defer other.Close()
	backend := rdb.NewRedisManagerWithClient(client)
	backend.SetRunStatusLookup(func(context.Context, string) (string, error) { return "running", nil })
	signal := &testInterrupt{called: make(chan struct{})}
	stop, err := rdb.ListenInterrupt(t.Context(), other, "run-1", signal)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	handler := New(backend, fakeStore{}, 0)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/runs/run-1/interrupt", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	select {
	case <-signal.called:
	case <-time.After(time.Second):
		t.Fatal("signal not delivered")
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/runs/run-1/interrupt", nil))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already_requested") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}

type testInterrupt struct{ called chan struct{} }

func (s *testInterrupt) TryInterrupt() string {
	select {
	case <-s.called:
		return "already_requested"
	default:
		close(s.called)
		return "accepted"
	}
}

func TestInterruptDoesNotAcknowledgeFinishedExecution(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	backend := rdb.NewRedisManagerWithClient(client)
	backend.SetRunStatusLookup(func(context.Context, string) (string, error) { return "running", nil })
	signal := application.NewInterrupt()
	stop, err := rdb.ListenInterrupt(t.Context(), client, "finished-run", signal)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	signal.Finish()
	response := httptest.NewRecorder()
	New(backend, fakeStore{}, 0).ServeHTTP(response, httptest.NewRequest("POST", "/runs/finished-run/interrupt", nil))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "terminal") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
}

func TestConcurrentInterruptRequestsHaveOneAcceptance(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	backend := rdb.NewRedisManagerWithClient(client)
	backend.SetRunStatusLookup(func(context.Context, string) (string, error) { return "running", nil })
	stop, err := rdb.ListenInterrupt(t.Context(), client, "concurrent-run", application.NewInterrupt())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	handler := New(backend, fakeStore{}, 0)
	results := make(chan int, 16)
	for i := 0; i < cap(results); i++ {
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/runs/concurrent-run/interrupt", nil))
			results <- response.Code
		}()
	}
	accepted := 0
	for i := 0; i < cap(results); i++ {
		switch code := <-results; code {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected response %d", code)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted=%d, want 1", accepted)
	}
}
