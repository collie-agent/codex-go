package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/collie-agent/codex-go/internal/database"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/collie-agent/codex-go/internal/agent"
	"github.com/collie-agent/codex-go/internal/application"
	state "github.com/collie-agent/codex-go/internal/context"
)

type taskInfoWriter struct{ ids chan string }

func (w taskInfoWriter) Write(p []byte) (int, error) {
	for _, f := range strings.Fields(string(p)) {
		if strings.HasPrefix(f, "run_id=") {
			select {
			case w.ids <- strings.TrimPrefix(f, "run_id="):
			default:
			}
		}
	}
	return len(p), nil
}

func TestMultiAgentApplicationResumesNestedParallelDelegations(t *testing.T) {
	_, store := runStore(t)
	gate := make(chan struct{})
	var branchCalls, leafCalls, otherCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		for _, m := range request.Input {
			input += m.Content
		}
		task := input
		if _, tail, ok := strings.Cut(input, `"delegated_task":`); ok {
			task, _, _ = strings.Cut(tail, `,"reports":`)
		}
		switch {
		case strings.Contains(task, `"worker":"branch"`):
			branchCalls.Add(1)
			if strings.Contains(input, "leaf report") {
				writeResult(w, `{"result":{"status":"completed","answer":"branch report"},"next":[]}`)
			} else {
				writeResult(w, `{"result":{"status":"completed","answer":"delegate leaf"},"next":[{"agent":"leaf","task":"nested leaf","completion_criteria":["done"]}]}`)
			}
		case strings.Contains(task, `"worker":"leaf"`):
			leafCalls.Add(1)
			writeResult(w, `{"result":{"status":"completed","answer":"leaf report"},"next":[]}`)
		case strings.Contains(task, `"worker":"other"`):
			if otherCalls.Add(1) == 1 {
				<-gate
				http.Error(w, "injected failure", http.StatusBadRequest)
			} else {
				writeResult(w, `{"result":{"status":"completed","answer":"other report"},"next":[]}`)
			}
		case strings.Contains(input, "branch report"):
			if !strings.Contains(input, "other report") || strings.Index(input, "branch report") > strings.Index(input, "other report") {
				t.Error("reports missing or out of order")
			}
			writeResult(w, `{"result":{"status":"completed","answer":"final"},"next":[]}`)
		default:
			writeResult(w, `{"entry_completion_criteria":["done"],"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"branch","task":"nested branch","completion_criteria":["done"]},{"agent":"other","task":"parallel other","completion_criteria":["done"]}]}`)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "multi.yaml")
	data := "name: multi-agent\nlog_level: error\nmulti-agent:\n  entry: supervisor\n  max_rounds: 15\n  max_parallel: 2\nbase:\n  llm:\n    base_url: " + server.URL + "\n    api_key: test\n    model: test\nagents:\n  supervisor:\n    instruction: supervise\n    next_agents: [branch, other]\n  branch:\n    instruction: branch\n    next_agents: [leaf, supervisor]\n  leaf:\n    instruction: leaf\n    next_agents: [branch]\n  other:\n    instruction: other\n    next_agents: [supervisor]\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ids := make(chan string, 1)
	finished := make(chan error, 1)
	go func() {
		_, err := application.Run(context.Background(), application.Options{ConfigPath: path, Task: "inspect", Workspace: t.TempDir(), Timeout: 10 * time.Second, Stderr: taskInfoWriter{ids}})
		finished <- err
	}()
	var id string
	select {
	case id = <-ids:
	case <-time.After(10 * time.Second):
		t.Fatal("missing run ID")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(run.MultiAgentCheckpoint), `"branch report"`) {
			break
		}
		if time.Now().After(deadline) {
			close(gate)
			t.Fatalf("branch did not persist: %s", run.MultiAgentCheckpoint)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(gate)
	if err := <-finished; err == nil {
		t.Fatal("injected failure did not stop run")
	}
	results, err := application.Resume(context.Background(), application.Options{ConfigPath: path, RunID: id, Timeout: 10 * time.Second})
	if err != nil || len(results) != 1 || results[0].Answer != "final" || branchCalls.Load() != 2 || leafCalls.Load() != 1 || otherCalls.Load() != 2 {
		t.Fatalf("results=%+v branch=%d leaf=%d other=%d error=%v", results, branchCalls.Load(), leafCalls.Load(), otherCalls.Load(), err)
	}
	assertGraphInstancesPersisted(t, store, id)
}

// assertGraphInstancesPersisted checks the scheduler's published instances against
// the shared store, including child nodes and later calls of the parent node.
func assertGraphInstancesPersisted(t *testing.T, store *database.RunStore, runID string) {
	t.Helper()
	run, err := store.Get(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var graph state.DelegationGraph[agent.MultiAgentResult, agent.DelegatedTask]
	if err := json.Unmarshal(run.MultiAgentCheckpoint, &graph); err != nil {
		t.Fatal(err)
	}
	executions := graph.Executions()
	if len(executions) < 5 {
		t.Fatalf("expected child and follow-up instances, got %d", len(executions))
	}
	for _, execution := range executions {
		saved, err := store.ExecutionByID(t.Context(), runID, execution.ExecutionID)
		if err != nil {
			t.Fatalf("instance %s not persisted: %v", execution.ExecutionID, err)
		}
		var savedResult, graphResult any
		if len(saved.ExecutionResult) > 0 {
			if err := json.Unmarshal(saved.ExecutionResult, &savedResult); err != nil {
				t.Fatal(err)
			}
		}
		if len(execution.ExecutionResult) > 0 {
			if err := json.Unmarshal(execution.ExecutionResult, &graphResult); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(savedResult, graphResult) || len(saved.ToolExecutions) != len(execution.ToolExecutions) || (len(saved.ToolExecutions) > 0 && !reflect.DeepEqual(saved.ToolExecutions, execution.ToolExecutions)) {
			t.Fatalf("instance %s differs: saved=%+v graph=%+v", execution.ExecutionID, saved, execution)
		}
	}
}

func TestMultiAgentCheckpointCommitsAllInstancesAtomically(t *testing.T) {
	db, store := runStore(t)
	ctx := t.Context()
	run, err := store.Create(ctx, "", "delegate", t.TempDir(), "multi-agent")
	if err != nil {
		t.Fatal(err)
	}
	graph := state.DelegationGraph[agent.MultiAgentResult, agent.DelegatedTask]{Nodes: map[string]*state.DelegationNode[agent.MultiAgentResult, agent.DelegatedTask]{}}
	for _, nodeID := range []string{"parent", "child"} {
		node := &state.DelegationNode[agent.MultiAgentResult, agent.DelegatedTask]{}
		turns := 1
		if nodeID == "parent" {
			turns = 2
		}
		for turn := 0; turn < turns; turn++ {
			node.Turns = append(node.Turns, &state.DelegationTurn[agent.DelegatedTask]{Local: &state.DurableState{
				RunId: run.RunID, ExecutionID: fmt.Sprintf("%s-%s-%d", run.RunID, nodeID, turn), Goal: nodeID,
				ToolExecutions: []*state.ToolResult{{ToolCallId: "same-call-id", ToolName: "read_file", Arguments: `{}`, Batch: 1}},
			}})
		}
		graph.Nodes[nodeID] = node
	}
	durable := &state.DurableState{RunId: run.RunID}
	save := func() error {
		var err error
		durable.MultiAgentCheckpoint, err = json.Marshal(graph)
		if err != nil {
			return err
		}
		return store.SaveDurableCheckpoint(ctx, &run, durable, "multi-agent")
	}
	if err := save(); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, execution := range graph.Executions() {
		saved, err := store.ExecutionByID(ctx, run.RunID, execution.ExecutionID)
		if err != nil || len(saved.ToolExecutions) != 1 || saved.ToolExecutions[0].Status != "" {
			t.Fatalf("tool registration missing: instance=%s saved=%+v error=%v", execution.ExecutionID, saved, err)
		}
		execution.ToolExecutions[0].Status = "success"
		execution.ToolExecutions[0].Result = "[]"
		execution.ExecutionResult = []byte(`{"status":"completed","answer":"done"}`)
	}
	trigger := "fail_child_" + run.RunID
	sql := "CREATE TRIGGER " + trigger + " BEFORE UPDATE ON tool_execution FOR EACH ROW BEGIN IF NEW.run_id = '" + run.RunID + "' AND NEW.execution_id LIKE '%-child-%' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'child checkpoint unavailable'; END IF; END"
	if err := db.Exec(sql).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER IF EXISTS " + trigger)
	if err := save(); err == nil || !strings.Contains(err.Error(), "child checkpoint unavailable") {
		t.Fatalf("expected child write failure, got %v", err)
	}
	after, err := store.Get(ctx, run.RunID)
	if err != nil || string(before.MultiAgentCheckpoint) != string(after.MultiAgentCheckpoint) {
		t.Fatalf("graph partially committed: error=%v", err)
	}
	for _, execution := range graph.Executions() {
		saved, err := store.ExecutionByID(ctx, run.RunID, execution.ExecutionID)
		if err != nil || saved.ToolExecutions[0].Status != "" || len(saved.ExecutionResult) != 0 {
			t.Fatalf("instance partially committed: saved=%+v error=%v", saved, err)
		}
	}
	if err := db.Exec("DROP TRIGGER " + trigger).Error; err != nil {
		t.Fatal(err)
	}
	if err := save(); err != nil {
		t.Fatal(err)
	}
	for _, execution := range graph.Executions() {
		saved, err := store.ExecutionByID(ctx, run.RunID, execution.ExecutionID)
		if err != nil || saved.ToolExecutions[0].Status != "success" || len(saved.ExecutionResult) == 0 {
			t.Fatalf("confirmed outcome missing: saved=%+v error=%v", saved, err)
		}
	}
}
