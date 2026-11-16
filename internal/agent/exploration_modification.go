package agent

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	state "github.com/collie-agent/codex-go/internal/context"
)

type modificationEvidence struct {
	Handoff int `json:",omitempty"` // 从 1 开始的获准交接编号；旧证据缺失时保持未知。
	Path    string
	Before  string
	After   string
	Batch   int
}
type targetSnapshot struct {
	Known   bool
	Version string
}

func (e *explorationState) beginModification(d *state.ExplorationDecision) error {
	if e.Phase == "implement" && !e.ModificationConfirmed {
		return fmt.Errorf("pending implementation handoff cannot refresh modification window")
	}
	paths := d.TargetFiles
	if len(paths) == 0 {
		path, _, _ := strings.Cut(d.Target, ":")
		paths = []string{strings.TrimSpace(path)}
	}
	for _, path := range paths {
		if path == "" || filepath.IsAbs(path) || filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) {
			return fmt.Errorf("target_files must contain workspace-relative file paths")
		}
	}
	copyDecision := *d
	copyDecision.TargetFiles = append([]string(nil), paths...)
	e.Handoff = &copyDecision
	// 按执行实例保留每次获准交接，后续窗口和证据可追溯到具体修改承诺。
	e.Handoffs = append(e.Handoffs, copyDecision)
	// 新交接不能用旧进程的修改基准证明下一次修改。
	e.SessionBaselines = nil
	e.ModificationLeft = e.Policy.ModificationWindowTurns
	if e.ModificationLeft == 0 {
		e.ModificationLeft = 3
	}
	e.ModificationConfirmed = false
	e.Phase = "implement"
	return nil
}

// 批次前后观察同一目标文件，既有脏内容只作为基准，不能充当本次变更。
func snapshotModificationTargets(workspace string, e *explorationState) map[string]targetSnapshot {
	snapshots := make(map[string]targetSnapshot)
	if e.Handoff == nil {
		return snapshots
	}
	for _, path := range e.Handoff.TargetFiles {
		snapshots[path] = targetSnapshot{}
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return snapshots
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return snapshots
	}
	for _, path := range e.Handoff.TargetFiles {
		full := filepath.Join(root, path)
		relative, err := filepath.Rel(root, full)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		safe := true
		current := root
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if os.IsNotExist(err) {
				break
			}
			if err != nil || info.Mode()&os.ModeSymlink != 0 || (current == full && !info.Mode().IsRegular()) {
				safe = false
				break
			}
		}
		if !safe {
			continue
		}
		// 限制单个目标快照大小，超限和不可读都保留为未知。
		file, err := os.Open(full)
		var content []byte
		if err == nil {
			info, statErr := file.Stat()
			if statErr != nil || !info.Mode().IsRegular() {
				file.Close()
				continue
			}
			content, err = io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
			file.Close()
			if len(content) > 4*1024*1024 {
				continue
			}
		}
		if os.IsNotExist(err) {
			snapshots[path] = targetSnapshot{Known: true, Version: "absent"}
			continue
		}
		if err == nil {
			snapshots[path] = targetSnapshot{Known: true, Version: fmt.Sprintf("sha256:%x", sha256.Sum256(content))}
		}
	}
	return snapshots
}
func (e *explorationState) confirmModification(before, after map[string]targetSnapshot, batch int) {
	for path, prior := range before {
		next := after[path]
		if prior.Known && next.Known && prior.Version != next.Version {
			e.ModificationEvidence = append(e.ModificationEvidence, modificationEvidence{Handoff: len(e.Handoffs), Path: path, Before: prior.Version, After: next.Version, Batch: batch})
			e.ModificationConfirmed = true
		}
	}
	if e.ModificationConfirmed {
		e.Phase = "exploring"
		e.ModificationLeft = 0
	}
}
