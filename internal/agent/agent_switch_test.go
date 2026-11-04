package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"github.com/collie-agent/codex-go/internal/provider"
)

type switchOutput struct {
	mu     sync.Mutex
	events []provider.OutputEvent
	err    error
}

func (o *switchOutput) Emit(_ context.Context, e provider.OutputEvent) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if e.Kind == "agent_switch" {
		o.events = append(o.events, e)
		return o.err
	}
	return nil
}

func TestMultiAgentEmitsDelegationAndReturnSwitches(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"check","completion_criteria":["checked"]}]}`)
		case 2:
			multiResponse(w, `{"result":{"status":"completed","answer":"checked"},"next":[{"agent":"supervisor"}]}`)
		default:
			multiResponse(w, `{"result":{"status":"completed","answer":"done"},"next":[]}`)
		}
	}))
	defer model.Close()
	output := &switchOutput{}
	o := NewMultiAgentProcessorOrchestrator(provider.WithOutputer(t.Context(), output), filemcp.Config{Workspace: t.TempDir()})
	if _, err := o.Run(multiConfig(t, model.URL), &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"checked"}}); err != nil {
		t.Fatal(err)
	}
	var got []map[string]string
	for _, e := range output.events {
		var body map[string]string
		if err := json.Unmarshal(e.Data, &body); err != nil {
			t.Fatal(err)
		}
		got = append(got, body)
	}
	want := []map[string]string{{"src_agent_name": "supervisor", "dst_agent_name": "worker"}, {"src_agent_name": "worker", "dst_agent_name": "supervisor"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("switches=%v, want %v", got, want)
	}
	for i, id := range []string{"run/delegation-1", "run/delegation-2"} {
		if e := output.events[i]; e.RunID != "run" || e.TaskID != "run" || e.AgentInstanceID != id {
			t.Fatalf("switch attribution=%+v", e)
		}
	}
}

func TestMultiAgentStopsWhenSwitchOutputFails(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			var calls atomic.Int32
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					multiResponse(w, `{"result":{"status":"completed","answer":"delegate"},"next":[{"agent":"worker","task":"check","completion_criteria":["checked"]}]}`)
				} else {
					multiResponse(w, `{"result":{"status":"completed","answer":"checked"},"next":[]}`)
				}
			}))
			defer model.Close()
			sentinel := errors.New("switch storage unavailable")
			output := &failingSwitchOutput{failAt: failAt, err: sentinel}
			o := NewMultiAgentProcessorOrchestrator(provider.WithOutputer(t.Context(), output), filemcp.Config{Workspace: t.TempDir()})
			_, err := o.Run(multiConfig(t, model.URL), &state.DurableState{RunId: "run", Goal: "inspect", CompletionCriteria: []string{"checked"}})
			if !errors.Is(err, sentinel) || calls.Load() != int32(failAt) {
				t.Fatalf("error=%v model calls=%d", err, calls.Load())
			}
		})
	}
}

type failingSwitchOutput struct {
	failAt, seen int
	err          error
}

func (o *failingSwitchOutput) Emit(_ context.Context, e provider.OutputEvent) error {
	if e.Kind == "agent_switch" {
		o.seen++
		if o.seen == o.failAt {
			return o.err
		}
	}
	return nil
}
