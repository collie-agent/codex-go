package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/database"
)

func TestConversationRuns(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := database.NewRunStore(db)
	first, err := store.Create(ctx, "", "first", t.TempDir(), "react")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, first.ConvID, "second", first.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(ctx, "", "other conversation", first.Workspace, "react")
	if err != nil {
		t.Fatal(err)
	}
	first.Status = "completed"
	first.Result = json.RawMessage(`{"answer":"done"}`)
	if err := store.Save(ctx, &first); err != nil {
		t.Fatal(err)
	}
	second.Status = "failed"
	second.Error = "model unavailable"
	if err := store.Save(ctx, &second); err != nil {
		t.Fatal(err)
	}
	for _, call := range []database.ToolExecution{
		{RunID: first.RunID, ExecutionID: first.RunID, CallID: "call-1", Name: "read_file", Arguments: `{"path":"README.md"}`, Status: "completed", Result: "contents"},
		{RunID: first.RunID, ExecutionID: "child-" + first.RunID, CallID: "call-2", Name: "exec", Arguments: `{"command":"false"}`, Status: "failed", Error: "exit 1"},
	} {
		if err := db.Create(&call).Error; err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	New(&fakeBackend{}, database.NewSSEEventStore(db), 0).ServeHTTP(w, httptest.NewRequest("GET", "/conversations/"+first.ConvID+"/runs", nil))
	var body struct {
		ConvID string `json:"conv_id"`
		Runs   []struct {
			RunID  string          `json:"run_id"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
			Calls  []struct {
				CallID    string `json:"call_id"`
				Arguments string `json:"arguments"`
				Result    string `json:"result"`
				Error     string `json:"error"`
			} `json:"tool_calls"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("status=%d body=%s: %v", w.Code, w.Body, err)
	}
	if w.Code != 200 || body.ConvID != first.ConvID || len(body.Runs) != 2 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	for _, run := range body.Runs {
		switch run.RunID {
		case first.RunID:
			if string(run.Result) != `{"answer":"done"}` || len(run.Calls) != 2 {
				t.Fatalf("run=%+v", run)
			}
			calls := map[string]string{}
			for _, c := range run.Calls {
				calls[c.CallID] = c.Result + c.Error
				if c.CallID == "call-1" && c.Arguments != `{"path":"README.md"}` {
					t.Fatalf("arguments=%q", c.Arguments)
				}
			}
			if calls["call-1"] != "contents" || calls["call-2"] != "exit 1" {
				t.Fatalf("calls=%v", calls)
			}
		case second.RunID:
			if run.Error != "model unavailable" || len(run.Calls) != 0 {
				t.Fatalf("run=%+v", run)
			}
		default:
			t.Fatalf("unrelated run: %s", run.RunID)
		}
	}
}

func TestConversationRunsEmptyAndErrors(t *testing.T) {
	raw := os.Getenv("CODEX_TEST_MYSQL_URL")
	if raw == "" {
		t.Skip("set CODEX_TEST_MYSQL_URL")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	conv := database.Conversation{ConvID: fmt.Sprintf("empty-%d", time.Now().UnixNano())}
	if err := db.Create(&conv).Error; err != nil {
		t.Fatal(err)
	}
	handler := New(&fakeBackend{}, database.NewSSEEventStore(db), 0)
	for _, tc := range []struct {
		id     string
		status int
		body   string
	}{
		{conv.ConvID, 200, `"runs":[]`},
		{"missing-" + conv.ConvID, 404, `"error":"conversation not found"`},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/conversations/"+tc.id+"/runs", nil))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	}
	if err := database.Close(db); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/conversations/"+conv.ConvID+"/runs", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "sql:") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
}
