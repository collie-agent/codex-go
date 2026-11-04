package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
	"github.com/collie-agent/codex-go/internal/rdb"
	"github.com/collie-agent/codex-go/internal/testutil"
)

func TestResumeInterruptedRunDoesNotProduceOutput(t *testing.T) {
	_, store := runStore(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeResult(w, `{"status":"completed","answer":"must not run"}`)
	}))
	defer model.Close()
	for _, mode := range []string{"react", "plan-execute", "multi-agent"} {
		t.Run(mode, func(t *testing.T) {
			run, err := store.Create(t.Context(), "", "stopped", t.TempDir(), mode)
			if err != nil {
				t.Fatal(err)
			}
			run.Status = "interrupted"
			if err := store.SaveDurableCheckpoint(t.Context(), &run, &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID}, mode); err != nil {
				t.Fatal(err)
			}
			path := agentConfig(t, model.URL)
			if mode == "multi-agent" {
				path = filepath.Join(t.TempDir(), "multi.yaml")
				data := "name: multi-agent\nmulti-agent:\n  entry: supervisor\n  max_rounds: 5\n  max_parallel: 2\nbase:\n  llm:\n    base_url: " + model.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    next_agents: []\n"
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = application.Resume(t.Context(), application.Options{RunID: run.RunID, ConfigPath: path, Timeout: time.Second, Stderr: io.Discard})
			if err == nil || !strings.Contains(err.Error(), "interrupted") {
				t.Errorf("expected interrupted refusal, got %v", err)
			}
			saved, err := store.Get(t.Context(), run.RunID)
			if err != nil || saved.Status != "interrupted" {
				t.Errorf("run changed: %+v, %v", saved, err)
			}

		})
	}
	if calls.Load() != 0 {
		t.Errorf("refused resume made %d model requests", calls.Load())
	}
}

