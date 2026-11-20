package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	state "github.com/collie-agent/codex-go/internal/context"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type rejectedRead struct {
	path    string
	pattern *regexp.Regexp
	line    int
}

// 只解析路径和查询语义明确的形式；未知命令不据此猜测历史调用。
func rejectedReads(param *sdk.CallToolParams) []rejectedRead {
	var args struct {
		Path, Pattern, Command string
		Args                   []string
		Offset                 int
		Literal                bool
		IgnoreCase             bool `json:"ignore_case"`
	}
	raw, err := json.Marshal(param.Arguments)
	if err != nil || json.Unmarshal(raw, &args) != nil {
		return nil
	}
	var paths []string
	var pattern *regexp.Regexp
	switch param.Name {
	case "read_file":
		if args.Path != "" {
			return []rejectedRead{{path: normalizedHandoffPath(args.Path), line: max(1, args.Offset)}}
		}
	case "grep":
		if args.Literal {
			args.Pattern = regexp.QuoteMeta(args.Pattern)
		}
		if args.IgnoreCase {
			args.Pattern = "(?i)" + args.Pattern
		}
		pattern, err = regexp.Compile(args.Pattern)
		paths = []string{args.Path}
	case "exec", "exec_start":
		if args.Command != "grep" && args.Command != "rg" {
			return nil
		}
		argv := args.Args
		for len(argv) > 0 && (argv[0] == "-n" || argv[0] == "-E") {
			argv = argv[1:]
		}
		if len(argv) < 2 || strings.HasPrefix(argv[0], "-") {
			return nil
		}
		pattern, err = regexp.Compile(argv[0])
		paths = argv[1:]
	}
	if err != nil {
		return nil
	}
	var reads []rejectedRead
	for _, path := range paths {
		if path == "" || strings.HasPrefix(path, "-") {
			return nil
		}
		reads = append(reads, rejectedRead{path: normalizedHandoffPath(path), pattern: pattern})
	}
	return reads
}

func (q rejectedRead) historicalPage(history []*state.ToolResult) string {
	for i := len(history) - 1; i >= 0; i-- {
		call := history[i]
		if savedReadPath(call) != q.path {
			continue
		}
		text, start, end := savedHandoffLines(call)
		if start == 0 || (q.line > 0 && (q.line < start || q.line > end)) {
			continue
		}
		if q.pattern != nil && !matchesSourceLine(q.pattern, text) {
			continue
		}
		return fmt.Sprintf("%s:%d-%d 可回读 read_tool_result({\"call_id\":%q,\"offset\":0,\"limit\":20000})。", q.path, start, end, call.ToolCallId)
	}
	return ""
}

// read_file 的行号是展示元数据，不能参与源码正则匹配。
func matchesSourceLine(pattern *regexp.Regexp, text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		_, source, _ := strings.Cut(line, ": ")
		if pattern.MatchString(source) {
			return true
		}
	}
	return false
}

func (e *explorationState) rejectedReadHints(params []*sdk.CallToolParams, history []*state.ToolResult, extensionBudget int) string {
	var hints strings.Builder
	seen := make(map[string]bool)
	add := func(q rejectedRead) {
		hint := q.historicalPage(history)
		if hint == "" {
			hint = fmt.Sprintf("%q 缺少与本次请求匹配的历史源码。", q.path)
		}
		if !seen[hint] {
			seen[hint] = true
			hints.WriteString("\n" + hint)
		}
	}
	requested := make(map[string]bool)
	for _, param := range params {
		for _, q := range rejectedReads(param) {
			requested[q.path] = true
			add(q)
		}
	}
	// 交接计划中的未读目标也可能是实施阻塞点，例如尚未读过的测试文件。
	if e.Handoff != nil {
		if len(requested) == 0 {
			hints.WriteString("\n未识别出具体读取范围，可参考交接目标的历史记录：")
		}
		for _, target := range e.Handoff.TargetFiles {
			path := normalizedHandoffPath(target)
			if len(requested) == 0 || (!requested[path] && latestHandoffRead(path, history) == nil) {
				add(rejectedRead{path: path})
			}
		}
	}
	hints.WriteString("\n已有证据足够时直接修改目标。")
	// extensionBudget 已预留下一次独立决策及最终收尾轮，与延期审批的可用额度一致。
	if e.Extensions < e.Policy.MaxExtensions && extensionBudget >= e.Policy.ExtensionTurns {
		hints.WriteString("若缺失源码阻塞修改，单独提交 choice=extend，填写 gap、blocking_reason、investigation（指定文件和所需范围）、exit_condition。")
	} else {
		hints.WriteString("探索延期额度不足。")
	}
	hints.WriteString("无法继续时选择 unable。")
	return hints.String()
}
