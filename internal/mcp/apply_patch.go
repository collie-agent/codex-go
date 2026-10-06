package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	mcpconfig "github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	patchInvalid   = 1201
	patchPath      = 1202
	patchConflict  = 1203
	patchDirectory = 1204
	patchSymlink   = 1205
	patchWrite     = 1206
)

type patchHunk struct {
	oldStart, oldCount, newStart, newCount int
	lines                                  []patchLine
}
type patchLine struct {
	op   byte
	text string
}
type filePatch struct {
	oldPath, newPath string
	hunks            []patchHunk
}
type applyPatchArgs struct {
	Patch string `json:"patch" jsonschema:"non-empty unified diff with ---/+++ file headers and @@ hunks; use /dev/null for creation or deletion; not the *** Begin Patch format"`
}
type applyPatchResult struct {
	Affected  []string `json:"affected"`
	Completed []string `json:"completed"`
	Failed    []string `json:"failed"`
}
type pendingPatch struct {
	content *string
	rel     string
}

var hunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// registerApplyPatchTool registers apply_patch against the shared workspace.
func registerApplyPatchTool(server *sdk.Server, workspace string, timeout time.Duration) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	sdk.AddTool(server, &sdk.Tool{Name: string(mcpconfig.ApplyPatchMCPToolName), Description: `Create, modify, or delete workspace UTF-8 text files using a non-empty unified diff string. Use --- old/path and +++ new/path headers followed by @@ -oldStart,oldCount +newStart,newCount @@ hunks with space-prefixed context, - removals, and + additions; this tool does not accept the *** Begin Patch format. Header paths are workspace-relative; optional a/ and b/ prefixes are stripped. Use /dev/null as the old path to create a new file or as the new path to delete a file (the deletion hunks must remove all contents). Renames, binary/NUL-containing files, directory targets, absolute paths other than the /dev/null marker, paths outside the workspace, and symlink path components are rejected. Parent directories for new files are created automatically. Hunk matching searches forward through the file and can tolerate whitespace and limited punctuation differences; line numbers are not strict match positions. All changes are prepared before writes begin, but a multi-file patch is not transactional: a write failure may leave earlier files changed. Returns workspace-relative affected paths; on success completed and failed are empty. On error isError is true and the text explains the failure; completed records writes already made and failed identifies the failing write when applicable. affected alone does not prove a file was written.`},
		func(ctx context.Context, _ *sdk.CallToolRequest, args applyPatchArgs) (*sdk.CallToolResult, applyPatchResult, error) {
			toolCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			result, err := ApplyPatch(toolCtx, workspace, args.Patch)
			response, result, responseErr := patchToolResponse(result, err)
			return response, result, responseErr
		})
}

