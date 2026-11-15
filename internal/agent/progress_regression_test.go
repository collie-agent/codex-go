package agent

import (
	"encoding/json"
	"testing"

	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

func TestSessionProgressAfterDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, status, errorKind string
		want                    bool
	}{
		{"completed before delayed poll", "exited", "", true},
		{"running past deadline", "running", "", false},
		{"actual timeout with final output", "exited", "timeout", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"session_id": "session", "state": tc.status, "error_kind": tc.errorKind, "elapsed_ms": 2000, "lifetime_ms": 1000, "next_cursor": 10})
			if err != nil {
				t.Fatal(err)
			}
			content, err := json.Marshal([]map[string]string{{"text": string(payload)}})
			if err != nil {
				t.Fatal(err)
			}
			call := &state.ToolResult{ToolName: "exec_poll", Result: string(content), Status: filemcp.ToolCallSuceessStatus}
			guard := newFileProgressGuard(FileProgressPolicy{})
			// The last recovery batch must accept a previously unseen successful exit.
			guard.recoveryActive, guard.recoveryLeft = true, 1
			_, err = guard.observe(BatchEvidence{Completed: []*state.ToolResult{call}}, &state.DurableState{}, 10)
			if (err == nil) != tc.want {
				t.Fatalf("recovery error = %v, want progress %v", err, tc.want)
			}
			if tc.want && guard.observeSession(call) {
				t.Fatal("replayed exit counted as new evidence")
			}
		})
	}
}

func TestFileProgressCheckpointRangeCompatibility(t *testing.T) {
	// Existing checkpoints store file lines inclusively, while history uses half-open offsets.
	durable := &state.DurableState{ExecutionID: "exec", ExecutionProgress: []byte(`{"ExecutionID":"exec","Coverage":{"file\u0000v":[{"Start":1,"End":2}]},"HistoryCoverage":{"call\u00000":[{"Start":0,"End":2}]}}`)}
	guard := newFileProgressGuard(FileProgressPolicy{})
	progress, err := restoreDecisionProgress(durable, guard)
	if err != nil {
		t.Fatal(err)
	}
	if guard.add(&filemcp.FileEvidence{Path: "file", Version: "v", StartLine: 1, EndLine: 2}) {
		t.Fatal("restored lines counted twice")
	}
	if !guard.add(&filemcp.FileEvidence{Path: "file", Version: "v", StartLine: 3, EndLine: 4}) {
		t.Fatal("adjacent new lines ignored")
	}
	if guard.add(&filemcp.FileEvidence{Path: "file", Version: "v", StartLine: 1, EndLine: 4}) {
		t.Fatal("merged lines counted twice")
	}
	if guard.historyCoverage.add(&state.HistoryReadEvidence{SourceCallID: "call", Start: 0, End: 2}, 0) {
		t.Fatal("restored history counted twice")
	}
	progress.capture(guard)
	got := progress.Coverage["file\x00v"]
	if len(got) != 1 || got[0] != (progressRange{1, 4}) {
		t.Fatalf("changed persisted interval convention: %v", got)
	}
}
