package mcp

import "testing"

func TestSwitchToPlanExecuteRequiresHandoff(t *testing.T) {
	for _, args := range []SwitchToPlanExecuteArgs{{}, {Reason: "stages"}, {Summary: "remaining"}, {Reason: " ", Summary: "remaining"}} {
		if _, err := SwitchToPlanExecute(t.Context(), args); err == nil {
			t.Errorf("accepted incomplete handoff: %+v", args)
		}
	}
	result, err := SwitchToPlanExecute(t.Context(), SwitchToPlanExecuteArgs{Reason: "dependent stages", Summary: "already inspected; implement next"})
	if err != nil || result.Mode != "plan-execute" || result.Summary != "already inspected; implement next" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
