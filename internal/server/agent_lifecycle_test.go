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
	"github.com/collie-agent/codex-go/internal/provider"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/testutil"
)

type lifecycleSSEEvent map[string]string

func TestCanceledSiblingHasTerminalStateThroughSSE(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"first","completion_criteria":["done"]},{"agent":"worker","task":"second","completion_criteria":["done"]}]}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer model.Close()
	client, addr := testutil.StartRedis(t)
	cfg := multiConfig(t, model.URL)
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	cfg.MultiAgent.MaxParallel = 1
	o := agent.NewMultiAgentProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if _, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "check", CompletionCriteria: []string{"done"}}); err == nil {
		t.Fatal("expected execution error")
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
	statuses := make(map[string][]string)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event lifecycleSSEEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "task_completed" {
			break
		}
		if event["type"] != "agent_lifecycle" || event["agent_name"] != "worker" {
			continue
		}
		var data struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(event["delta"]), &data); err != nil {
			t.Fatal(err)
		}
		statuses[event["agent_instance_id"]] = append(statuses[event["agent_instance_id"]], data.Status)
	}
	for _, id := range []string{"run/delegation-2", "run/delegation-3"} {
		states := statuses[id]
		if len(states) < 2 || states[0] != "pending" || states[len(states)-1] != "failed" {
			t.Fatalf("sibling states=%v", statuses)
		}
	}
}

func TestDelegatedAgentTerminalStatesThroughSSE(t *testing.T) {
	for _, tc := range []struct{ name, want string }{{"business failure", "failed"}, {"execution error", "failed"}, {"interrupt", "interrupted"}} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch calls.Add(1) {
				case 1:
					multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"check","completion_criteria":["checked"]}]}`)
				case 2:
					switch tc.name {
					case "business failure":
						multiResponse(w, `{"result":{"status":"failed","answer":"cannot check"},"next":[]}`)
					case "execution error":
						w.WriteHeader(http.StatusBadRequest)
					case "interrupt":
						close(started)
						select {
						case <-r.Context().Done():
						case <-release:
						}
					}
				default:
					multiResponse(w, `{"result":{"status":"completed","answer":"reported"},"next":[]}`)
				}
			}))
			defer model.Close()
			defer close(release)
			client, addr := testutil.StartRedis(t)
			cfg := multiConfig(t, model.URL)
			cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			o := agent.NewMultiAgentProcessorOrchestrator(ctx, filemcp.Config{Workspace: t.TempDir()})
			done := make(chan error, 1)
			go func() {
				_, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "check", CompletionCriteria: []string{"done"}})
				done <- err
			}()
			if tc.name == "interrupt" {
				select {
				case <-started:
					cancel(provider.ErrInterrupted)
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not start")
				}
			}
			if err := <-done; (tc.name == "business failure") != (err == nil) {
				t.Fatalf("unexpected execution error: %v", err)
			}
			backend := rdb.NewRedisManagerWithClient(client)
			if _, err := backend.WriteTaskStream(t.Context(), "run", &entity.LLMResponse{Type: "task_completed", Delta: "{}"}); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(New(backend, nil))
			defer srv.Close()
			readCtx, stop := context.WithTimeout(t.Context(), 3*time.Second)
			defer stop()
			req, _ := http.NewRequestWithContext(readCtx, http.MethodGet, srv.URL+"/tasks/run/events", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var statuses []string
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event lifecycleSSEEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if event["type"] == "task_completed" {
					break
				}
				if event["type"] != "agent_lifecycle" || event["agent_name"] != "worker" {
					continue
				}
				if event["agent_instance_id"] != "run/delegation-2" || event["parent_agent_instance_id"] != "run/delegation-1" {
					t.Fatalf("incorrect identity: %v", event)
				}
				var data struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal([]byte(event["delta"]), &data); err != nil {
					t.Fatal(err)
				}
				statuses = append(statuses, data.Status)
			}
			if !reflect.DeepEqual(statuses, []string{"pending", "running", tc.want}) {
				t.Fatalf("statuses=%v", statuses)
			}
		})
	}
}

func TestOrdinaryAgentLifecycleVisibleThroughSSE(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		multiResponse(w, `{"status":"completed","answer":"done"}`)
	}))
	defer model.Close()
	client, addr := testutil.StartRedis(t)
	cfg := multiConfig(t, model.URL).ForAgent("supervisor")
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	o := agent.NewProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if _, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "check"}); err != nil {
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
	var statuses []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event lifecycleSSEEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "task_completed" {
			break
		}
		if event["type"] != "agent_lifecycle" {
			continue
		}
		if event["agent_instance_id"] != "run/main" || event["agent_name"] == "" || event["parent_agent_instance_id"] != "" {
			t.Fatalf("incorrect attribution: %v", event)
		}
		var data struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(event["delta"]), &data); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, data.Status)
	}
	if !reflect.DeepEqual(statuses, []string{"pending", "running", "completed"}) {
		t.Fatalf("statuses=%v", statuses)
	}
}

func TestAgentLifecycleVisibleThroughSSE(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		multiResponse(w, `{"result":{"status":"completed","answer":"done"},"next":[]}`)
	}))
	defer model.Close()
	client, addr := testutil.StartRedis(t)
	cfg := multiConfig(t, model.URL)
	cfg.Global = &config.GlobalConfig{Redis: &config.RedisConfig{Addr: addr}}
	o := agent.NewMultiAgentProcessorOrchestrator(t.Context(), filemcp.Config{Workspace: t.TempDir()})
	if _, err := o.Run(cfg, &state.DurableState{RunId: "run", Goal: "check", CompletionCriteria: []string{"done"}}); err != nil {
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
	var statuses []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event lifecycleSSEEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "task_completed" {
			break
		}
		if event["type"] != "agent_lifecycle" {
			continue
		}
		if event["run_id"] != "run" || event["agent_instance_id"] != "run/delegation-1" || event["agent_name"] != "supervisor" || event["parent_agent_instance_id"] != "" {
			t.Fatalf("incorrect attribution: %v", event)
		}
		var data struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(event["delta"]), &data); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, data.Status)
	}
	if !reflect.DeepEqual(statuses, []string{"pending", "running", "completed"}) {
		t.Fatalf("statuses=%v", statuses)
	}
}
