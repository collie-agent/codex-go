package context

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFormatForLogIncludesCompleteDurableState(t *testing.T) {
	for _, state := range []*DurableState{
		nil,
		{RunId: "cli-task", Goal: "inspect agents"},
		{
			RunId: "planned-task", Goal: "inspect agents", CompletionCriteria: []string{"relationships explained"},
			CurrentStepIndex: 1,
			StepExecutions: []StepExecution{
				{Revision: 1, StepId: "read", Status: CompletedExecutionStatus, Intent: "read components", CompletionCriteria: "components read", Observation: []string{"found planner"}, CompletionEvidence: []string{"read planner.go"}},
				{Revision: 2, StepId: "explain", Status: RunningExecutionStatus, Intent: "explain relationships", CompletionCriteria: "relationships explained"},
			},
		},
	} {
		rc := NewReactContext(state)
		formatted := rc.FormatForLog()
		_, block, ok := strings.Cut(formatted, "# DurableState\n\n```json\n")
		if !ok {
			t.Fatalf("missing durable state JSON: %s", formatted)
		}
		encoded, _, ok := strings.Cut(block, "\n```")
		if !ok {
			t.Fatalf("unterminated durable state JSON: %s", block)
		}
		var got *DurableState
		if err := json.Unmarshal([]byte(encoded), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, state) {
			t.Fatalf("logged state = %+v, want %+v", got, state)
		}
		if state != nil {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
				t.Fatal(err)
			}
			stateType := reflect.TypeOf(*state)
			for i := 0; i < stateType.NumField(); i++ {
				if _, ok := fields[stateType.Field(i).Name]; !ok {
					t.Errorf("log omitted field %s", stateType.Field(i).Name)
				}
			}
		}
		modelInput, _ := rc.FormatForModel()
		if modelInput != rc.Format() || strings.Contains(modelInput, "```json") {
			t.Fatal("log formatting changed model input")
		}
	}
}

func TestCloneIsolatesStepResults(t *testing.T) {
	state := &DurableState{StepExecutions: []StepExecution{{Observation: []string{"observed"}, CompletionEvidence: []string{"verified"}}}}
	clone := state.Clone()
	clone.StepExecutions[0].Observation[0] = "changed observation"
	clone.StepExecutions[0].CompletionEvidence[0] = "changed evidence"
	if state.StepExecutions[0].Observation[0] != "observed" || state.StepExecutions[0].CompletionEvidence[0] != "verified" {
		t.Fatalf("Clone shared step result slices: %+v", state)
	}
}
