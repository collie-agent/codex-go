package agent

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 空用途表示必须查看调用参数或历史用途才能确定。
func fixedExplorationPurpose(name config.McpToolName) string {
	switch name {
	case config.ApplyPatchMCPToolName:
		return "modification"
	case config.ExecMCPToolName, config.ExecStartMCPToolName, config.ExecPollMCPToolName,
		config.ExecStopMCPToolName, config.ReadToolResultMCPToolName:
		return ""
	default:
		return "investigation"
	}
}

// 仅识别有明确语法的业务操作；解释器、项目脚本和未知选项保守计为调查。
func explorationPurpose(param *sdk.CallToolParams) string {
	if purpose := fixedExplorationPurpose(config.McpToolName(param.Name)); purpose != "" {
		return purpose
	}
	switch param.Name {
	case "exec", "exec_start":
		raw, _ := json.Marshal(param.Arguments)
		var args struct {
			Command string
			Args    []string
		}
		if json.Unmarshal(raw, &args) != nil {
			return "investigation"
		}
		return filemcp.CommandPurpose(args.Command, args.Args)
	}
	return "verification" // 进程会话与历史回读由 batchPurpose 按原调用用途覆盖。
}

// 保存每批工具调用的用途、扣费状态，以及确认目标文件变更所需的证据
// 批次是并行调用的证据边界；不把净变化归因到某一个命令。
type explorationAccounting struct {
	Batch               int
	Handoff             int
	Charged             bool `json:",omitempty"`
	ModificationCharged bool `json:",omitempty"`
	Applied             bool
	SinceBatch          int    `json:",omitempty"`
	Purpose             string //investigation,modification,workspace_check,verification
	Calls               map[string]string
	Before              map[string]targetSnapshot `json:",omitempty"`
	After               map[string]targetSnapshot `json:",omitempty"`
	Outcome             string
}

// 仅在所有调查调用均明确失败时退回预扣；未知结果保持原有停止边界。
// Charged 与预算一起落盘，检查点回调和恢复可重复结算而不会重复退款。
func (e *explorationState) settleFailedInvestigation(record *explorationAccounting, completed []*state.ToolResult) {
	e.settleFailedModification(record, completed)
	if !record.Charged {
		return
	}
	failed := make(map[string]bool)
	for _, call := range completed {
		failed[call.ToolCallId] = call.Status == filemcp.ToolCallFailedStatus
	}
	for id, purpose := range record.Calls {
		if purpose == "investigation" && !failed[id] {
			return
		}
	}
	e.Used--
	record.Charged = false
}

// 实施窗口按批次预扣；仅所有调用明确失败才退还，未知或部分成功仍计费。
// 标记与退款一起持久化，检查点回调和恢复重复结算不会增加窗口。
func (e *explorationState) settleFailedModification(record *explorationAccounting, completed []*state.ToolResult) {
	if !record.ModificationCharged || len(record.Calls) == 0 {
		return
	}
	failed := make(map[string]bool)
	for _, call := range completed {
		failed[call.ToolCallId] = call.Status == filemcp.ToolCallFailedStatus
		// 命令非零退出的 MCP 状态仍可为 success；实施窗口按真实退出结果退款。
		// 运行中、缺少退出码或无法解析的结果不属于已确认失败。
		if call.Status == filemcp.ToolCallSuceessStatus && isCommandProgressTool(call.ToolName) {
			var out struct {
				State     string `json:"state"`
				ExitCode  *int   `json:"exit_code"`
				ErrorKind string `json:"error_kind"`
			}
			if commandResultPayload(call, &out) && out.State != "running" && out.ExitCode != nil {
				failed[call.ToolCallId] = *out.ExitCode != 0 || out.ErrorKind != ""
			}
		}
	}
	for id := range record.Calls {
		if !failed[id] {
			return
		}
	}
	if e.Phase == phaseImplement && record.Handoff == len(e.Handoffs) {
		e.ModificationLeft++
	}
	record.ModificationCharged = false
}

