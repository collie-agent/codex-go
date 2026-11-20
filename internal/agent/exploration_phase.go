package agent

import (
	"fmt"
	"strings"

	state "github.com/collie-agent/codex-go/internal/context"
)

// 阶段值也写入运行检查点，恢复时沿用原来的字符串表示。
type explorationPhase string

const (
	phaseExploring explorationPhase = "exploring"
	phaseImplement explorationPhase = "implement"
	phaseUnable    explorationPhase = "unable"
)

func (e *explorationState) allowance() int {
	return e.Policy.InitialTurns + e.Extensions*e.Policy.ExtensionTurns
}

func (e *explorationState) awaitingTargetChange() bool {
	return e.Phase == phaseImplement && !e.ModificationConfirmed
}

func (e *explorationState) required() bool {
	windowOpen := e.Phase == phaseImplement && e.ModificationLeft > 0
	return e.Pending || (e.TaskType != "analysis" && e.Used >= e.allowance() && !windowOpen)
}

func (e *explorationState) stopReason() string {
	if e.Pending && e.Invalid > e.Policy.DecisionRetries {
		return "exploration decision corrections exhausted: " + e.Rejection
	}
	switch e.Phase {
	case phaseUnable:
		if e.Handoff == nil {
			return "saved unable decision"
		}
		return "unable: " + e.Handoff.Reason + "; confirmed execution state: " + e.Handoff.ExecutionState
	case phaseImplement:
		if e.ModificationLeft <= 0 {
			return "modification window exhausted without confirmed target change"
		}
	}
	return ""
}

// 模型请求前预扣，使用最后一轮探索额度的响应仍可正常执行。
func (e *explorationState) reserveExplorationTurn() bool {
	if e.Phase == phaseImplement || e.TaskType == "analysis" || e.required() {
		return false
	}
	e.Used++
	return true
}

func (e *explorationState) needsDecision(result *ReactResult, hasTools bool, purpose string, reserved bool) bool {
	if result.Exploration != nil {
		return true
	}
	if reserved || !e.required() {
		return false
	}
	verification := hasTools && purpose == "verification"
	completion := !hasTools && result.Status == CompletedReactStatus && e.ModificationConfirmed
	return e.Pending || !(verification || completion)
}

func (e *explorationState) allowVerificationRetry(purpose string, calls map[string]string) bool {
	if e.VerificationRetryUsed || e.Pending || !e.ModificationConfirmed || purpose != "investigation" {
		return false
	}
	for _, callPurpose := range calls {
		if callPurpose == "verification" {
			// 按执行实例记录，恢复或后续实施交接都不会刷新这次机会。
			e.VerificationRetryUsed = true
			return true
		}
	}
	return false
}

func (e *explorationState) refundNonInvestigation(reserved bool, purpose string) bool {
	if !reserved || purpose == "investigation" {
		return false
	}
	e.Used--
	return true
}

type explorationDecisionOutcome struct {
	err        error
	correction string
	stopReason string
}

func (e *explorationState) requireCorrection(err error) explorationDecisionOutcome {
	e.Invalid++
	e.Pending = true
	e.Rejection = err.Error()
	outcome := explorationDecisionOutcome{err: err, correction: "Exploration decision rejected: " + e.Rejection}
	if e.Invalid > e.Policy.DecisionRetries {
		outcome.stopReason = e.stopReason()
	}
	return outcome
}

func (e *explorationState) applyDecision(d *state.ExplorationDecision, hasTools, reserved bool, remaining int) explorationDecisionOutcome {
	if reserved && d != nil {
		e.Used--
	}
	// 只有整个决策通过校验才提交分类、交接和延期，拒绝时按原阶段计费。
	candidate := *e
	var reason string
	var err error
	if hasTools && e.Phase == phaseImplement {
		err = fmt.Errorf("phase decision must be standalone; mixed tool batch was not executed")
	} else {
		reason, err = candidate.decide(d, remaining)
	}
	if err == nil {
		candidate.Pending = false
		*e = candidate
		return explorationDecisionOutcome{stopReason: reason}
	}
	if e.awaitingTargetChange() {
		e.ModificationLeft--
		e.Pending = false
		e.Rejection = fmt.Sprintf("Exploration decision rejected: %s; apply a target modification, submit a valid standalone extend decision or choose unable. modification_left=%d", err, e.ModificationLeft)
		return explorationDecisionOutcome{err: err}
	}
	return e.requireCorrection(err)
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
		// 使用 skip_next_operation 数字字段检测否定语义，避免字符串匹配。
		if d.SkipNextOperation == 1 {
			return "", fmt.Errorf("implement next_operation must describe a concrete modification; use normal completion for verified existing changes")
		}
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
			return "", fmt.Errorf("extension limit exhausted; choose implement or unable")
		}
		if remaining < e.Policy.ExtensionTurns {
			return "", fmt.Errorf("insufficient total decision budget for extension; choose implement or unable")
		}
		e.Extensions++
		e.Rejection = ""
		if e.Phase != phaseImplement {
			e.Phase = phaseExploring
		}
		return "", nil
	case "unable":
		if !nonempty(d.Reason, d.ExecutionState) {
			return "", fmt.Errorf("unable requires reason and execution_state")
		}
		e.Handoff = d
		e.Phase = phaseUnable
		return e.stopReason(), nil
	default:
		return "", fmt.Errorf("invalid phase choice")
	}
}