func ApplyPatch(ctx context.Context, workspace, text string) (applyPatchResult, error) {
	if strings.TrimSpace(text) == "" {
		return applyPatchResult{}, patchErr(patchInvalid, "patch must be a non-empty string")
	}
	files, err := parsePatch(text)
	if err != nil {
		return applyPatchResult{}, patchErr(patchInvalid, err.Error())
	}
	root, err := resolveWorkspace(workspace)
	if err != nil {
		return applyPatchResult{}, patchErr(patchPath, err.Error())
	}
	if len(files) == 0 {
		return applyPatchResult{}, patchErr(patchInvalid, "patch contains no file changes")
	}
	type prepared struct {
		fp       filePatch
		old, new string
	}
	preparedFiles := make([]prepared, 0, len(files))
	for _, fp := range files {
		if fp.oldPath != "/dev/null" && fp.newPath != "/dev/null" && fp.oldPath != fp.newPath {
			return applyPatchResult{}, patchErr(patchInvalid, "path change is not a valid unified diff")
		}
		p := prepared{fp: fp}
		if fp.oldPath != "/dev/null" {
			p.old, err = patchPathResolve(root, fp.oldPath)
			if err != nil {
				return applyPatchResult{}, err
			}
			info, e := os.Lstat(p.old)
			if e != nil {
				if os.IsNotExist(e) {
					return applyPatchResult{}, patchErr(patchConflict, "file to modify does not exist: "+fp.oldPath)
				}
				return applyPatchResult{}, patchErr(patchConflict, e.Error())
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return applyPatchResult{}, patchErr(patchSymlink, "symbolic link target not patchable: "+fp.oldPath)
			}
			if info.IsDir() {
				return applyPatchResult{}, patchErr(patchDirectory, "directory target not patchable: "+fp.oldPath)
			}
			if !info.Mode().IsRegular() {
				return applyPatchResult{}, patchErr(patchConflict, "file target is not regular: "+fp.oldPath)
			}
		}
		if fp.newPath != "/dev/null" {
			p.new, err = patchPathResolve(root, fp.newPath)
			if err != nil {
				return applyPatchResult{}, err
			}
			if fp.oldPath == "/dev/null" {
				if _, e := os.Lstat(p.new); e == nil {
					return applyPatchResult{}, patchErr(patchConflict, "target already exists: "+fp.newPath)
				} else if !os.IsNotExist(e) {
					return applyPatchResult{}, patchErr(patchConflict, e.Error())
				}
			}
		}
		preparedFiles = append(preparedFiles, p)
	}
	pending := map[string]pendingPatch{}
	affected := []string{}
	for _, p := range preparedFiles {
		if err := ctx.Err(); err != nil {
			return applyPatchResult{Affected: affected}, patchErr(patchWrite, err.Error())
		}
		var content string
		if p.fp.oldPath != "/dev/null" {
			if v, ok := pending[p.old]; ok {
				if v.content == nil {
					return applyPatchResult{Affected: affected}, patchErr(patchConflict, "cannot modify a deleted file: "+p.fp.oldPath)
				}
				content = *v.content
			} else {
				b, e := os.ReadFile(p.old)
				if e != nil {
					return applyPatchResult{Affected: affected}, patchErr(patchConflict, e.Error())
				}
				if bytesInvalid(b) {
					return applyPatchResult{Affected: affected}, patchErr(patchConflict, "file is not valid UTF-8 text: "+p.fp.oldPath)
				}
				content = string(b)
			}
		}
		lines := splitKeepends(content)
		cursor, delta := 0, 0
		for _, h := range p.fp.hunks {
			var e error
			lines, cursor, delta, e = applyHunk(lines, h, cursor, delta)
			if e != nil {
				return applyPatchResult{Affected: affected}, patchErr(patchConflict, "patch does not apply to "+p.fp.oldPath+": "+e.Error())
			}
		}
		if p.fp.newPath == "/dev/null" {
			if len(lines) != 0 {
				return applyPatchResult{Affected: affected}, patchErr(patchConflict, "deletion patch does not fully remove "+p.fp.oldPath+": "+fmt.Sprintf("%q", strings.Join(lines, "")))
			}
			pending[p.old] = pendingPatch{rel: filepath.ToSlash(p.fp.oldPath)}
			affected = append(affected, filepath.ToSlash(p.fp.oldPath))
		} else {
			out := strings.Join(lines, "")
			if bytesInvalid([]byte(out)) {
				return applyPatchResult{Affected: affected}, patchErr(patchInvalid, "patched content is not valid UTF-8 text: "+p.fp.newPath)
			}
			pending[p.new] = pendingPatch{content: &out, rel: filepath.ToSlash(p.fp.newPath)}
			affected = append(affected, filepath.ToSlash(p.fp.newPath))
		}
	}
	worstCase := applyPatchResult{Affected: affected, Completed: affected, Failed: affected}
	if _, err := toolResponse(worstCase, patchReportText(worstCase, nil), false); err != nil {
		return applyPatchResult{}, patchErr(patchInvalid, err.Error())
	}
	completed := []string{}
	pendingOrder := make([]string, 0, len(pending))
	seenPending := make(map[string]bool, len(pending))
	for _, p := range preparedFiles {
		key := p.new
		if p.fp.newPath == "/dev/null" {
			key = p.old
		}
		if seenPending[key] {
			continue
		}
		seenPending[key] = true
		pendingOrder = append(pendingOrder, key)
	}
	for _, key := range pendingOrder {
		entry := pending[key]
		// Keep write reporting relative to the workspace, as required by the MCP contract.
		target := filepath.Join(root, filepath.FromSlash(entry.rel))
		content := entry.content
		if err := ctx.Err(); err != nil {
			return applyPatchResult{Affected: affected, Completed: completed, Failed: []string{entry.rel}}, patchErr(patchWrite, err.Error())
		}
		if content == nil {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return applyPatchResult{Affected: affected, Completed: completed, Failed: []string{entry.rel}}, patchErr(patchWrite, err.Error())
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return applyPatchResult{Affected: affected, Completed: completed, Failed: []string{entry.rel}}, patchErr(patchWrite, err.Error())
			}
			if err := atomicWrite(target, []byte(*content)); err != nil {
				return applyPatchResult{Affected: affected, Completed: completed, Failed: []string{entry.rel}}, patchErr(patchWrite, err.Error())
			}
		}
		completed = append(completed, entry.rel)
	}
	return applyPatchResult{Affected: affected}, nil
}