// 同一批次的测试可能生成目标文件，不能将净变化归因于并行的修改操作。
// 用途来自运行时分类；进程轮询沿用启动用途，恢复无需重读工作区。
func (r *explorationAccounting) hasVerificationCommand(completed []*state.ToolResult) bool {
	for _, call := range completed {
		if !isCommandProgressTool(call.ToolName) {
			continue
		}
		if r.Calls[call.ToolCallId] == "verification" {
			return true
		}
		if call.ToolName == "exec" || call.ToolName == "exec_start" {
			var args struct {
				Command string
				Args    []string
			}
			if json.Unmarshal([]byte(call.Arguments), &args) == nil && filemcp.CommandPurpose(args.Command, args.Args) == "verification" {
				return true
			}
		}
	}
	return false
}

// 检查点回调和正常完成共享相同的证据可信度规则。
func (r *explorationAccounting) updateOutcome(completed []*state.ToolResult) {
	r.Outcome = "known"
	if !modificationBatchKnown(completed) {
		r.Outcome = "unknown"
	}
	if r.Before != nil && r.hasVerificationCommand(completed) {
		r.Outcome = "unknown"
	}
	for path, before := range r.Before {
		if !before.Known || !r.After[path].Known {
			r.Outcome = "unknown"
		}
	}
}

// 只重放落盘的批次前后证据，绝不使用恢复时的工作区推断过去的修改。
// Applied 与阶段一起保存，使多次恢复既不重复确认，也不刷新修改窗口。
func (e *explorationState) reconcile(completed []*state.ToolResult) {
	if len(completed) == 0 {
		return
	}
	for i := range e.Accounting {
		record := &e.Accounting[i]
		if record.Batch != completed[0].Batch || record.Applied {
			continue
		}
		e.settleFailedInvestigation(record, completed)
		if record.Outcome == "known" {
			record.updateOutcome(completed)
		}
		if record.Outcome == "known" && record.Handoff == len(e.Handoffs) && (record.Handoff == 0 || e.Phase == phaseImplement) && modificationBatchKnown(completed) && (modificationBatchCandidate(completed) || record.SinceBatch > 0) {
			e.confirmModification(record.Before, record.After, record.Batch)
		}
		if record.Outcome == "pending" {
			record.Outcome = "unknown"
		}
		record.Applied = true
	}
}

// 进程已结束时保存从启动到退出的批次基准；恢复只消费证据，不重连旧进程。
func (e *explorationState) captureSessionBaseline(completed []*state.ToolResult, record *explorationAccounting) {
	for _, call := range completed {
		if call.ToolName != "exec_poll" && call.ToolName != "exec_stop" {
			continue
		}
		var query struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(call.Arguments), &query) != nil {
			continue
		}
		if baseline, ok := e.SessionBaselines[query.SessionID]; ok && modificationBatchKnown([]*state.ToolResult{call}) {
			record.Before = baseline.Targets
			record.SinceBatch = baseline.Batch
		}
	}
}

/*
返回

purpose 是整批的用途，供主循环判断探索额度和批次准入。
calls 是 调用 ID → 用途 的映射，供批次记账、失败退款和后续查询使用
整批采用其中优先级最高的用途：

investigation（调查） > modification（修改） > workspace_check（工作区检查） > verification（验证）

例如，同一批包含 rg 搜索和 apply_patch，calls 会分别记录 investigation 和 modification，整批则按 investigation 处理。如果实施阶段的探索额度已经用尽，这一批会整体被拒绝
*/
func (e *explorationState) batchPurpose(params []*sdk.CallToolParams) (string, map[string]string) {
	purpose := "verification"
	calls := make(map[string]string)
	for _, param := range params {
		next := explorationPurpose(param)
		if e.verifiesModifiedTargets(param) {
			next = "verification"
		}
		if param.Name == "exec_poll" || param.Name == "exec_stop" {
			raw, _ := json.Marshal(param.Arguments)
			var query struct {
				SessionID string `json:"session_id"`
			}
			_ = json.Unmarshal(raw, &query)
			next = e.SessionPurposes[query.SessionID]
			if next == "" {
				next = "investigation"
			}
		}
		if param.Name == "read_tool_result" {
			raw, _ := json.Marshal(param.Arguments)
			var query struct {
				CallID string `json:"call_id"`
			}
			_ = json.Unmarshal(raw, &query)
			next = "investigation"
			for i := len(e.Accounting) - 1; i >= 0; i-- {
				if original, ok := e.Accounting[i].Calls[query.CallID]; ok {
					next = original
					break
				}
			}
		}
		id, _ := param.Meta[toolCallIDMetaKey].(string)
		calls[id] = next
		// 调查优先于修改、工作区检查和验证，禁止混合批次借低成本用途部分放行。
		if next == "investigation" {
			purpose = next
		} else if next == "modification" && purpose != "investigation" {
			purpose = next
		} else if next == "workspace_check" && purpose == "verification" {
			purpose = next
		}
	}
	return purpose, calls
}

