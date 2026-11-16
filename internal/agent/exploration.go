package agent

import (
	"fmt"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
)

type explorationState struct {
	Accounting            []explorationAccounting
	SessionPurposes       map[string]string
	SessionBaselines      map[string]sessionModificationBaseline
	Extensions            int
	Rejection             string
	ModificationLeft      int
	ModificationConfirmed bool
	ModificationEvidence  []modificationEvidence
	Policy                config.ExplorationBudget
	TaskType              string
	Phase                 string
	Used                  int
	Invalid               int
	Pending               bool
	Handoff               *state.ExplorationDecision
	Handoffs              []state.ExplorationDecision
}

const explorationInstructions = `Exploration budget: classify the task after obtaining context, at the latest when the exploration allowance is exhausted. Return an independent exploration object (EXPLORATION child block for line protocol) with task_type modification or analysis and a visible basis. An analysis classification must explain why the original goal needs no code changes; it cannot discard a modification goal. At the threshold return exploration with choice implement, extend, or unable, without tools. implement establishes a modification task and requires target_files (workspace-relative paths), target (expected changes), evidence, next_operation; a legacy single file in target is also accepted; extend requires gap, blocking_reason, investigation, exit_condition; unable requires reason and execution_state. A decision response never executes accompanying tools. Do not use completed as a phase switch. If investigation proves no change necessary, include concrete evidence in exploration.evidence before completing. Implementation requires confirmed target file changes within the modification window. Investigation still consumes exploration allowance; a modification window does not grant free exploration. Extensions are runtime-approved only when allowance, extension count and total decision budget permit. Rejected extensions require implement or unable; do not repeat an exhausted request. Runtime tool accounting recognizes go test with supported standard test flags as verification, simple sed -i substitutions and apply_patch as modification, and git diff -- exact confirmed target paths as result verification after actual modification. Source reads/searches, arbitrary scripts/interpreters and unsupported command options conservatively consume exploration; claiming verification does not exempt them. Mixed batches containing investigation consume one exploration turn. Process polling/stopping and stored result reads inherit the originating command/call purpose; unknown identities remain investigation. Run verification commands separately from modification operations: changes in mixed batches have unknown attribution. Only confirmed target file changes prove implementation, never workspace_version, test outputs, exit status or a declaration.

Exploration decision examples (adapt paths and evidence to actual observations; never invent evidence):
The JSON below shows ordinary ReAct responses. For multi-agent responses, wrap each exploration object as {"result":{"exploration":{...}},"next":[]} and omit result.status and result.answer. For Line Protocol, express the same fields in an EXPLORATION block inside the appropriate RESULT. These are independent phase decisions without tool calls; perform the next operation only after runtime acceptance.

Example 1 — implement when the cause and target are known.
Context: The user requested a nil-input fix. Reading internal/parser/parser.go confirmed Parse dereferences input before checking it; the exploration allowance is exhausted.
Response:
{"exploration":{"task_type":"modification","basis":"The user requested a code fix for nil input.","choice":"implement","target_files":["internal/parser/parser.go"],"target":"Return an error for nil input before dereferencing it in Parse.","evidence":"The observed Parse implementation dereferences input before its nil check.","next_operation":"Apply a patch moving the nil check before the dereference."}}

Example 2 — request a bounded extension for a specific blocking gap.
Context: The user requested a timeout fix. The timeout branch is known, but its caller's cancellation contract has not been read. The exploration allowance is exhausted and extensions remain available.
Response:
{"exploration":{"task_type":"modification","basis":"The requested timeout fix requires changing cancellation handling.","choice":"extend","gap":"The caller's ownership of cancellation is unknown.","blocking_reason":"Changing cancellation without its contract could cancel shared work.","investigation":"Read the direct caller and its cancellation test.","exit_condition":"Identify who owns cancellation and select the exact branch to patch."}}

Example 3 — stop when a required dependency is unavailable and extension was rejected.
Context: A requested API migration depends on a private schema absent from the workspace. Access failed, and runtime rejected further extensions. No files have changed.
Response:
{"exploration":{"task_type":"modification","basis":"The user requested an API migration requiring code changes.","choice":"unable","reason":"The required private schema is unavailable and no further extension is permitted.","execution_state":"Confirmed the schema is absent and access failed; no files changed and migration validation has not run."}}

Example 4 — classify a genuinely analytical goal without forcing a modification.
Context: The user only asked how Parse handles nil input. Reading the function established the answer; no code change was requested.
Response:
{"exploration":{"task_type":"analysis","basis":"The goal is to explain existing nil-input handling, which can be answered from the inspected function without changing code."}}
After this classification is accepted, provide the explanation in a subsequent response using the normal answer protocol.`

