package agent

import (
	"encoding/json"

	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

// Search coverage keys include the source version, never cache invalidation epochs.
type searchProgressCoverage map[string]map[string]bool

// 返回是否有新证据
func (coverage searchProgressCoverage) add(e *filemcp.SearchEvidence) bool {
	if e == nil || e.Query == "" || e.Version == "" {
		return true
	}
	key := e.Query + "\x00" + e.Version
	seen, exists := coverage[key]
	if !exists {
		// Bound the runtime index as well as its checkpoint representation. When
		// evidence exceeds capacity, stay conservative rather than infer a stall.
		if len(coverage) >= 256 {
			return true
		}
		seen = make(map[string]bool)
		coverage[key] = seen
	}
	fresh := !exists
	for _, position := range e.Positions {
		if !seen[position] {
			fresh = true
			if len(seen) < 4096 {
				seen[position] = true
			}
		}
	}
	// A populated final page already establishes EOF. Later empty pages must
	// not count that same boundary as new exclusion evidence.
	if e.EOF {
		if !seen["\x00eof"] {
			fresh = true
		}
		seen["\x00eof"] = true
	}
	return fresh
}

func searchEvidenceFromResult(value any) *filemcp.SearchEvidence {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result struct {
		Evidence *filemcp.SearchEvidence `json:"search_evidence"`
	}
	if json.Unmarshal(data, &result) != nil {
		return nil
	}
	return result.Evidence
}
