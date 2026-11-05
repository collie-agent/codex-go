package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/collie-agent/codex-go/internal/agent"
	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/database"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

// 只通过 HTTP 观察历史；模型是外部边界，运行和持久化使用真实实现。
func TestConversationRunsExposeAgentHistory(t *testing.T) {
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
	run, err := store.Create(ctx, "", "inspect repository", t.TempDir(), "multi-agent")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate inspection"},"next":[{"agent":"worker","task":"inspect files","completion_criteria":["files checked"]}]}`)
		case 2:
			multiResponse(w, `{"result":{"status":"completed","answer":"files checked"},"next":[]}`)
		default:
			multiResponse(w, `{"result":{"status":"completed","answer":"inspection done"},"next":[]}`)
		}
	}))
	defer model.Close()
	orchestrator := agent.NewMultiAgentProcessorOrchestrator(ctx, filemcp.Config{Workspace: run.Workspace})
	orchestrator.SetCheckpoint(func(d *state.DurableState, mode string) error { return store.SaveDurableCheckpoint(ctx, &run, d, mode) })
	durable := &state.DurableState{RunId: run.RunID, Goal: run.Goal, CompletionCriteria: []string{"repository inspected"}}
	if _, err := orchestrator.Run(multiConfig(t, model.URL), durable); err != nil {
		t.Fatal(err)
	}
	run.Status = "completed"
	if err := store.Save(ctx, &run); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	New(&fakeBackend{}, store).ServeHTTP(w, httptest.NewRequest("GET", "/conversations/"+run.ConvID+"/runs", nil))
	var body struct {
		Runs []struct {
			Agents []struct {
				ID     string `json:"agent_instance_id"`
				Parent string `json:"parent_agent_instance_id"`
				Name   string `json:"agent_name"`
				Goal   string `json:"goal"`
				Status string `json:"status"`
				Result struct {
					Answer string `json:"answer"`
				} `json:"result"`
				Turns []struct {
					ExecutionID string `json:"execution_id"`
					Result      struct {
						Answer string `json:"answer"`
					} `json:"result"`
				} `json:"turns"`
			} `json:"agents"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(body.Runs) != 1 || len(body.Runs[0].Agents) != 2 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	root, child := body.Runs[0].Agents[0], body.Runs[0].Agents[1]
	if root.ID != run.RunID+"/delegation-1" || root.Parent != "" || root.Name != "supervisor" || root.Status != "completed" || root.Result.Answer != "inspection done" || len(root.Turns) != 2 || root.Turns[0].Result.Answer != "delegate inspection" || root.Turns[1].Result.Answer != "inspection done" {
		t.Fatalf("root=%+v", root)
	}
	if child.ID != run.RunID+"/delegation-2" || child.Parent != root.ID || child.Name != "worker" || child.Goal != "inspect files" || child.Status != "completed" || child.Result.Answer != "files checked" || len(child.Turns) != 1 || child.Turns[0].ExecutionID != child.ID+"/turn-0" {
		t.Fatalf("child=%+v", child)
	}
}

func TestConversationRunsKeepDelegationsAndToolsSeparate(t *testing.T) {
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
	run, err := store.Create(ctx, "", "inspect", t.TempDir(), "multi-agent")
	if err != nil {
		t.Fatal(err)
	}
	rootID := run.RunID + "/delegation-1"
	firstID, secondID, nestedID := run.RunID+"/delegation-2", run.RunID+"/delegation-3", run.RunID+"/delegation-4"
	result := func(status state.ReactStatus, answer string) *state.MultiAgentResult {
		return &state.MultiAgentResult{Result: state.ReactResult{Status: status, Answer: answer}}
	}
	firstResult := result(state.CompletedReactStatus, "first checked")
	failedResult := result(state.FailedReactStatus, "file missing")
	firstRaw, err := json.Marshal(firstResult)
	if err != nil {
		t.Fatal(err)
	}
	graph := state.DelegationGraph[state.MultiAgentResult, state.DelegatedTask]{Nodes: map[string]*state.DelegationNode[state.MultiAgentResult, state.DelegatedTask]{
		rootID: {Turns: []*state.DelegationTurn[state.DelegatedTask]{{Local: &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID}, Children: []state.DelegatedTask{
			{TaskID: firstID, Worker: "worker", Goal: "first task"}, {TaskID: secondID, Worker: "worker", Goal: "second task"},
		}}}},
		firstID: {Done: firstResult, Turns: []*state.DelegationTurn[state.DelegatedTask]{
			{Local: &state.DurableState{RunId: run.RunID, ExecutionID: firstID + "/turn-0", ExecutionResult: firstRaw, ToolExecutions: []*state.ToolResult{{ToolCallId: "call-first", ToolName: "read_file", Arguments: `{"path":"a.go"}`, Status: "completed", Result: "first contents"}}}, Children: []state.DelegatedTask{{TaskID: nestedID, Worker: "reviewer", Goal: "review first"}}},
			{Local: &state.DurableState{RunId: run.RunID, ExecutionID: firstID + "/turn-1", ExecutionResult: firstRaw, ToolExecutions: []*state.ToolResult{{ToolCallId: "call-followup", ToolName: "read_file", Arguments: `{"path":"b.go"}`, Status: "completed", Result: "followup contents"}}}},
		}},
		nestedID: {Done: failedResult},
	}}
	checkpoint, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDurableCheckpoint(ctx, &run, &state.DurableState{RunId: run.RunID, MultiAgentCheckpoint: checkpoint}, "multi-agent"); err != nil {
		t.Fatal(err)
	}
	// 父运行结束不能让尚无终态的子代理被误判为成功。
	run.Status = "interrupted"
	if err := store.Save(ctx, &run); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	New(&fakeBackend{}, store).ServeHTTP(w, httptest.NewRequest("GET", "/conversations/"+run.ConvID+"/runs", nil))
	var body struct {
		Runs []struct {
			Calls  []json.RawMessage `json:"tool_calls"`
			Agents []struct {
				ID     string             `json:"agent_instance_id"`
				Parent string             `json:"parent_agent_instance_id"`
				Name   string             `json:"agent_name"`
				Status string             `json:"status"`
				Result *state.ReactResult `json:"result"`
				Calls  []struct {
					ID     string `json:"call_id"`
					Result string `json:"result"`
				} `json:"tool_calls"`
				Turns []struct {
					Calls []struct {
						ID string `json:"call_id"`
					} `json:"tool_calls"`
				} `json:"turns"`
			} `json:"agents"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(body.Runs) != 1 || len(body.Runs[0].Agents) != 4 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	agents := body.Runs[0].Agents
	if agents[0].ID != rootID || agents[1].ID != firstID || agents[2].ID != nestedID || agents[3].ID != secondID {
		t.Fatalf("unexpected delegation order: %s", w.Body)
	}
	if agents[1].Parent != rootID || agents[2].Parent != firstID || agents[3].Parent != rootID || agents[1].Name != "worker" || agents[3].Name != "worker" {
		t.Fatalf("lost hierarchy: %s", w.Body)
	}
	if agents[1].Status != "completed" || agents[2].Status != "failed" || agents[2].Result.Answer != "file missing" || agents[0].Status != "unknown" || agents[3].Status != "unknown" || agents[3].Result != nil {
		t.Fatalf("incorrect agent states: %s", w.Body)
	}
	if len(body.Runs[0].Calls) != 2 || len(agents[1].Calls) != 2 || agents[1].Calls[0].ID != "call-first" || agents[1].Calls[0].Result != "first contents" || agents[1].Calls[1].ID != "call-followup" || len(agents[1].Turns) != 2 || len(agents[1].Turns[0].Calls) != 1 || agents[1].Turns[0].Calls[0].ID != "call-first" || len(agents[1].Turns[1].Calls) != 1 || agents[1].Turns[1].Calls[0].ID != "call-followup" || len(agents[0].Calls) != 0 || len(agents[3].Calls) != 0 {
		t.Fatalf("tool history mixed across agents/turns: %s", w.Body)
	}
	for _, forbidden := range []string{"MultiAgentCheckpoint", "PreviousRunOutcomes", "ToolExecutions", "ExecutionResult"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatalf("leaked recovery field %s", forbidden)
		}
	}
}