func patchToolResponse(result applyPatchResult, toolErr error) (*sdk.CallToolResult, applyPatchResult, error) {
	if result.Affected == nil {
		result.Affected = []string{}
	}
	if result.Completed == nil {
		result.Completed = []string{}
	}
	if result.Failed == nil {
		result.Failed = []string{}
	}
	text := patchReportText(result, toolErr)
	response, err := toolResponse(result, text, toolErr != nil)
	if err != nil {
		return nil, applyPatchResult{}, err
	}
	return response, result, nil
}

func patchReportText(result applyPatchResult, toolErr error) string {
	if toolErr == nil {
		return "Applied patch to: " + strings.Join(result.Affected, ", ")
	}
	text := toolErr.Error()
	if len(result.Completed) > 0 {
		text += "\nCompleted: " + strings.Join(result.Completed, ", ")
	}
	if len(result.Failed) > 0 {
		text += "\nFailed: " + strings.Join(result.Failed, ", ")
	}
	return text
}

func bytesInvalid(b []byte) bool { return bytesContainsNUL(b) || !utf8.Valid(b) }
func bytesContainsNUL(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}
func patchErr(code int, msg string) error { return fmt.Errorf("[%d] %s", code, msg) }

func patchPathResolve(root, path string) (string, error) {
	if path == "" {
		return "", patchErr(patchPath, "empty patch path")
	}
	target, _, err := resolveWorkspacePath(root, filepath.FromSlash(path), rejectSymlinks, true)
	if err != nil {
		code := patchPath
		if errors.Is(err, errSymlink) {
			code = patchSymlink
		}
		return "", patchErr(code, err.Error())
	}
	return target, nil
}

