package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

// FileProgressPolicy bounds recovery within the existing overall turn budget.
// Zero values select defaults: four stalled batches, a four-batch recovery
// window, and one recovery. Configure before Run.
type FileProgressPolicy struct {
	StalledBatches int
	RecoveryWindow int
	MaxRecoveries  int
}

func (r *BaseReactProcess[T]) SetFileProgressPolicy(policy FileProgressPolicy) {
	r.fileProgressPolicy = policy
}

// BatchEvidence is assembled synchronously after all tool results arrive.
// Cached responses are observations too, but never represent a fresh read.
type BatchEvidence struct {
	Completed []*state.ToolResult
	Cached    []*state.ToolResult
}

type lineInterval struct{ start, end int }
type fileCoverage struct{ ranges []lineInterval }
type fileProgressGuard struct {
	policy          FileProgressPolicy
	coverage        map[string]*fileCoverage //read_file
	historyCoverage historyProgressCoverage  //read_tool_result
	searchCoverage  searchProgressCoverage   //grep,glob
	commandCoverage map[string]bool          //exec系列
	stale           int                      //停滞批次计数。有可观察结果但没有新证据时递增；出现新证据时归零；没有可观察结果时保持不变
	recoveries      int                      //已启动的恢复次数
	recoveryLeft    int                      //当前恢复窗口剩余的停滞批次数
	recoveryActive  bool                     //是否正在恢复窗口中
}

func newFileProgressGuard(policy FileProgressPolicy) *fileProgressGuard {
	if policy.StalledBatches <= 0 {
		policy.StalledBatches = 4
	}
	if policy.RecoveryWindow <= 0 {
		policy.RecoveryWindow = 4
	}
	if policy.MaxRecoveries <= 0 {
		policy.MaxRecoveries = 1
	}
	return &fileProgressGuard{
		policy: policy, coverage: make(map[string]*fileCoverage),
		historyCoverage: make(historyProgressCoverage),
		searchCoverage:  make(searchProgressCoverage),
		commandCoverage: make(map[string]bool),
	}
}

func (g *fileProgressGuard) add(e *filemcp.FileEvidence) bool {
	if e == nil || e.Path == "" || e.Version == "" {
		return true
	} // 未知版本不能证明停滞。
	key := e.Path + "\x00" + e.Version
	coverage, exists := g.coverage[key]
	if !exists {
		coverage = &fileCoverage{}
		g.coverage[key] = coverage
	}
	if e.StartLine == 0 && e.EndLine == 0 {
		return !exists
	} // 首次 EOF 是证据，反复 EOF 不是。
	if e.StartLine <= 0 || e.EndLine < e.StartLine {
		return true
	}
	// Convert inclusive line numbers to zero-based half-open intervals.
	return coverage.add(e.StartLine-1, e.EndLine)
}

// add records a half-open interval and reports whether it includes unseen content.
// Both file reads and history pages use this algorithm; empty/EOF evidence is
// handled by their callers because it depends on the source identity.
func (c *fileCoverage) add(start, end int) bool {
	fresh := true
	for _, r := range c.ranges {
		if r.start <= start && r.end >= end {
			fresh = false
		}
	}
	merged := make([]lineInterval, 0, len(c.ranges)+1)
	for _, r := range c.ranges {
		if r.end < start || r.start > end {
			merged = append(merged, r)
			continue
		}
		start = min(start, r.start)
		end = max(end, r.end)
	}
	c.ranges = append(merged, lineInterval{start, end})
	return fresh
}

