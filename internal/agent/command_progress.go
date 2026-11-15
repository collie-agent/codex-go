package agent

import (
	"encoding/json"
	"fmt"

	state "github.com/collie-agent/codex-go/internal/context"
	filemcp "github.com/collie-agent/codex-go/internal/mcp"
)

// Read retained native content, including command identity preserved by summaries.
func commandResultPayload(call *state.ToolResult, output any) bool {
	var content []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(call.Result), &content) != nil {
		return false
	}
	for _, item := range content {
		if json.Unmarshal([]byte(item.Text), output) == nil {
			return true
		}
	}
	return false
}

func (g *fileProgressGuard) observeCommand(call *state.ToolResult) bool {
	var out struct {
		Evidence *filemcp.CommandEvidence `json:"command_evidence"`
	}
	if !commandResultPayload(call, &out) || out.Evidence == nil || out.Evidence.WorkspaceVersion == "" {
		return true
	}
	key := toolInvocationKey(call.ToolName, call.Arguments) + "\x00" + out.Evidence.WorkspaceVersion + "\x00" + out.Evidence.Fingerprint
	fresh := !g.commandCoverage[key]
	g.commandCoverage[key] = true
	return fresh
}

func isCommandProgressTool(name string) bool {
	return name == "exec" || name == "exec_start" || name == "exec_poll" || name == "exec_stop"
}

func (g *fileProgressGuard) observeSession(call *state.ToolResult) bool {
	var out struct {
		SessionID  string `json:"session_id"`
		State      string `json:"state"`
		Cursor     int64  `json:"next_cursor"`
		ElapsedMS  int64  `json:"elapsed_ms"`
		LifetimeMS int64  `json:"lifetime_ms"`
		ErrorKind  string `json:"error_kind"`
	}
	if !commandResultPayload(call, &out) {
		// Unknown/expired IDs are errors, never proof of a live process. Replaying
		// the same error is bounded without automatically restarting the command.
		key := toolInvocationKey(call.ToolName, call.Arguments) + "\x00" + call.Result
		fresh := !g.commandCoverage[key]
		g.commandCoverage[key] = true
		return fresh
	}
	// Waiting is legitimate until the original absolute deadline. Output never
	// extends it. A completed process can still provide new evidence when polled
	// after the deadline; only running or explicitly timed-out sessions expire.
	if out.ErrorKind == "timeout" || (out.State == "running" && out.LifetimeMS > 0 && out.ElapsedMS >= out.LifetimeMS) {
		return false
	}
	if out.SessionID != "" && out.State == "running" {
		return true
	}
	// Exit/status/cursor changes are new evidence once; replaying an exit is not.
	key := fmt.Sprintf("session\x00%s\x00%s\x00%d\x00%s", out.SessionID, out.State, out.Cursor, out.ErrorKind)
	fresh := !g.commandCoverage[key]
	g.commandCoverage[key] = true
	return fresh
}
