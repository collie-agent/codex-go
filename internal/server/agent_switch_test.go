package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/entity"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/testutil"
)

func TestMultiAgentSwitchesReachSSE(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"check","completion_criteria":["checked"]}]}`)
		} else {
			multiResponse(w, `{"result":{"status":"completed","answer":"done"},"next":[]}`)
		}
	}))
	defer model.Close()
	client, addr := testutil.StartRedis(t)
	cfg := multiConfig(t, model.URL)
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	o := agent.NewMultiAgentProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if _, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"checked"}}); err != nil {
		t.Fatal(err)
	}
	backend := rdb.NewRedisManagerWithClient(client)
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
	var got []map[string]string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "data: ") {
			continue
		}
		var event lifecycleSSEEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "task_completed" {
			break
		}
		if event["type"] != "agent_switch" {
			continue
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(event["delta"]), &body); err != nil {
			t.Fatal(err)
		}
		if event["run_id"] != "run" || event["agent_name"] != body["src_agent_name"] {
			t.Fatalf("switch attribution=%v", event)
		}
		got = append(got, body)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{{"src_agent_name": "supervisor", "dst_agent_name": "worker"}, {"src_agent_name": "worker", "dst_agent_name": "supervisor"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SSE switches=%v, want %v", got, want)
	}
}
