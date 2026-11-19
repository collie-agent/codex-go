package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	state "github.com/collie-agent/codex-go/internal/context"
)

type executionProgress struct {
	Exploration          *explorationState
	ExecutionID          string
	UsedTurns            int
	Finalizing           bool //是否已经进入最终收尾阶段。在请求模型生成最终失败总结前设为 true
	StopReason           string
	Correction           string
	Stale                int                        //没有产生新调查证据的累计停滞批次数；出现新证据时归零。没有可观察结果的批次不会增加它
	Recoveries           int                        //已启动过多少次停滞恢复,发现新证据不会重置
	RecoveryLeft         int                        //当前恢复窗口还允许多少个无新证据的批次。恢复期间每遇到一个停滞批次就减一，耗尽则停止。
	RecoveryActive       bool                       //当前是否处于停滞恢复窗口。发现新证据后变为 false。
	Coverage             map[string][]progressRange //已读取的文件内容范围。键为 文件路径 + "\x00" + 文件版本，值为行区间列表；持久化时采用 从 1 开始、两端包含的行号
	HistoryCoverage      map[string][]progressRange
	SearchCoverage       searchProgressCoverage //已观察过的搜索位置记录，供 grep、glob 判断结果是否带来了新证据
	CommandCoverage      map[string]bool        //已观察过的命令证据标识集合，供命令执行及会话观察逻辑识别重复证据
	ObservedThroughBatch int                    //已完成进展分析的最大工具批次号。恢复时，只补分析编号大于它的已落盘批次，处理“工具结果已保存，但进展检查点尚未保存就崩溃”的情况
}

func saveDecisionProgress(durable *state.DurableState, progress *executionProgress, checkpoint func() error) error {
	progress.Correction = progress.Correction[:min(len(progress.Correction), 16*1024)]
	progress.StopReason = progress.StopReason[:min(len(progress.StopReason), 8*1024)]
	encoded, err := json.Marshal(progress)
	// Storage has a fixed upper bound independent of tool output size. Evidence
	// evicted here is unknown on resume; it never grants extra decision budget.
	if err == nil && len(encoded) > 128*1024 {
		progress.Coverage, progress.HistoryCoverage, progress.SearchCoverage, progress.CommandCoverage = nil, nil, nil, nil
		encoded, err = json.Marshal(progress)
	}
	if err != nil {
		return err
	}
	previous := durable.ExecutionProgress
	durable.ExecutionProgress = encoded
	if checkpoint != nil {
		if err := checkpoint(); err != nil {
			durable.ExecutionProgress = previous
			return err
		}
	}
	return nil
}

// progressRange is an explicit persisted representation of the interval index.
type progressRange struct{ Start, End int }

// 将observe progress结果snapshot到executionProgress
func (p *executionProgress) capture(g *fileProgressGuard) {
	p.CommandCoverage = g.commandCoverage
	if len(p.CommandCoverage) > 256 {
		p.CommandCoverage = nil
	}
	p.Stale, p.Recoveries, p.RecoveryLeft, p.RecoveryActive = g.stale, g.recoveries, g.recoveryLeft, g.recoveryActive
	p.HistoryCoverage = make(map[string][]progressRange)
	for key, coverage := range g.historyCoverage {
		for _, span := range coverage.ranges {
			if len(p.HistoryCoverage[key]) < 256 {
				p.HistoryCoverage[key] = append(p.HistoryCoverage[key], progressRange{span.start, span.end})
			}
		}
		if _, ok := p.HistoryCoverage[key]; !ok {
			p.HistoryCoverage[key] = nil
		}
	}
	p.SearchCoverage = make(searchProgressCoverage)
	for key, positions := range g.searchCoverage {
		limited := make(map[string]bool)
		keys := make([]string, 0, len(positions))
		for position := range positions {
			keys = append(keys, position)
		}
		sort.Strings(keys)
		for _, position := range keys[:min(len(keys), 256)] {
			limited[position] = positions[position]
		}
		p.SearchCoverage[key] = limited
	}
	if len(p.HistoryCoverage) > 256 {
		p.HistoryCoverage = nil
	}
	if len(p.SearchCoverage) > 256 {
		p.SearchCoverage = nil
	}
	p.Coverage = make(map[string][]progressRange)
	for key, coverage := range g.coverage {
		for _, span := range coverage.ranges {
			if len(p.Coverage[key]) < 256 {
				// Keep persisted file ranges one-based and inclusive for old checkpoints.
				p.Coverage[key] = append(p.Coverage[key], progressRange{span.start + 1, span.end})
			}
		}
		if _, ok := p.Coverage[key]; !ok {
			p.Coverage[key] = nil
		}
	}
	// Bound persisted evidence. Dropped entries become unknown, never confirmed progress.
	if len(p.Coverage) > 256 {
		p.Coverage = nil
	}
}

