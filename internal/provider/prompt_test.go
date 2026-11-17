package provider

import (
	"strings"
	"testing"

	fileContext "github.com/collie-agent/codex-go/internal/context"
)

// Validate the actual examples sent to the model against the output parser.
func TestLineProtocolFewShotExamples(t *testing.T) {
	type stepExecution struct {
		Revision           int
		StepId             string
		Status             string
		Intent             string
		CompletionCriteria string
		CompletionEvidence []string
		Observation        []string
	}
	type example struct {
		StepExecutions []stepExecution
	}
	sections := strings.Split((&fileContext.PlannerContext{}).LineProtocolFewShot(), " output:\n")[1:]
	if len(sections) != 1 {
		t.Fatalf("expected one planner example, got %d", len(sections))
	}
	for i, section := range sections {
		document := strings.SplitN(section, "\n\n", 2)[0]
		var got example
		if err := (&LineProtocolOutput[*example]{}).Parse(document, &got); err != nil {
			t.Fatalf("example %d: %v", i+1, err)
		}
		if len(got.StepExecutions) != 2 {
			t.Fatalf("expected two planned steps, got %+v", got.StepExecutions)
		}
		for j, step := range got.StepExecutions {
			if step.StepId != []string{"read-notes", "write-summary"}[j] || step.Revision != 1 || step.Status != "pending" || step.Intent == "" || step.CompletionCriteria == "" || len(step.CompletionEvidence) != 0 || len(step.Observation) != 0 {
				t.Fatalf("unexpected planned step %d: %+v", j+1, step)
			}
		}
	}
}
