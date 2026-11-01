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
		if modelInput != rc.format(false, false) || strings.Contains(modelInput, "```json") || strings.Contains(modelInput, "# sysPrompt") {
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

func TestPreviousAgentRunOutcomesFollowAdditionalInstructions(t *testing.T) {
	rc := NewReactContext(&DurableState{RunId: "current", Goal: "continue", PreviousRunOutcomes: []RunOutcome{
		{RunID: "earlier", Result: `{"answer":"saved report","Observation":["checked"]}`, Error: "first attempt failed\nretry succeeded"},
	}})
	rc.SetSysPrompt("Use Chinese.")
	// 系统提示词独立发送；历史结果必须紧随补充指令进入同一模型请求。
	rc.FormatForModel()
	want := "# Additional instructions\n\nUse Chinese.\n\n# Conversation history\n\n## Agent run\n\n- Run ID:\n\n  > earlier\n\n- Result:\n\n  > - Observation:\n  >   - Item 1:\n  > \n  >     > checked\n  > \n  > - answer:\n  > \n  >   > saved report\n\n- Error:\n\n  > first attempt failed\n  > retry succeeded\n"
	if !strings.HasSuffix(rc.SystemPrompt(), want) || !strings.HasSuffix(rc.ModelRequest().SystemPrompt, want) {
		t.Fatalf("system prompt missing ordered Markdown outcomes: %s", rc.SystemPrompt())
	}
}

func TestHistoricalToolsRemainMarkdownReference(t *testing.T) {
	rc := NewReactContext(&DurableState{RunId: "current", Goal: "another task", PreviousRunOutcomes: []RunOutcome{
		{RunID: "prior", History: "- Goal:\n  > inspect\n", ToolResults: []*ToolResult{{ToolCallId: "old-call", ToolName: "read_file", Arguments: "{}", Status: "success", Result: "line one\n# embedded heading\n- embedded item"}}},
	}})
	rc.AppendToolResult(&ToolResult{ToolCallId: "current-call", ToolName: "read_file", Status: "success", Result: "current result"})
	_, tools := rc.FormatForModel()
	if len(tools) != 1 || tools[0].ToolCallId != "current-call" {
		t.Fatalf("historical calls leaked into current tool messages: %+v", tools)
	}
	prompt := rc.SystemPrompt()
	for _, want := range []string{"## Agent run", "- Tool call:", "old-call", "  > line one\n  > # embedded heading\n  > - embedded item"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("missing scoped historical evidence %q: %s", want, prompt)
		}
	}
}

func TestHistoricalResultDecodesJSONNewlines(t *testing.T) {
	rc := NewReactContext(&DurableState{PreviousRunOutcomes: []RunOutcome{{RunID: "old", Result: `{"answer":"first\nsecond\nkeep literal \\n","status":"completed"}`}}})
	prompt := rc.ModelRequest().SystemPrompt
	if !strings.Contains(prompt, "> first\n") || !strings.Contains(prompt, "> second\n") || !strings.Contains(prompt, `keep literal \n`) {
		t.Fatalf("historical JSON text was not decoded exactly once: %s", prompt)
	}
}