func (g *fileProgressGuard) observe(batch BatchEvidence, durable *state.DurableState, remaining int) (correction string, err error) {
	// 进度诊断始终输出；defer 保证正常、停滞和预算耗尽等返回路径都有输入输出。
	input, _ := json.Marshal(map[string]any{
		"batch": batch, "goal": durable.Goal, "completion_criteria": durable.CompletionCriteria,
		"current_step": durable.CurrentStep(), "remaining": remaining,
		"stale": g.stale, "recoveries": g.recoveries,
		"recovery_active": g.recoveryActive, "recovery_left": g.recoveryLeft,
	})
	log.Printf("[INFO] task %s: [react] observe progress input=%s", durable.RunId, input)
	defer func() {
		errorText := ""
		if err != nil {
			errorText = err.Error()
		}
		output, _ := json.Marshal(map[string]any{
			"correction_triggered": correction != "", "correction": correction, "error": errorText,
			"stale": g.stale, "recoveries": g.recoveries,
			"recovery_active": g.recoveryActive, "recovery_left": g.recoveryLeft,
		})
		log.Printf("[INFO] task %s: [react] observe progress output=%s", durable.RunId, output)
	}()
	hasObservation, newEvidence := false, false
	var references []string
	for _, call := range batch.Completed {
		if call == nil {
			continue
		}
		reference := fmt.Sprintf("call_id=%s", call.ToolCallId)
		fresh := false
		switch config.McpToolName(call.ToolName) {
		case config.ExecStartMCPToolName, config.ExecPollMCPToolName, config.ExecStopMCPToolName:
			fresh = g.observeSession(call)
			reference += fmt.Sprintf(" tool=%s arguments=%s result=%s", call.ToolName, call.Arguments, call.Result[:min(len(call.Result), 2000)])
		case config.ExecMCPToolName:
			fresh = g.observeCommand(call)
			reference += fmt.Sprintf(" command=%s result=%s", call.Arguments, call.Result[:min(len(call.Result), 2000)])
		case config.ReadFileMCPToolName:
			fresh = g.add(call.FileEvidence)
			if e := call.FileEvidence; e != nil {
				reference += fmt.Sprintf(" path=%s version=%s lines=%d-%d", e.Path, e.Version, e.StartLine, e.EndLine)
			}
		case config.ReadToolResultMCPToolName:
			fresh = g.historyCoverage.add(call.HistoryEvidence, historyCompactionGeneration(durable))
			if e := call.HistoryEvidence; e != nil {
				// The source ID identifies evidence; a new paging call ID does not.
				reference += fmt.Sprintf(" source call_id=%s characters=%d-%d eof=%t", e.SourceCallID, e.Start, e.End, e.EOF)
			}
		case config.GrepMCPToolName, config.GlobMCPToolName:
			fresh = g.searchCoverage.add(call.SearchEvidence)
			if e := call.SearchEvidence; e != nil {
				reference += fmt.Sprintf(" query=%s version=%s offset=%d positions=%v eof=%t", e.Query, e.Version, e.Offset, e.Positions, e.EOF)
			}
		default:
			// Tools without an evidence contract remain conservatively unknown.
			newEvidence = true
			continue
		}
		hasObservation = true
		references = append(references, reference)
		if fresh || (call.Status != filemcp.ToolCallSuceessStatus && !isCommandProgressTool(call.ToolName)) {
			newEvidence = true
		}
	}
	for _, call := range batch.Cached {
		if call == nil {
			continue
		}
		switch config.McpToolName(call.ToolName) {
		case config.ExecMCPToolName, config.ExecStartMCPToolName, config.ExecPollMCPToolName, config.ExecStopMCPToolName, config.ReadFileMCPToolName, config.ReadToolResultMCPToolName, config.GrepMCPToolName, config.GlobMCPToolName:
			hasObservation = true
			references = append(references, fmt.Sprintf("cached call_id=%s %s", call.ToolCallId, call.Arguments))
		}
	}
	if newEvidence {
		g.stale = 0
		g.recoveryActive = false
		return "", nil
	}
	if !hasObservation {
		return "", nil
	}
	g.stale++
	if g.recoveryActive {
		g.recoveryLeft--
		if g.recoveryLeft <= 0 {
			return "", fmt.Errorf("file investigation stalled: recovery window exhausted; no new investigation evidence; %s", strings.Join(references, "; "))
		}
		return "", nil
	}
	if g.stale < g.policy.StalledBatches {
		return "", nil
	}
	if g.recoveries >= g.policy.MaxRecoveries || remaining <= 0 {
		return "", fmt.Errorf("file investigation stalled: recovery budget exhausted; no new investigation evidence; %s", strings.Join(references, "; "))
	}
	g.recoveries++
	g.recoveryActive = true
	// 恢复不能超出剩余总预算，确保停滞以明确原因结束。
	g.recoveryLeft = min(g.policy.RecoveryWindow, remaining)
	criteria := strings.Join(durable.CompletionCriteria, "; ")
	goal := durable.Goal
	if step := durable.CurrentStep(); step != nil {
		goal += "; current step: " + step.Intent
		criteria = step.CompletionCriteria
	}
	// Include original evidence references, not just the latest repeated page.
	for _, call := range durable.ToolExecutions {
		if call.FileEvidence != nil && call.ToolName == string(config.ReadFileMCPToolName) {
			references = append(references, fmt.Sprintf("source call_id=%s path=%s", call.ToolCallId, call.FileEvidence.Path))
			break
		}
	}
	return fmt.Sprintf("File investigation recovery (%d/%d): %d batches provided no new investigation evidence. Goal: %s. Completion conditions (not established as satisfied by these reads): %s. Repeated evidence: %s. Remaining budget: %d turns remaining; recovery window: %d batches. First distinguish whether remaining work requires investigation, modification or verification. If existing evidence is sufficient to determine the required change, modify immediately and verify the result; do not reread already covered, unchanged content. A gap in a historical summary that tool results have since resolved is not a reason for further investigation. If essential information is missing, identify the specific gap and its completion criterion, then make a targeted read; if blocked, report the reason, changes and verification results. New evidence alone does not prove completion; do not declare success without meeting the conditions.", g.recoveries, g.policy.MaxRecoveries, g.stale, goal, criteria, strings.Join(references, "; "), remaining, g.recoveryLeft), nil
}

// readFileEvidence accepts metadata only from the successful tool response.
func readFileEvidence(value any) (*filemcp.FileEvidence, int) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, 0
	}
	var result struct {
		Evidence   *filemcp.FileEvidence `json:"evidence"`
		NextOffset int                   `json:"next_offset"`
	}
	if json.Unmarshal(data, &result) != nil {
		return nil, 0
	}
	return result.Evidence, result.NextOffset
}
