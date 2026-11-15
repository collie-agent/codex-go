package agent

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
)

func TestObserveProgressLogsEveryOutcome(t *testing.T) {
	var logs bytes.Buffer
	writer := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(writer)
	guard := newFileProgressGuard(FileProgressPolicy{StalledBatches: 1, RecoveryWindow: 1})
	durable := &state.DurableState{RunId: "progress-log", Goal: "inspect", CompletionCriteria: []string{"verified"}}
	batch := BatchEvidence{Cached: []*state.ToolResult{{ToolCallId: "cached-read", ToolName: string(config.ReadFileMCPToolName), Arguments: `{"path":"notes.txt"}`, Result: "cached content"}}}
	for _, tc := range []struct {
		name                string
		batch               BatchEvidence
		correction, failure bool
	}{
		{name: "no correction"},
		{name: "correction", batch: batch, correction: true},
		{name: "exhausted", batch: batch, failure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			correction, err := guard.observe(tc.batch, durable, 3)
			if (correction != "") != tc.correction || (err != nil) != tc.failure {
				t.Fatalf("observe = %q, %v", correction, err)
			}
			for _, want := range []string{"task progress-log", "observe progress input=", `"goal":"inspect"`, `"remaining":3`, "output=", `"correction_triggered":` + map[bool]string{true: "true", false: "false"}[tc.correction], `"error":`} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("missing %q in %s", want, logs.String())
				}
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 2 || !strings.Contains(lines[0], "observe progress input=") || !strings.Contains(lines[1], "observe progress output=") {
				t.Fatalf("expected separate input and output log lines, got %s", logs.String())
			}
			if tc.batch.Cached != nil {
				for _, want := range []string{"cached-read", "notes.txt", "cached content"} {
					if !strings.Contains(logs.String(), want) {
						t.Errorf("missing input %q in %s", want, logs.String())
					}
				}
			}
		})
	}
}