func (e *explorationState) recordCommandSessions(completed []*state.ToolResult, calls map[string]string, before map[string]targetSnapshot, batch int) {
	for _, call := range completed {
		if call.ToolName != "exec_start" {
			continue
		}
		var out struct {
			SessionID string `json:"session_id"`
		}
		if commandResultPayload(call, &out) && out.SessionID != "" {
			if e.SessionPurposes == nil {
				e.SessionPurposes = make(map[string]string)
			}
			e.SessionPurposes[out.SessionID] = calls[call.ToolCallId]
			if before != nil && !(&explorationAccounting{Calls: calls}).hasVerificationCommand(completed) && modificationBatchCandidate([]*state.ToolResult{call}) {
				if e.SessionBaselines == nil {
					e.SessionBaselines = make(map[string]sessionModificationBaseline)
				}
				e.SessionBaselines[out.SessionID] = sessionModificationBaseline{Batch: batch, Targets: before}
			}
		}
	}
}

func modificationBatchKnown(completed []*state.ToolResult) bool {
	for _, call := range completed {
		if !isCommandProgressTool(call.ToolName) {
			continue
		}
		var out struct {
			ErrorKind string `json:"error_kind"`
			State     string `json:"state"`
			ExitCode  *int   `json:"exit_code"`
		}
		if !commandResultPayload(call, &out) || out.ErrorKind != "" || out.ExitCode == nil || out.State == "running" {
			return false
		}
	}
	return true
}

// 只读调查和测试即便碰巧伴随目标变化，也不构成实施操作。
// 未知解释器仍需扣探索；其成功执行后的目标净变化才是证据。
func modificationBatchCandidate(completed []*state.ToolResult) bool {
	for _, call := range completed {
		if call.ToolName == "apply_patch" {
			return true
		}
		if call.ToolName != "exec" && call.ToolName != "exec_start" {
			continue
		}
		var args struct {
			Command string
			Args    []string
		}
		if json.Unmarshal([]byte(call.Arguments), &args) != nil {
			continue
		}
		if filemcp.CommandPurpose(args.Command, args.Args) == "verification" {
			continue
		}
		switch args.Command {
		case "rg", "grep", "find", "go", "cargo", "rustc":
			continue
		case "sed":
			if len(args.Args) == 0 || args.Args[0] != "-i" {
				continue
			}
		case "git":
			if len(args.Args) == 0 {
				continue
			}
			switch args.Args[0] {
			case "diff", "status", "show", "log", "ls-files", "grep":
				continue
			}
		}
		return true
	}
	return false
}

