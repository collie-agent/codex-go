package agent

import (
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	"testing"
)

func TestFailedModificationWindowSettlement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		want     int
	}{
		{"all failed", []string{filemcp.ToolCallFailedStatus, filemcp.ToolCallFailedStatus}, 1},
		{"partial success", []string{filemcp.ToolCallFailedStatus, filemcp.ToolCallSuceessStatus}, 0},
		{"incomplete", []string{filemcp.ToolCallFailedStatus}, 0},
		{"unknown", []string{filemcp.ToolCallFailedStatus, ""}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := explorationState{Phase: "implement", Accounting: []explorationAccounting{{Batch: 1, ModificationCharged: true, Outcome: "pending", Calls: map[string]string{"a": "modification", "b": "modification"}}}}
			var calls []*state.ToolResult
			for i, status := range tc.statuses {
				calls = append(calls, &state.ToolResult{Batch: 1, ToolCallId: []string{"a", "b"}[i], Status: filemcp.ToolCallStatus(status)})
			}
			e.settleFailedInvestigation(&e.Accounting[0], calls)
			e.reconcile(calls)
			e.reconcile(calls)
			if e.ModificationLeft != tc.want {
				t.Fatalf("remaining=%d, want %d", e.ModificationLeft, tc.want)
			}
		})
	}
}