func (e *explorationState) allowance() int {
	return e.Policy.InitialTurns + e.Extensions*e.Policy.ExtensionTurns
}

func (e *explorationState) required() bool {
	return e.Pending || (e.TaskType != "analysis" && e.Used >= e.allowance() && !(e.Phase == "implement" && e.ModificationLeft > 0))
}
func (e *explorationState) decide(d *state.ExplorationDecision, remaining int) (string, error) {
	if d == nil {
		return "", fmt.Errorf("exploration threshold requires classification and phase decision")
	}
	nonempty := func(values ...string) bool {
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return false
			}
		}
		return true
	}
	switch d.TaskType {
	case "analysis":
		if !nonempty(d.Basis) {
			return "", fmt.Errorf("analysis requires basis")
		}
		// 一旦确认修改目标，不允许仅重命名任务类型；无须改动必须交付证据。
		if e.TaskType == "modification" && !nonempty(d.Evidence) {
			return "", fmt.Errorf("modification goal requires concrete no-change evidence")
		}
	case "modification":
		if !nonempty(d.Basis) {
			return "", fmt.Errorf("modification classification requires basis")
		}
	case "":
		if e.TaskType == "" {
			return "", fmt.Errorf("task_type required")
		}
	default:
		return "", fmt.Errorf("invalid task_type")
	}
	if d.TaskType != "" {
		e.TaskType = d.TaskType
	}
	if d.Choice == "" {
		if e.TaskType != "analysis" && e.Used >= e.allowance() {
			return "", fmt.Errorf("phase choice required")
		}
		return "", nil
	}
	switch d.Choice {
	case "implement":
		if !nonempty(d.Target, d.Evidence, d.NextOperation) {
			return "", fmt.Errorf("implement requires target, evidence, next_operation")
		}
		if err := e.beginModification(d); err != nil {
			return "", err
		}
		// 实施承诺确认了修改目标，不能继续享有分析任务的探索豁免。
		e.TaskType = "modification"
		return "", nil
	case "extend":
		if !nonempty(d.Gap, d.BlockingReason, d.Investigation, d.ExitCondition) {
			return "", fmt.Errorf("extend requires gap, blocking_reason, investigation, exit_condition")
		}
		// 审批使用扣除当前决策与失败收尾预留后的额度，不增加总预算。
		if e.Extensions >= e.Policy.MaxExtensions {
			e.Rejection = "extension limit exhausted; choose implement or unable"
			return "", fmt.Errorf("%s", e.Rejection)
		}
		if remaining < e.Policy.ExtensionTurns {
			e.Rejection = "insufficient total decision budget for extension; choose implement or unable"
			return "", fmt.Errorf("%s", e.Rejection)
		}
		e.Extensions++
		e.Rejection = ""
		if e.Phase != "implement" {
			e.Phase = "exploring"
		}
		return "", nil
	case "unable":
		if !nonempty(d.Reason, d.ExecutionState) {
			return "", fmt.Errorf("unable requires reason and execution_state")
		}
	default:
		return "", fmt.Errorf("invalid phase choice")
	}
	e.Handoff = d
	e.Phase = d.Choice
	if d.Choice == "unable" {
		return "unable: " + d.Reason + "; confirmed execution state: " + d.ExecutionState, nil
	}
	return "", nil
}