// 仅核验本实例已有实际修改证据的精确路径；全仓 diff 和历史调查仍计探索。
func (e *explorationState) verifiesModifiedTargets(param *sdk.CallToolParams) bool {
	if param.Name != "exec" || !e.ModificationConfirmed {
		return false
	}
	raw, _ := json.Marshal(param.Arguments)
	var args struct {
		Command string
		Args    []string
	}
	if json.Unmarshal(raw, &args) != nil || args.Command != "git" || len(args.Args) < 3 || args.Args[0] != "diff" || args.Args[1] != "--" {
		return false
	}
	for _, path := range args.Args[2:] {
		found := false
		for _, evidence := range e.ModificationEvidence {
			if evidence.Path == path {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type sessionModificationBaseline struct {
	Batch   int
	Targets map[string]targetSnapshot
}

func (e *explorationState) confirmSessionModification(completed []*state.ToolResult, after map[string]targetSnapshot, record *explorationAccounting) {
	for _, call := range completed {
		if call.ToolName != "exec_poll" && call.ToolName != "exec_stop" {
			continue
		}
		var query struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(call.Arguments), &query) != nil {
			continue
		}
		baseline, ok := e.SessionBaselines[query.SessionID]
		if !ok || !modificationBatchKnown([]*state.ToolResult{call}) {
			continue
		}
		record.Before = baseline.Targets
		record.SinceBatch = baseline.Batch
		e.confirmModification(baseline.Targets, after, record.Batch)
		delete(e.SessionBaselines, query.SessionID)
	}
}

// 完成证据从本实例落盘的实际调用推导，恢复时不凭模型声明或当前工作区补造。
func (e *explorationState) preHandoffVerified(history []*state.ToolResult) bool {
	latest := 0
	for _, evidence := range e.ModificationEvidence {
		if evidence.Batch > latest {
			latest = evidence.Batch
		}
	}
	if latest == 0 {
		return false
	}
	// 以后续验证为准，不能让较早的成功掩盖最新失败或未知结果。
	for i := len(e.Accounting) - 1; i >= 0; i-- {
		record := e.Accounting[i]
		if record.Batch <= latest || record.Purpose != "verification" {
			continue
		}
		found := 0
		valid := record.Outcome == "known"
		for _, call := range history {
			if call.Batch != record.Batch {
				continue
			}
			if _, ok := record.Calls[call.ToolCallId]; !ok {
				continue
			}
			found++
			var out struct {
				ExitCode  *int   `json:"exit_code"`
				State     string `json:"state"`
				ErrorKind string `json:"error_kind"`
			}
			if call.Status != filemcp.ToolCallSuceessStatus || !isCommandProgressTool(call.ToolName) || !commandResultPayload(call, &out) || out.ExitCode == nil || *out.ExitCode != 0 || out.State == "running" || out.ErrorKind != "" {
				valid = false
			}
		}
		return valid && found > 0 && found == len(record.Calls)
	}
	return false
}

// registerBatch 在工具执行前按批次用途结算探索额度。被拒批次没有执行记录可供退款。
func (e *explorationState) registerBatch(batch int, purpose string, calls map[string]string) bool {
	if e.Phase == phaseImplement && purpose == "investigation" && e.TaskType != "analysis" {
		if e.Used >= e.allowance() {
			// 策略拒绝不是工具失败：不登记执行或预扣探索，直接消耗业务轮窗口，不能退款。
			e.ModificationLeft--
			e.Rejection = fmt.Sprintf("Operation rejected: exploration allowance exhausted; batch was not executed. Apply a target modification using existing evidence, submit a standalone exploration extend decision with gap, blocking_reason, investigation and exit_condition, or choose unable. modification_left=%d", e.ModificationLeft)
			return false
		}
		e.Used++
	}
	e.Rejection = "" // 已接受纠正操作，避免后续模型继续收到过期的拒绝及窗口余量。
	e.Accounting = append(e.Accounting, explorationAccounting{Batch: batch, Handoff: len(e.Handoffs), Purpose: purpose, Calls: calls, Charged: purpose == "investigation" && e.TaskType != "analysis", Outcome: "pending"})
	if len(e.Accounting) > 256 {
		e.Accounting = e.Accounting[len(e.Accounting)-256:]
	}
	return true
}

// 批次登记成功后才扣修改窗口；扣费标记和余额在下一检查点一起保存。
func (e *explorationState) chargeModificationBatch() bool {
	if e.Phase != phaseImplement {
		return false
	}
	e.ModificationLeft--
	e.Accounting[len(e.Accounting)-1].ModificationCharged = true
	return true
}

// admitExplorationBatch 先持久化批次记账，再单独持久化实施窗口扣减。
func (r *BaseReactProcess[T]) admitExplorationBatch(durable *state.DurableState, budget *executionProgress, toolParams []*sdk.CallToolParams, batch int, batchPurpose string, batchCalls map[string]string) (admitted bool, stopReason string, err error) {
	e := budget.Exploration
	if !e.registerBatch(batch, batchPurpose, batchCalls) {
		log.Printf("[INFO] [exploration budget] instance=%s purpose=%s admission=denied exploration=%d/%d modification_left=%d", durable.ExecutionID, batchPurpose, e.Used, e.allowance(), e.ModificationLeft)
		r.publishRejectedToolBatch(durable.RunId, toolParams, e.Rejection)
		if err := saveDecisionProgress(durable, budget, r.checkpoint); err != nil {
			return false, "", err
		}
		log.Printf("[INFO] [exploration budget] instance=%s operation=rejected correction_allowed=%t modification_left=%d", durable.ExecutionID, e.ModificationLeft > 0, e.ModificationLeft)
		if reason := e.stopReason(); reason != "" {
			return false, reason + "; exploration allowance exhausted", nil
		}
		return false, "", nil
	}
	if err := saveDecisionProgress(durable, budget, r.checkpoint); err != nil {
		return false, "", err
	}
	if e.chargeModificationBatch() {
		if err := saveDecisionProgress(durable, budget, r.checkpoint); err != nil {
			return false, "", err
		}
	}
	log.Printf("[INFO] [exploration budget] instance=%s purpose=%s admission=allowed exploration=%d/%d modification_left=%d", durable.ExecutionID, batchPurpose, e.Used, e.allowance(), e.ModificationLeft)
	return true, "", nil
}

// checkpointExplorationBatch 将最后一个调用的结果与批次快照一起落盘，便于恢复时补做确认。
func (r *BaseReactProcess[T]) checkpointExplorationBatch(durable *state.DurableState, budget *executionProgress) error {
	e := budget.Exploration
	record := &e.Accounting[len(e.Accounting)-1]
	var completed []*state.ToolResult
	for _, call := range durable.ToolExecutions {
		if call.Batch != record.Batch {
			continue
		}
		if call.Status == "" {
			return nil
		}
		completed = append(completed, call)
	}
	if len(completed) == 0 {
		return nil
	}
	e.captureSessionBaseline(completed, record)
	if record.Before != nil {
		record.After = snapshotRecordedTargets(r.toolConfig.Workspace, record.Before)
	}
	e.settleFailedInvestigation(record, completed)
	record.updateOutcome(completed)
	return saveDecisionProgress(durable, budget, nil)
}

// settleExplorationBatch 确认批次结果和阶段变化，再提交已结算的预算。
func (r *BaseReactProcess[T]) settleExplorationBatch(durable *state.DurableState, budget *executionProgress, completed []*state.ToolResult, modificationBefore map[string]targetSnapshot) error {
	e := budget.Exploration
	record := &e.Accounting[len(e.Accounting)-1]
	e.recordCommandSessions(completed, record.Calls, modificationBefore, record.Batch)
	if modificationBefore != nil && record.After == nil {
		record.After = snapshotRecordedTargets(r.toolConfig.Workspace, record.Before)
	}
	e.captureSessionBaseline(completed, record)
	e.settleFailedInvestigation(record, completed)
	record.updateOutcome(completed)
	if record.hasVerificationCommand(completed) {
		// 测试与仍在运行的修改进程可能重叠，旧启动基准不再能区分变更来源。
		e.SessionBaselines = nil
	}
	if modificationBefore != nil && record.Outcome == "known" {
		if modificationBatchCandidate(completed) {
			e.confirmModification(modificationBefore, record.After, record.Batch)
		}
		e.confirmSessionModification(completed, record.After, record)
	}
	log.Printf("[INFO] [exploration budget] instance=%s exploration=%d/%d batch=%d since_batch=%d purpose=%s calls=%v outcome=%s confirmed=%t evidence=%v", durable.ExecutionID, e.Used, e.allowance(), record.Batch, record.SinceBatch, record.Purpose, record.Calls, record.Outcome, e.ModificationConfirmed, e.ModificationEvidence)
	record.Applied = true
	return saveDecisionProgress(durable, budget, r.checkpoint)
}
