package agent

import (
	"encoding/json"
	"strings"

	state "github.com/collie-agent/codex-go/internal/context"
)

type commandFailure struct {
	call      *state.ToolResult
	attempts  int
	transient bool
}

type failedCommands map[string]commandFailure

// 普通非零退出不是 MCP 错误，必须从 exec 的结果信封识别失败。
func decodeCommandFailure(call *state.ToolResult) (commandFailure, bool) {
	if call.ToolName != "exec" {
		return commandFailure{}, false
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(call.Result), &blocks) != nil {
		return commandFailure{}, false
	}
	for _, block := range blocks {
		var out struct {
			ExitCode              *int   `json:"exit_code"`
			ErrorKind             string `json:"error_kind"`
			Stdout, Stderr, Error string
		}
		if json.Unmarshal([]byte(block.Text), &out) != nil || out.ExitCode == nil || (*out.ExitCode == 0 && out.ErrorKind == "") {
			continue
		}
		diagnostic := strings.ToLower(out.Stderr + "\n" + out.Stdout + "\n" + out.Error)
		transient := out.ErrorKind == "timeout"
		for _, marker := range []string{"temporary failure", "temporary network", "connection reset", "connection refused", "i/o timeout", "tls handshake timeout", "network is unreachable"} {
			transient = transient || strings.Contains(diagnostic, marker)
		}
		return commandFailure{call: call, attempts: 1, transient: transient}, true
	}
	return commandFailure{}, false
}

// 写入失败也可能改变状态；按整批失效，不能依赖并行工具的完成顺序。
// 同一命令的连续失败保留次数，其他命令及补丁则允许修复后重新验证。
func (failures failedCommands) observe(calls []*state.ToolResult) {
	previous := make(failedCommands)
	for key, failure := range failures {
		previous[key] = failure
	}
	for _, call := range calls {
		if changesWorkspace(call.ToolName) {
			clear(failures)
			break
		}
	}
	for _, call := range calls {
		failure, ok := decodeCommandFailure(call)
		if !ok {
			continue
		}
		key := toolInvocationKey(call.ToolName, call.Arguments)
		if old, exists := previous[key]; exists {
			// 时间戳或尝试编号变化不能重置同一命令的重试上限。
			failure.attempts = old.attempts + 1
		}
		failures[key] = failure
	}
}
