package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"

	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"testing"

	state "github.com/collie-agent/codex-go/internal/context"
	"github.com/collie-agent/codex-go/internal/provider"
)

type recoveryOutput struct{ events []provider.OutputEvent }

func (o *recoveryOutput) Emit(_ context.Context, e provider.OutputEvent) error {
	o.events = append(o.events, e)
	return nil
}

func TestCompletedDelegationRestoresSubtreeTerminalStates(t *testing.T) {
	for _, status := range []state.ReactStatus{CompletedReactStatus, FailedReactStatus} {
		t.Run(string(status), func(t *testing.T) {
			output := &recoveryOutput{}
			root := DelegatedTask{TaskID: "run/root", Worker: "supervisor"}
			child := DelegatedTask{TaskID: "run/child", Worker: "worker"}
			result := MultiAgentResult{Result: ReactResult{Status: status}}
			run := multiAgentRun{ctx: context.Background(), output: output, durable: &state.DurableState{RunId: "run"}, graphState: delegationGraph{Nodes: map[string]*delegationNode{
				root.TaskID:  {Done: &result, Turns: []*delegationTurn{{Children: []DelegatedTask{child}}}},
				child.TaskID: {Done: &result},
			}}}
			if _, err := run.execute(root, "", nil, "", false); err != nil {
				t.Fatal(err)
			}
			if len(output.events) != 2 {
				t.Fatalf("cached subtree missing terminal events: %+v", output.events)
			}
			want := provider.AgentCompleted
			if status == FailedReactStatus {
				want = provider.AgentFailed
			}
			for i, id := range []string{child.TaskID, root.TaskID} {
				e := output.events[i]
				var lifecycle provider.AgentLifecycleOutput
				if err := json.Unmarshal(e.Data, &lifecycle); err != nil {
					t.Fatal(err)
				}
				if e.AgentInstanceID != id || lifecycle.Status != want {
					t.Fatalf("event=%+v status=%s", e, lifecycle.Status)
				}
			}
			if output.events[0].ParentAgentInstanceID != root.TaskID {
				t.Fatal("lost parent identity")
			}
		})
	}
}

type failTerminalOutput struct {
	recoveryOutput
	fail bool
}

func (o *failTerminalOutput) Emit(ctx context.Context, e provider.OutputEvent) error {
	var lifecycle provider.AgentLifecycleOutput
	_ = json.Unmarshal(e.Data, &lifecycle)
	if o.fail && e.Kind == provider.OutputAgentLifecycle && lifecycle.Status == provider.AgentCompleted {
		o.fail = false
		return errors.New("terminal unavailable")
	}
	return o.recoveryOutput.Emit(ctx, e)
}

func TestResumeRepairsTerminalWriteFailureWithoutModelReplay(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		multiResponse(w, `{"result":{"status":"completed","answer":"done"},"next":[]}`)
	}))
	defer server.Close()
	output := &failTerminalOutput{fail: true}
	ctx := provider.WithOutputer(context.Background(), output)
	cfg := multiConfig(t, server.URL)
	durable := &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"checked"}}
	orchestrator := NewMultiAgentProcessorOrchestrator(ctx, filemcp.Config{Workspace: t.TempDir()})
	var saved *state.DurableState
	orchestrator.SetCheckpoint(func(s *state.DurableState, _ string) error { saved = s.Clone(); return nil })
	if _, err := orchestrator.Run(cfg, durable); err == nil {
		t.Fatal("terminal failure was lost")
	}
	if saved == nil {
		t.Fatal("missing completed checkpoint")
	}
	before := calls
	results, err := orchestrator.Run(cfg, saved)
	if err != nil || len(results) != 1 || calls != before {
		t.Fatalf("results=%v err=%v model calls=%d before=%d", results, err, calls, before)
	}
	last := output.events[len(output.events)-1]
	var lifecycle provider.AgentLifecycleOutput
	_ = json.Unmarshal(last.Data, &lifecycle)
	if last.AgentInstanceID != "run/delegation-1" || lifecycle.Status != provider.AgentCompleted {
		t.Fatalf("terminal not repaired: %+v", last)
	}
}