func parsePatch(text string) ([]filePatch, error) {
	// A missing transport newline does not describe the file's line ending.
	// Only an explicit no-newline marker can remove a body line's newline.
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	raw := splitKeepends(text)
	if len(raw) > 0 && raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1]
	}
	var out []filePatch
	for i := 0; i < len(raw); {
		if !strings.HasPrefix(raw[i], "--- ") {
			if len(out) > 0 && strings.TrimSpace(raw[i]) != "" && !patchPreamble(raw[i]) {
				return nil, fmt.Errorf("unexpected line outside hunk")
			}
			i++
			continue
		}
		if i+1 >= len(raw) || !strings.HasPrefix(raw[i+1], "+++ ") {
			return nil, fmt.Errorf("expected +++ header")
		}
		fp := filePatch{oldPath: patchHeaderPath(raw[i][4:]), newPath: patchHeaderPath(raw[i+1][4:])}
		i += 2
		for i < len(raw) && strings.HasPrefix(raw[i], "@@") {
			m := hunkRE.FindStringSubmatch(strings.TrimSuffix(raw[i], "\n"))
			if m == nil {
				return nil, fmt.Errorf("malformed hunk header")
			}
			h := patchHunk{oldStart: atoi(m[1]), oldCount: atoiDefault(m[2]), newStart: atoi(m[3]), newCount: atoiDefault(m[4])}
			i++
			oldSeen, newSeen := 0, 0
			for i < len(raw) {
				line := raw[i]
				if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "diff --git ") {
					break
				}
				if strings.HasPrefix(line, "--- ") && i+1 < len(raw) && strings.HasPrefix(raw[i+1], "+++ ") &&
					((i+2 < len(raw) && strings.HasPrefix(raw[i+2], "@@")) ||
						(oldSeen == h.oldCount && newSeen == h.newCount) ||
						(i+2 == len(raw) && (oldSeen+1 != h.oldCount || newSeen+1 != h.newCount))) {
					if oldSeen+1 == h.oldCount && newSeen+1 == h.newCount {
						return nil, fmt.Errorf("ambiguous ---/+++ boundary; add context lines or separate files with diff --git")
					}
					break
				}
				if strings.TrimRight(line, "\r\n") == "\\ No newline at end of file" {
					if len(h.lines) == 0 || strings.HasPrefix(raw[i-1], "\\") {
						return nil, fmt.Errorf("invalid no-newline marker")
					}
					h.lines[len(h.lines)-1].text = strings.TrimRight(h.lines[len(h.lines)-1].text, "\r\n")
					i++
					continue
				}
				if line == "\n" || line == "\r\n" {
					oldSeen++
					newSeen++
					h.lines = append(h.lines, patchLine{' ', line})
					i++
					continue
				}
				if line[0] != ' ' && line[0] != '+' && line[0] != '-' {
					return nil, fmt.Errorf("invalid hunk body line")
				}
				if line[0] != '+' {
					oldSeen++
				}
				if line[0] != '-' {
					newSeen++
				}
				h.lines = append(h.lines, patchLine{line[0], line[1:]})
				i++
			}
			h.oldCount, h.newCount = oldSeen, newSeen
			if len(h.lines) == 0 {
				return nil, fmt.Errorf("empty hunk")
			}
			fp.hunks = append(fp.hunks, h)
		}
		if len(fp.hunks) == 0 {
			return nil, fmt.Errorf("no hunks for file")
		}
		out = append(out, fp)
	}
	return out, nil
}
func patchPreamble(line string) bool {
	for _, prefix := range []string{"diff --git ", "index ", "new file mode ", "deleted file mode ", "old mode ", "new mode "} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func atoi(s string) int { var n int; fmt.Sscanf(s, "%d", &n); return n }
func atoiDefault(s string) int {
	if s == "" {
		return 1
	}
	return atoi(s)
}
func patchHeaderPath(s string) string {
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "/dev/null" {
		return s
	}
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		s = s[2:]
	}
	return s
}
func splitKeepends(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
func applyHunk(lines []string, h patchHunk, cursor, delta int) ([]string, int, int, error) {
	old, neu := []string{}, []string{}
	for _, l := range h.lines {
		if l.op != '+' {
			old = append(old, l.text)
		}
		if l.op != '-' {
			neu = append(neu, l.text)
		}
	}
	if len(old) == 0 {
		pos := h.oldStart + delta
		if pos < cursor || pos > len(lines) {
			return nil, 0, 0, fmt.Errorf("insertion point out of range")
		}
		return append(append(append([]string{}, lines[:pos]...), neu...), lines[pos:]...), pos + len(neu), delta + h.newCount, nil
	}
	for profile := 0; profile < 4; profile++ {
		for i := cursor; i+len(old) <= len(lines); i++ {
			ok := true
			for j := range old {
				if normalizePatch(lines[i+j], profile) != normalizePatch(old[j], profile) {
					ok = false
					break
				}
			}
			if ok {
				out := append(append(append([]string{}, lines[:i]...), neu...), lines[i+len(old):]...)
				return out, i + len(neu), delta + h.newCount - h.oldCount, nil
			}
		}
	}
	return nil, 0, 0, fmt.Errorf("hunk context does not match file content")
}

// Keep this table identical to atom-mcp's limited punctuation equivalents.
var patchPunctuation = strings.NewReplacer(
	"‘", "'", "’", "'", "‚", "'", "‛", "'",
	"“", "\"", "”", "\"", "„", "\"", "‟", "\"",
	"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-",
	"…", "...", "\u00a0", " ",
)

func normalizePatch(s string, profile int) string {
	if profile == 0 {
		return s
	}
	end := ""
	if strings.HasSuffix(s, "\n") {
		s = s[:len(s)-1]
		end = "\n"
	}
	if profile >= 1 {
		s = strings.TrimRight(s, " \t")
	}
	if profile >= 2 {
		s = strings.Trim(s, " \t")
	}
	if profile >= 3 {
		s = patchPunctuation.Replace(s)
	}
	return s + end
}
func atomicWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".apply-patch-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
