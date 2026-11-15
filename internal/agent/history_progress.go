package agent

import (
	"encoding/json"
	"strconv"

	state "github.com/collie-agent/codex-go/internal/context"
)

// 历史结果不可变；按源调用和压缩代次记录实际字符覆盖，避免新回读ID伪装新证据。
type historyProgressCoverage map[string]*fileCoverage

func (coverage historyProgressCoverage) add(e *state.HistoryReadEvidence, generation int) bool {
	if e == nil || e.SourceCallID == "" || e.Start < 0 || e.End < e.Start {
		return true
	}
	key := e.SourceCallID + "\x00" + strconv.Itoa(generation)
	ranges, exists := coverage[key]
	if !exists {
		ranges = &fileCoverage{}
		coverage[key] = ranges
	}
	// EOF以第一次观察为证据；无内容的重复分页不能持续刷新停滞窗口。
	if e.Start == e.End {
		return !exists
	}
	return ranges.add(e.Start, e.End)
}

func historyCompactionGeneration(durable *state.DurableState) int {
	if durable == nil {
		return 0
	}
	var compact ExecutionCompaction
	if json.Unmarshal(durable.ExecutionCompaction, &compact) == nil && compact.ExecutionID == durable.ExecutionID {
		return compact.ThroughBatch
	}
	return 0
}

func historyEvidenceFromResult(value any) *state.HistoryReadEvidence {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var evidence state.HistoryReadEvidence
	if json.Unmarshal(data, &evidence) != nil || evidence.SourceCallID == "" {
		return nil
	}
	return &evidence
}
