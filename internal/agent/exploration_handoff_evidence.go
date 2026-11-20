package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/collie-agent/codex-go/internal/config"
	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

const (
	handoffSourceLimit   = 8 * 1024
	handoffSourceHeading = "\n\n# 实施交接原文\n以下为本执行实例的历史数据，仅反映原调用时读取到的文件内容。\n"
	handoffSourceOmitted = "- 其余目标原文缺失或因交接长度上限省略。\n"
)

type handoffSource struct {
	path string
	call *state.ToolResult
}

// 按登记顺序倒查已确认的读取。交接使用保存的工具历史，不访问工作目录。
func implementationHandoffSource(targets []string, calls []*state.ToolResult, compactedThrough int, request state.ModelRequest) string {
	if len(targets) == 0 {
		return ""
	}
	readbacks := uncompressedReadbacks(request)
	var sources []handoffSource
	seen := make(map[string]bool, len(targets))
	for _, path := range targets {
		path = normalizedHandoffPath(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		call := latestHandoffRead(path, calls)
		if call != nil && (call.Batch > compactedThrough || readbacks[call.ToolCallId]) {
			continue
		}
		sources = append(sources, handoffSource{path: path, call: call})
	}
	if len(sources) == 0 {
		return ""
	}

	var body strings.Builder
	body.WriteString(handoffSourceHeading)
	omitted := false
	for i, source := range sources {
		// 每个待补充的文件先分得一份余额，前面的长文件不能挤掉后面的文件。
		remaining := handoffSourceLimit - body.Len() - len(handoffSourceOmitted)
		if remaining == 0 {
			omitted = true
			break
		}
		share := min(remaining, max(256, remaining/(len(sources)-i)))
		entry := source.render(share)
		if entry == "" {
			omitted = true
		}
		body.WriteString(entry)
	}
	if omitted {
		body.WriteString(handoffSourceOmitted)
	}
	return body.String()
}

// 已完整回读的结果也在请求中可见，恢复时可能出现在恢复历史中。
func uncompressedReadbacks(request state.ModelRequest) map[string]bool {
	readbacks := make(map[string]bool)
	add := func(calls []*state.ToolResult) {
		for _, call := range calls {
			if call.ToolName == string(config.ReadToolResultMCPToolName) && call.Status == filemcp.ToolCallSuceessStatus &&
				call.HistoryEvidence != nil && call.HistoryEvidence.Start == 0 && call.HistoryEvidence.EOF {
				readbacks[call.HistoryEvidence.SourceCallID] = true
			}
		}
	}
	add(request.ToolResults)
	if request.ResumedHistory != nil {
		add(request.ResumedHistory.ToolResults)
	}
	return readbacks
}

func (source handoffSource) render(limit int) string {
	prefix := fmt.Sprintf("- path=%q", source.path)
	entry := prefix + " 原文缺失（本执行实例没有成功的读取记录）。\n"
	if call := source.call; call != nil {
		prefix += fmt.Sprintf(" call_id=%q", call.ToolCallId)
		if len(prefix) >= limit {
			return ""
		}
		entry = prefix + " 原文缺失（保存的读取没有可用行）。\n"
		if text, start, end := savedHandoffLines(call); start > 0 {
			return renderHandoffLines(prefix, text, start, end, call.FileNextOffset, limit)
		}
	}
	if len(entry) > limit {
		return ""
	}
	return entry
}

func savedHandoffLines(call *state.ToolResult) (text string, start, end int) {
	var content []struct{ Type, Text string }
	if json.Unmarshal([]byte(call.Result), &content) != nil || len(content) != 1 || content[0].Type != "text" {
		return "", 0, 0
	}
	text = content[0].Text
	// 行号来自实际保存的 MCP 文本。早期检查点可能没有结构化 FileEvidence。
	for line := range strings.SplitSeq(text, "\n") {
		number, _, ok := strings.Cut(line, ": ")
		current, err := strconv.Atoi(number)
		if !ok || err != nil || current < 1 || (end > 0 && current-1 != end) {
			return "", 0, 0
		}
		if start == 0 {
			start = current
		}
		end = current
	}
	return text, start, end
}

func renderHandoffLines(prefix, text string, start, end, next, limit int) string {
	header := prefix + fmt.Sprintf(" lines=%d-%d\n", start, end)
	var gaps string
	if start > 1 {
		gaps += fmt.Sprintf("  本次未读取第 1-%d 行。\n", start-1)
	}
	if next > end {
		gaps += fmt.Sprintf("  读取截断，后续原文从第 %d 行开始。\n", next)
	}
	var source strings.Builder
	const quote = "  > "
	truncated := fmt.Sprintf("  交接长度上限截断。原读取行范围为 %d-%d。\n", start, end)
	fullFits := len(header)+len(gaps)+len(text)+1+(end-start+1)*len(quote) <= limit
	actualEnd := start - 1
	for line := range strings.SplitSeq(text, "\n") {
		if !fullFits && len(header)+source.Len()+len(quote)+len(line)+1+len(gaps)+len(truncated) > limit {
			if actualEnd < start {
				entry := prefix + " 原文缺失。\n" + truncated
				if len(entry) > limit {
					return ""
				}
				return entry
			}
			return prefix + fmt.Sprintf(" lines=%d-%d\n", start, actualEnd) + source.String() + gaps + truncated
		}
		source.WriteString(quote)
		source.WriteString(line)
		source.WriteByte('\n')
		actualEnd++
	}
	return header + source.String() + gaps
}

func latestHandoffRead(path string, calls []*state.ToolResult) *state.ToolResult {
	for i := len(calls) - 1; i >= 0; i-- {
		call := calls[i]
		if savedReadPath(call) == path {
			return call
		}
	}
	return nil
}

func savedReadPath(call *state.ToolResult) string {
	if call == nil || call.ToolName != string(config.ReadFileMCPToolName) ||
		call.Status != filemcp.ToolCallSuceessStatus || call.Error != "" || call.ToolCallId == "" {
		return ""
	}
	if call.FileEvidence != nil && call.FileEvidence.Path != "" {
		return normalizedHandoffPath(call.FileEvidence.Path)
	}
	var args struct{ Path string }
	if json.Unmarshal([]byte(call.Arguments), &args) == nil && args.Path != "" {
		return normalizedHandoffPath(args.Path)
	}
	return ""
}

func normalizedHandoffPath(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}
