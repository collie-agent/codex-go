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
	"github.com/collie-agent/codex-go/internal/provider"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/testutil"
)

func TestSeparateAssistantAndAnswerEventsReachSSE(t *testing.T) {
	client, _ := testutil.StartRedis(t)
	backend := rdb.NewRedisManagerWithClient(client)
	output := provider.NewRedisOutputer(backend)
	for _, sample := range []struct {
		kind provider.OutputKind
		text string
	}{
		{"assistant_delta", "继续检查上下文预算。"},
		{"answer_delta", "检查完成。"},
	} {
		data, _ := json.Marshal(provider.TextOutput{Text: sample.text})
		if err := output.Emit(t.Context(), provider.OutputEvent{TaskID: "run", RunID: "run", AgentInstanceID: "main", AgentName: "react", Kind: sample.kind, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := backend.WriteTaskStream(t.Context(), "run", &entity.LLMResponse{Type: "task_completed", Delta: "{}"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(backend, nil))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/tasks/run/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := map[string]string{}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "data: ") {
			continue
		}
		var event map[string]string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "task_completed" {
			break
		}
		var body provider.TextOutput
		if err := json.Unmarshal([]byte(event["delta"]), &body); err != nil {
			t.Fatal(err)
		}
		if event["run_id"] != "run" || event["agent_instance_id"] != "main" || event["agent_name"] != "react" {
			t.Fatalf("attribution=%v", event)
		}
		got[event["type"]] += body.Text
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if got["assistant_delta"] != "继续检查上下文预算。" || got["answer_delta"] != "检查完成。" || len(got) != 2 {
		t.Fatalf("events=%v", got)
	}
}