func restoreDecisionProgress(durable *state.DurableState, g *fileProgressGuard) (*executionProgress, error) {
	p := &executionProgress{ExecutionID: durable.ExecutionID}
	var counters struct{ UsedTurns *int }
	if len(durable.ExecutionProgress) > 0 {
		if err := json.Unmarshal(durable.ExecutionProgress, p); err != nil {
			return nil, fmt.Errorf("restore execution progress: %w", err)
		}
		if p.ExecutionID != durable.ExecutionID {
			return &executionProgress{ExecutionID: durable.ExecutionID}, nil
		}
		if err := json.Unmarshal(durable.ExecutionProgress, &counters); err != nil {
			return nil, fmt.Errorf("restore decision counter: %w", err)
		}
	}
	if p.UsedTurns < 0 {
		return nil, fmt.Errorf("invalid negative decision budget")
	}
	// 旧进度可能存在但尚无 UsedTurns 字段。已登记批次是已知消耗的下界，
	// 不能因为 JSON 解码得到零值就刷新额度，也不猜测未记录的无工具决策。
	if counters.UsedTurns == nil {
		batches := make(map[int]bool)
		for _, call := range durable.ToolExecutions {
			batches[call.Batch] = true
		}
		p.UsedTurns = len(batches)
	}
	if p.CommandCoverage != nil {
		g.commandCoverage = p.CommandCoverage
	}
	g.stale, g.recoveries, g.recoveryLeft, g.recoveryActive = p.Stale, p.Recoveries, p.RecoveryLeft, p.RecoveryActive
	for key, ranges := range p.Coverage {
		coverage := &fileCoverage{}
		for _, span := range ranges {
			coverage.ranges = append(coverage.ranges, lineInterval{span.Start - 1, span.End})
		}
		g.coverage[key] = coverage
	}
	for key, ranges := range p.HistoryCoverage {
		coverage := &fileCoverage{}
		for _, span := range ranges {
			coverage.ranges = append(coverage.ranges, lineInterval{span.Start, span.End})
		}
		g.historyCoverage[key] = coverage
	}
	if p.SearchCoverage != nil {
		g.searchCoverage = p.SearchCoverage
	}
	return p, nil
}

// finishProgress reserves at most one summary decision before doing any request.
// Tool requests and claimed success from this summary never become execution.
func (r *BaseReactProcess[T]) finishProgress(worker *Worker[T], modelContext state.Contexter, durable *state.DurableState, progress *executionProgress, reason string, progressResult func(*T) *ReactResult) (T, []AgentProcessorType, error) {
	var zero T
	if progress.StopReason != "" {
		reason = progress.StopReason
	}
	fallback := ReactResult{Status: FailedReactStatus, Answer: "Execution stopped: " + reason, Observation: []string{"Unfinished goal: " + durable.Goal}}
	if e := progress.Exploration; e != nil {
		for _, evidence := range e.ModificationEvidence {
			fallback.Observation = append(fallback.Observation, fmt.Sprintf("Confirmed target change: %s handoff=%d batch=%d before=%s after=%s", evidence.Path, evidence.Handoff, evidence.Batch, evidence.Before, evidence.After))
		}
	}
	for _, call := range durable.ToolExecutions[max(0, len(durable.ToolExecutions)-32):] {
		if call.Status == "" {
			continue
		}
		detail := fmt.Sprintf("call_id=%s tool=%s status=%s", call.ToolCallId, call.ToolName, call.Status)
		if e := progress.Exploration; e != nil {
			for _, record := range e.Accounting {
				if record.Batch == call.Batch && record.Calls[call.ToolCallId] == "verification" {
					fallback.Observation = append(fallback.Observation, "Recorded verification: "+detail+" result="+call.Result[:min(len(call.Result), 2000)]+" error="+call.Error[:min(len(call.Error), 2000)])
					break
				}
			}
		}
		if changesWorkspace(call.ToolName) {
			fallback.Observation = append(fallback.Observation, "Recorded action (effects require verification): "+detail)
		}
		if call.ToolName == "exec" {
			fallback.Observation = append(fallback.Observation, "Recorded command: "+detail+" result="+call.Result[:min(len(call.Result), 2000)]+" error="+call.Error[:min(len(call.Error), 2000)])
		}
	}
	if !progress.Finalizing && progress.UsedTurns < r.maxTurn {
		progress.Finalizing, progress.StopReason = true, reason
		progress.UsedTurns++
		if err := saveDecisionProgress(durable, progress, r.checkpoint); err != nil {
			return zero, nil, err
		}
		worker.corrections = append(worker.corrections, "Execution budget closing: "+reason+". Return one final failed result summarizing recorded changes, verification, unfinished work and stop reason. Do not call tools or claim completion.")
		request := modelContext.ModelRequest()
		request.AllowTools = false
		result, _, tools, err := worker.Run(frozenCompactContext{Contexter: modelContext, request: request})
		if err == nil && len(tools) == 0 {
			if value := progressResult(&result); value.Status == FailedReactStatus && strings.TrimSpace(value.Answer) != "" {
				fallback.Answer = value.Answer
				fallback.Observation = append(fallback.Observation, value.Observation...)
			}
		}
	}
	fallback.Observation = append(fallback.Observation, "Stop reason: "+reason, "Unfinished completion conditions: "+strings.Join(durable.CompletionCriteria, "; "))
	if step := durable.CurrentStep(); step != nil {
		fallback.Observation = append(fallback.Observation, "Unfinished step: "+step.Intent+"; conditions: "+step.CompletionCriteria)
	}
	var result T
	*progressResult(&result) = fallback
	encoded, err := json.Marshal(result)
	if err != nil {
		return zero, nil, err
	}
	previous := durable.ExecutionResult
	durable.ExecutionResult = encoded
	if err := saveDecisionProgress(durable, progress, r.checkpoint); err != nil {
		durable.ExecutionResult = previous
		return zero, nil, err
	}
	return result, nil, fmt.Errorf("execution stopped: %s: %w", reason, &BusinessFailureError{Result: fallback})
}