func TestResumeRebuildsNestedSameNamedAgentOutput(t *testing.T) {
	_, store := runStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	run, err := store.Create(ctx, "", "inspect", t.TempDir(), "multi-agent")
	if err != nil {
		t.Fatal(err)
	}
	redisClient, _ := testutil.StartRedis(t)
	backend := rdb.NewRedisManagerWithClient(redisClient)
	output := provider.NewRedisOutputer(backend)
	gate := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(gate) })
	var branchCalls, leafCalls, otherCalls, laterCalls, rootCalls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var input string
		for _, item := range request.Input {
			input += item.Content
		}
		_, task, _ := strings.Cut(input, `"delegated_task":`)
		task, _, _ = strings.Cut(task, `,"reports":`)
		switch {
		case strings.Contains(task, `"goal":"nested branch"`):
			branchCalls.Add(1)
			if strings.Contains(input, `"reports":[{`) {
				writeResult(w, `{"result":{"status":"completed","answer":"branch report"},"next":[]}`)
			} else {
				writeResult(w, `{"result":{"status":"completed","answer":"delegate leaf"},"next":[{"agent":"leaf","task":"nested leaf","completion_criteria":["done"]}]}`)
			}
		case strings.Contains(task, `"worker":"leaf"`):
			leafCalls.Add(1)
			writeResult(w, `{"result":{"status":"completed","answer":"leaf report"},"next":[]}`)
		case strings.Contains(task, `"goal":"parallel other"`):
			if otherCalls.Add(1) == 1 {
				select {
				case <-gate:
				case <-ctx.Done():
					return
				}
				http.Error(w, "injected failure", http.StatusBadRequest)
			} else {
				writeResult(w, `{"result":{"status":"completed","answer":"other report"},"next":[]}`)
			}
		case strings.Contains(task, `"goal":"later work"`):
			laterCalls.Add(1)
			writeResult(w, `{"result":{"status":"completed","answer":"later report"},"next":[]}`)
		default:
			switch rootCalls.Add(1) {
			case 1:
				writeResult(w, `{"entry_completion_criteria":["done"],"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"nested branch","completion_criteria":["done"]},{"agent":"worker","task":"parallel other","completion_criteria":["done"]}]}`)
			case 2:
				writeResult(w, `{"result":{"status":"completed","answer":"delegate again"},"next":[{"agent":"worker","task":"later work","completion_criteria":["done"]}]}`)
			default:
				writeResult(w, `{"result":{"status":"completed","answer":"final"},"next":[]}`)
			}
		}
	}))
	defer model.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	config := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 15\n  max_parallel: 2\nbase:\n  llm:\n    base_url: " + model.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    next_agents: [worker]\n  worker:\n    next_agents: [leaf, supervisor]\n  leaf:\n    next_agents: [worker]\n"
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	opts := application.Options{RunID: run.RunID, ConfigPath: path, Timeout: 5 * time.Second, Stderr: io.Discard, Output: output}
	done := make(chan error, 1)
	go func() {
		_, err := application.Resume(provider.WithTaskStreamID(ctx, "before-resume"), opts)
		done <- err
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
waiting:
	for {
		select {
		case <-ctx.Done():
			t.Fatal("completed branch was not checkpointed")
		case err := <-done:
			t.Fatalf("run ended before branch completed: %v", err)
		case <-ticker.C:
			saved, err := store.Get(ctx, run.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(saved.MultiAgentCheckpoint), `"branch report"`) {
				break waiting
			}
		}
	}
	release.Do(func() { close(gate) })
	if err := <-done; err == nil {
		t.Fatal("injected failure did not stop execution")
	}
	// Replay each task from Redis; resumed tasks have independent stream cursors.
	// Stream IDs belong to Redis records, independently of the output payload.
	type recordedEvent struct {
		provider.OutputEvent
		StreamID string
	}
	readHistory := func(task string) []recordedEvent {
		t.Helper()
		rows, err := redisClient.XRange(ctx, "codex-go-resp-stream-"+task, "-", "+").Result()
		if err != nil {
			t.Fatal(err)
		}
		events := make([]recordedEvent, 0, len(rows))
		for _, row := range rows {
			value := func(key string) string { v, _ := row.Values[key].(string); return v }
			events = append(events, recordedEvent{StreamID: row.ID, OutputEvent: provider.OutputEvent{TaskID: task, RunID: value("run_id"), AgentInstanceID: value("agent_instance_id"), ParentAgentInstanceID: value("parent_agent_instance_id"), AgentName: value("agent_name"), Kind: provider.OutputKind(value("type")), Data: json.RawMessage(value("delta"))}})
		}
		return events
	}
	before := readHistory("before-resume")
	if len(before) == 0 {
		t.Fatal("offline history is empty")
	}
	cursor := len(before)
	// Resume under a different task route; participation and run identities stay put.
	results, err := application.Resume(provider.WithTaskStreamID(ctx, "after-resume"), opts)
	if err != nil || len(results) != 1 || results[0].Answer != "final" {
		t.Fatalf("results=%+v error=%v", results, err)
	}
	if branchCalls.Load() != 2 || leafCalls.Load() != 1 || otherCalls.Load() != 2 || laterCalls.Load() != 1 || rootCalls.Load() != 3 {
		t.Fatalf("repeated completed work: branch=%d leaf=%d other=%d later=%d root=%d", branchCalls.Load(), leafCalls.Load(), otherCalls.Load(), laterCalls.Load(), rootCalls.Load())
	}
	after := readHistory("after-resume")
	all := append(append([]recordedEvent{}, before...), after...)
	if len(all) != len(before)+len(after) {
		t.Fatalf("history cursor lost events: before=%d after=%d all=%d", len(before), len(after), len(all))
	}
	statuses := map[string][]string{}
	texts := map[string][]string{}
	parents := map[string]string{}
	names := map[string]string{}
	ids := map[string]bool{}
	for i, event := range all {
		if event.StreamID == "" || ids[event.TaskID+"/"+event.StreamID] || event.RunID != run.RunID {
			t.Fatalf("unstable event identity at %d: %+v", i, event)
		}
		ids[event.TaskID+"/"+event.StreamID] = true
		if i < cursor {
			if event.StreamID != before[i].StreamID || event.TaskID != "before-resume" {
				t.Fatalf("old event changed: %+v", event)
			}
		} else if event.TaskID != "after-resume" {
			t.Fatalf("new route missing: %+v", event)
		}
		if event.AgentInstanceID == "" {
			continue
		}
		if name, exists := names[event.AgentInstanceID]; exists && (name != event.AgentName || parents[event.AgentInstanceID] != event.ParentAgentInstanceID) {
			t.Fatalf("participation changed: %+v", event)
		}
		names[event.AgentInstanceID], parents[event.AgentInstanceID] = event.AgentName, event.ParentAgentInstanceID
		if event.Kind == provider.OutputAgentLifecycle {
			var data provider.AgentLifecycleOutput
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			statuses[event.AgentInstanceID] = append(statuses[event.AgentInstanceID], string(data.Status))
		}
		if event.Kind == provider.OutputText {
			var data provider.TextOutput
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			texts[event.AgentInstanceID] = append(texts[event.AgentInstanceID], data.Text)
		}
	}
	root := run.RunID + "/delegation-1"
	var workers []string
	var leaf string
	for id, name := range names {
		switch name {
		case "worker":
			workers = append(workers, id)
			if parents[id] != root {
				t.Fatalf("worker parent=%v", parents)
			}
		case "leaf":
			leaf = id
		}
	}
	if len(names) != 5 || len(workers) != 3 || leaf == "" || names[parents[leaf]] != "worker" || parents[root] != "" {
		t.Fatalf("hierarchy names=%v parents=%v", names, parents)
	}
	for id, states := range statuses {
		if states[len(states)-1] != "completed" {
			t.Fatalf("participation %s states=%v", id, states)
		}
	}
	if strings.Join(texts[root], ",") != "delegate,delegate again,final" || strings.Join(texts[leaf], ",") != "leaf report" {
		t.Fatalf("repeated or missing output: %v", texts)
	}
	if strings.Join(statuses[root], ",") != "pending,running,failed,pending,running,completed" {
		t.Fatalf("root did not resume its participation: %v", statuses[root])
	}
	for _, id := range workers {
		switch strings.Join(texts[id], ",") {
		case "delegate leaf,branch report":
			if strings.Join(statuses[id], ",") != "pending,running,completed,completed" {
				t.Fatalf("completed child restarted: %v", statuses[id])
			}
			for _, event := range after {
				if (event.AgentInstanceID == id || event.AgentInstanceID == leaf) && event.Kind != provider.OutputAgentLifecycle {
					t.Fatalf("completed child output resent: %+v", event)
				}
			}
		case "other report":
			if strings.Join(statuses[id], ",") != "pending,running,failed,pending,running,completed" {
				t.Fatalf("resumed identity states=%v", statuses[id])
			}
		case "later report":
			if strings.Join(statuses[id], ",") != "pending,running,completed" {
				t.Fatalf("new delegation reused an old participation: %v", statuses[id])
			}
		default:
			t.Fatalf("worker output missing: %v", texts)
		}
	}
	// Reopening a completed run returns saved results without re-emitting its output.
	if _, err := application.Resume(ctx, opts); err != nil {
		t.Fatal(err)
	}
	if got := append(readHistory("before-resume"), readHistory("after-resume")...); !reflect.DeepEqual(got, all) {
		t.Fatal("completed run replay emitted new output")
	}
}
