package agent

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/collie-agent/codex-go/internal/config"
	fileContext "github.com/collie-agent/codex-go/internal/context"
)

func TestReactStepResult(t *testing.T) {
	for _, schema := range []config.LLMOutputSchema{config.JsonLLMOutputSchema, config.LineProtocolLLMOutputSchema} {
		for _, tc := range []struct {
			name, json, line, wantError string
		}{
			{"completed", `{"status":"completed","answer":"step done","observation":["read file","checked value"],"CompletionEvidence":["value matches criterion","check passed"]}`, "STATUS=\"completed\"\nANSWER=\"step done\"\nOBSERVATION=\"read file\"\nOBSERVATION=\"checked value\"\nCOMPLETION_EVIDENCE=\"value matches criterion\"\nCOMPLETION_EVIDENCE=\"check passed\"", ""},
			{"missing observation", `{"status":"completed","answer":"step done","CompletionEvidence":["check passed"]}`, "STATUS=\"completed\"\nANSWER=\"step done\"\nCOMPLETION_EVIDENCE=\"check passed\"", "requires non-empty observation and CompletionEvidence"},
			{"missing evidence", `{"status":"completed","answer":"step done","observation":["read file"]}`, "STATUS=\"completed\"\nANSWER=\"step done\"\nOBSERVATION=\"read file\"", "requires non-empty observation and CompletionEvidence"},
			{"blank evidence", `{"status":"completed","answer":"step done","observation":["read file"],"CompletionEvidence":[" "]}`, "STATUS=\"completed\"\nANSWER=\"step done\"\nOBSERVATION=\"read file\"\nCOMPLETION_EVIDENCE=\" \"", "requires non-empty observation and CompletionEvidence"},
			{"failed", `{"status":"failed","answer":"permission denied","observation":["read failed"],"CompletionEvidence":[]}`, "STATUS=\"failed\"\nANSWER=\"permission denied\"\nOBSERVATION=\"read failed\"", "ReAct reported failure: permission denied"},
		} {
			t.Run(string(schema)+"/"+tc.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					text := tc.json
					if schema == config.LineProtocolLLMOutputSchema {
						text = "BEGIN RESULT\n" + tc.line + "\nEND RESULT"
					}
					writeObservationResponse(w, []any{observationMessage(text)})
				}))
				defer server.Close()
				cfg := &config.AgentConfig{LLMConfig: &config.LLMConfig{BaseUrl: server.URL, ApiKey: "test", Model: "test", OutputSchema: schema}}
				state := &fileContext.DurableState{RunId: "step-result", Goal: "verify value", StepExecutions: []fileContext.StepExecution{{Status: fileContext.RunningExecutionStatus, CompletionCriteria: "value verified"}}}
				process := &ReactProcess[ReactResult]{ctx: t.Context(), maxTurn: 1}
				got, _, err := process.Run(cfg, state)
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("Run() = %+v, %v; want %q", got, err, tc.wantError)
					}
					return
				}
				if err != nil || !slices.Equal(got.Observation, []string{"read file", "checked value"}) || !slices.Equal(got.CompletionEvidence, []string{"value matches criterion", "check passed"}) {
					t.Fatalf("Run() = %+v, %v", got, err)
				}
			})
		}
	}
}