func TestConversationRunsExposeSavedAgentErrorsWithoutRecoveryContext(t *testing.T) {
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
	run, err := store.Create(ctx, "", "inspect", t.TempDir(), "multi-agent")
	if err != nil {
		t.Fatal(err)
	}
	rootID := run.RunID + "/delegation-1"
	graph := state.DelegationGraph[state.MultiAgentResult, state.DelegatedTask]{Nodes: map[string]*state.DelegationNode[state.MultiAgentResult, state.DelegatedTask]{rootID: {Turns: []*state.DelegationTurn[state.DelegatedTask]{{Local: &state.DurableState{RunId: run.RunID, ExecutionID: run.RunID, Error: "model unavailable", PreviousRunOutcomes: []state.RunOutcome{{Goal: "private inherited goal"}}, PlanningContext: "private planning context"}}}}}}
	checkpoint, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDurableCheckpoint(ctx, &run, &state.DurableState{RunId: run.RunID, MultiAgentCheckpoint: checkpoint}, "multi-agent"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	New(&fakeBackend{}, store).ServeHTTP(w, httptest.NewRequest("GET", "/conversations/"+run.ConvID+"/runs", nil))
	var body struct {
		Runs []struct {
			Agents []struct {
				Error string `json:"error"`
				Turns []struct {
					Error string `json:"error"`
				} `json:"turns"`
			} `json:"agents"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(body.Runs) != 1 || len(body.Runs[0].Agents) != 1 || len(body.Runs[0].Agents[0].Turns) != 1 || body.Runs[0].Agents[0].Error != "model unavailable" || body.Runs[0].Agents[0].Turns[0].Error != "model unavailable" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "private inherited goal") || strings.Contains(w.Body.String(), "private planning context") {
		t.Fatalf("leaked recovery context: %s", w.Body)
	}
}
