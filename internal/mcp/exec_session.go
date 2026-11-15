package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/collie-agent/codex-go/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type sessionQuery struct {
	SessionID string `json:"session_id" jsonschema:"exact session_id returned by exec_start on this MCP connection; do not invent an ID or reuse one from an earlier execution"`
	Cursor    int64  `json:"cursor,omitempty" jsonschema:"nonnegative JSON integer byte offset shared by stdout and stderr (default 0); pass the previous next_cursor, never calculate it from text length"`
	WaitMS    int    `json:"wait_ms,omitempty" jsonschema:"exec_poll only: JSON integer wait duration from 0 to 1000 milliseconds (default 0); returns earlier on new output or exit; ignored by exec_stop"`
}

// Timing is server-observed: model latency counts toward elapsed_ms, and
// waited_ms reports only this poll. Neither polling nor output renews the limit.
type sessionResult struct {
	ElapsedMS  int64  `json:"elapsed_ms"`
	WaitedMS   int64  `json:"waited_ms"`
	LifetimeMS int64  `json:"lifetime_ms"`
	SessionID  string `json:"session_id"`
	State      string `json:"state"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	NextCursor int64  `json:"next_cursor"`
	Truncated  bool   `json:"truncated"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorKind  string `json:"error_kind,omitempty"`
}

type execSession struct {
	started  time.Time
	lifetime time.Duration
	mu       sync.Mutex
	id       string
	logs     []sessionLog
	cursor   int64
	done     chan struct{}
	changed  chan struct{}
	cancel   context.CancelFunc
	exited   bool
	result   CommandResult
	err      error
	stopped  bool
}

type sessionLog struct {
	offset int64
	stderr bool
	text   string
}

const sessionLogBytes = 64 * 1024
const maxExecSessions = 32

func (s *execSession) write(stderr bool, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, sessionLog{offset: s.cursor, stderr: stderr, text: string(data)})
	s.cursor += int64(len(data))
	// Keep only the newest bytes. Absolute cursors make lost output explicit
	// while allowing callers to replay a retained page without consuming it.
	cutoff := s.cursor - sessionLogBytes
	for len(s.logs) > 0 && s.logs[0].offset+int64(len(s.logs[0].text)) <= cutoff {
		s.logs[0] = sessionLog{}
		s.logs = s.logs[1:]
	}
	if len(s.logs) > 0 && s.logs[0].offset < cutoff {
		log := &s.logs[0]
		log.text = strings.Clone(log.text[cutoff-log.offset:])
		log.offset = cutoff
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// Each MCP connection belongs to one execution; disconnect revokes every
// process it created. A tool request's cancellation must not own that lifetime.
type execSessionOwner struct {
	mu       sync.Mutex
	sessions map[string]*execSession
	closed   bool
}

func (o *execSessionOwner) close() {
	o.mu.Lock()
	o.closed = true
	sessions := make([]*execSession, 0, len(o.sessions))
	for _, s := range o.sessions {
		s.cancel()
		sessions = append(sessions, s)
	}
	o.mu.Unlock()
	for _, s := range sessions {
		<-s.done
	}
}

// execSessionRegistry owns the per-connection session map shared by all three
// session tools (exec_start, exec_poll, exec_stop). It is intentionally kept
// as a small state object so each tool can be registered independently while
// sharing the same connection lifecycle and cleanup.
type execSessionRegistry struct {
	mu       sync.Mutex
	closed   bool
	owners   map[*sdk.ServerSession]*execSessionOwner
	workspace string
	cfg      Config
	executor *CommandExecutor
	startupErr error
	policy   string
}

// ownerFor returns the session owner for connection, creating one on first use.
// When the connection closes, the owner is shut down and removed.
func (r *execSessionRegistry) ownerFor(connection *sdk.ServerSession) *execSessionOwner {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner := r.owners[connection]; owner != nil {
		return owner
	}
	owner := &execSessionOwner{sessions: make(map[string]*execSession), closed: r.closed}
	r.owners[connection] = owner
	go func() {
		_ = connection.Wait()
		owner.close()
		r.mu.Lock()
		delete(r.owners, connection)
		r.mu.Unlock()
	}()
	return owner
}

// close shuts down every active session owner.
func (r *execSessionRegistry) close() {
	r.mu.Lock()
	r.closed = true
	all := make([]*execSessionOwner, 0, len(r.owners))
	for _, owner := range r.owners {
		all = append(all, owner)
	}
	r.mu.Unlock()
	for _, owner := range all {
		owner.close()
	}
}

// findSession locates a session for the given connection and ID, or returns
// an error that callers can surface directly as a tool error.
func (r *execSessionRegistry) findSession(connection *sdk.ServerSession, sessionID string) (*execSession, error) {
	owner := r.ownerFor(connection)
	owner.mu.Lock()
	s := owner.sessions[sessionID]
	owner.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("unknown process session %q for this execution; sessions do not survive disconnect or resume", sessionID)
	}
	return s, nil
}

// validateSessionCursor checks that cursor is not ahead of produced output.
// The caller must hold s.mu.
func validateSessionCursor(s *execSession, cursor int64) error {
	if cursor > s.cursor {
		return fmt.Errorf("cursor is ahead of output")
	}
	return nil
}

// buildSessionResult assembles the response payload for exec_poll / exec_stop
// by replaying retained logs from cursor onward and attaching exit state.
// The caller must hold s.mu. waitStarted is used to compute waited_ms for the
// current poll; pass time.Now() from right before any optional wait.
func buildSessionResult(s *execSession, cursor int64, waitStarted time.Time) sessionResult {
	out := sessionResult{
		SessionID:  s.id,
		State:      "running",
		NextCursor: s.cursor,
		ElapsedMS:  time.Since(s.started).Milliseconds(),
		WaitedMS:   time.Since(waitStarted).Milliseconds(),
		LifetimeMS: s.lifetime.Milliseconds(),
	}
	out.Truncated = len(s.logs) > 0 && cursor < s.logs[0].offset
	var stdout, stderr strings.Builder
	for _, log := range s.logs {
		start := max(cursor-log.offset, 0)
		if start >= int64(len(log.text)) {
			continue
		}
		if log.stderr {
			stderr.WriteString(log.text[start:])
		} else {
			stdout.WriteString(log.text[start:])
		}
	}
	out.Stdout, out.Stderr = stdout.String(), stderr.String()
	if s.exited {
		out.State = "exited"
		code := s.result.ExitCode
		out.ExitCode = &code
		if s.err != nil {
			switch {
			case errors.Is(s.err, context.DeadlineExceeded):
				out.ErrorKind = "timeout"
			case errors.Is(s.err, context.Canceled):
				if !s.stopped {
					out.ErrorKind = "canceled"
				}
			case errors.Is(s.err, ErrCommandParameter):
				out.ErrorKind = "parameter"
				*out.ExitCode = -1
			case errors.Is(s.err, ErrCommandCleanup):
				out.ErrorKind = "cleanup"
			default:
				out.ErrorKind = "sandbox_startup"
				*out.ExitCode = -1
			}
			if out.ErrorKind != "" {
				out.Error = s.err.Error()
			}
		}
	}
	return out
}

func registerExecSessionTools(server *sdk.Server, workspace string, cfg Config, executor *CommandExecutor, startupErr error) func() {
	network := cfg.ExecNetwork
	if network == "" {
		network = "none"
	}
	policy := fmt.Sprintf("Host-authorized persistent caches are writable and shared with synchronous exec. Allowed entrypoints for this server: %s. Network mode: %s. Authorized host environment names: %s. Session lifetime limit: %s plus cleanup time.", strings.Join(execCommands(cfg.ExecCommands), ", "), network, strings.Join(cfg.ExecEnv, ", "), min(cfg.Timeout, 5*time.Minute))
	registry := &execSessionRegistry{
		owners:     make(map[*sdk.ServerSession]*execSessionOwner),
		workspace:  workspace,
		cfg:        cfg,
		executor:   executor,
		startupErr: startupErr,
		policy:     policy,
	}
	registerExecStartTool(server, registry)
	registerExecPollTool(server, registry)
	registerExecStopTool(server, registry)
	return registry.close
}

func registerExecStartTool(server *sdk.Server, r *execSessionRegistry) {
	sdk.AddTool(server, &sdk.Tool{Name: string(config.ExecStartMCPToolName), InputSchema: toolInputSchema[execToolArgs](), Description: `Start a long-running workspace command so you can inspect its logs and perform verification in later tool calls. Use exec for commands whose final output you need before continuing, such as builds and tests. Example arguments: {"command":"go","args":["run","./cmd/server"]}. Another example: {"command":"npm","args":["run","dev"]}.

command is an exact host-approved entrypoint name, never an executable path or shell string. args is an optional JSON array of strings (default []), never null or a quoted array. Each array element is one literal argument; pipes, redirects, &&, wildcard expansion and environment-variable expansion are not interpreted. Execution starts at the workspace root with stdin at EOF and no PTY. Host-configured runtimes, environment and network policy match exec; arguments cannot override them. Approval does not guarantee a runtime or its dependencies are installed.

Returns session_id, state: "running", empty stdout/stderr and next_cursor: 0 immediately. This acknowledges scheduling, not successful startup or service readiness. Save the exact session_id, then call exec_poll until logs or a health check confirm readiness. Compilation and sandbox startup may still fail; inspect exited state, exit_code, error_kind and error. Start, poll, verify and stop in separate dependent calls, not one concurrent batch. Once ready, use exec to run interface checks or a verification script, inspect the actual response, then call exec_stop even if verification fails. A separate exec can reach the service over TCP only when the host has enabled exec_network: host; polling works in either network mode.

Every successful call creates a new process session, including repeated identical commands. Stop the old session before restarting. At most 32 sessions are retained on this connection; new starts can evict completed sessions when full and are rejected if all 32 are active. Sessions belong to this MCP connection and are cleaned up when their ReAct execution ends, the connection closes or their lifetime expires. IDs do not carry across steps, delegation, disconnect or resume; an unknown ID requires inspecting current task state before starting a replacement. Workspace file changes remain after cleanup. ` + r.policy}, func(ctx context.Context, req *sdk.CallToolRequest, args execToolArgs) (*sdk.CallToolResult, sessionResult, error) {
		if r.startupErr != nil {
			return nil, sessionResult{}, r.startupErr
		}
		if !r.executor.commandAllowed(args.Command) {
			return nil, sessionResult{}, fmt.Errorf("unapproved command %q", args.Command)
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return nil, sessionResult{}, err
		}
		owner := r.ownerFor(req.Session)
		owner.mu.Lock()
		defer owner.mu.Unlock()
		if owner.closed {
			return nil, sessionResult{}, fmt.Errorf("execution connection closed")
		}
		if len(owner.sessions) >= maxExecSessions {
			// Reclaim a completed session before rejecting new work; active
			// processes are never evicted or killed to make room.
			for id, previous := range owner.sessions {
				select {
				case <-previous.done:
					delete(owner.sessions, id)
				default:
				}
				if len(owner.sessions) < maxExecSessions {
					break
				}
			}
			if len(owner.sessions) >= maxExecSessions {
				return nil, sessionResult{}, fmt.Errorf("active process session limit (32) reached; stop a session before starting another")
			}
		}
		limit := min(r.cfg.Timeout, 5*time.Minute)
		lifetime, cancel := context.WithTimeout(context.Background(), limit)
		s := &execSession{started: time.Now(), lifetime: limit, id: hex.EncodeToString(id[:]), done: make(chan struct{}), changed: make(chan struct{}), cancel: cancel}
		owner.sessions[s.id] = s
		go func() {
			defer cancel()
			result, err := r.executor.execute(lifetime, r.workspace, args.Command, args.Args, s.write)
			s.mu.Lock()
			s.result, s.err, s.exited = result, err, true
			close(s.done)
			close(s.changed)
			s.mu.Unlock()
		}()
		return sessionResponse(sessionResult{SessionID: s.id, State: "running", LifetimeMS: s.lifetime.Milliseconds()})
	})
}

func registerExecPollTool(server *sdk.Server, r *execSessionRegistry) {
	sdk.AddTool(server, &sdk.Tool{Name: string(config.ExecPollMCPToolName), InputSchema: toolInputSchema[sessionQuery](), Description: `Read a process session's logs and status without stopping it. Call after exec_start and while verifying a service. Example arguments: {"session_id":"id-from-exec_start","cursor":0,"wait_ms":1000}. Use the exact session_id returned on this MCP connection.

cursor is a nonnegative JSON integer (default 0). It is one absolute byte offset shared by stdout and stderr, not a line number or separate offset for each stream. For the next call, copy next_cursor exactly; do not derive it from returned string lengths. Logs are not consumed: reusing a cursor replays retained output, and the same arguments may be polled repeatedly. Only the latest 64 KiB across both streams is retained. truncated: true means some requested bytes were discarded; the response contains the retained tail and next_cursor advances past it. A cursor ahead of produced output is rejected.

wait_ms is a JSON integer from 0 to 1000 (default 0). If caught up and still running, wait up to this duration for new output or exit; otherwise return immediately. Empty logs after a wait do not imply readiness, failure or completion. Polling does not extend the session lifetime, and cancelling a poll does not stop the process.

Returns session_id, state, stdout, stderr, next_cursor, truncated, elapsed_ms (time since start), waited_ms (actual time waiting in this poll), and lifetime_ms (fixed host-configured deadline duration). state: "running" includes startup in progress; verify readiness using logs and actual interface checks. state: "exited" includes exit_code; a natural nonzero exit is an ordinary result that you must inspect. Failures encountered while executing the session return isError, error_kind and error while retaining available logs (timeout, canceled, sandbox_startup, parameter or cleanup). Invalid poll arguments and unknown IDs are tool errors and may provide only an error message. error_kind: "cleanup" means teardown could not be confirmed even though the command has ended. Stop polling after exit and assess the result. IDs are private to this connection and expire on disconnect, execution end, resume or eviction of a completed session; an unknown ID is an error, not evidence that the service is healthy.`}, func(ctx context.Context, req *sdk.CallToolRequest, args sessionQuery) (*sdk.CallToolResult, sessionResult, error) {
		if args.Cursor < 0 || args.WaitMS < 0 || args.WaitMS > 1000 {
			return nil, sessionResult{}, fmt.Errorf("cursor must be nonnegative and wait_ms between 0 and 1000")
		}
		s, err := r.findSession(req.Session, args.SessionID)
		if err != nil {
			return nil, sessionResult{}, err
		}
		s.mu.Lock()
		if err := validateSessionCursor(s, args.Cursor); err != nil {
			s.mu.Unlock()
			return nil, sessionResult{}, err
		}
		waitStarted := time.Now()
		if !s.exited && args.Cursor == s.cursor && args.WaitMS > 0 {
			changed := s.changed
			s.mu.Unlock()
			timer := time.NewTimer(time.Duration(args.WaitMS) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-changed:
			case <-timer.C:
			case <-ctx.Done():
				return nil, sessionResult{}, ctx.Err()
			}
			s.mu.Lock()
		}
		defer s.mu.Unlock()
		return sessionResponse(buildSessionResult(s, args.Cursor, waitStarted))
	})
}

func registerExecStopTool(server *sdk.Server, r *execSessionRegistry) {
	sdk.AddTool(server, &sdk.Tool{Name: string(config.ExecStopMCPToolName), InputSchema: toolInputSchema[sessionQuery](), Description: `Stop a process session after verification, on verification failure, or before restarting its command. Wait for termination and sandbox cleanup, then return final status and logs. Example arguments: {"session_id":"id-from-exec_start","cursor":0}. Use the exact session_id from exec_start on this connection and the latest next_cursor from exec_poll when available.

cursor is an optional nonnegative JSON integer byte offset (default 0), shared by stdout and stderr. Pass the previous next_cursor to return only unread logs; omitting it replays retained logs. A cursor ahead of output is rejected before stopping anything. wait_ms is accepted by the shared schema but ignored: it does not limit shutdown time.

Sends SIGTERM to the sandbox process group, gives it up to two seconds to exit, then forces cleanup if needed. Cleanup includes detached descendants; stopping go run also removes its compiled child server. A launcher may exit before its child finishes graceful shutdown, so verify shutdown evidence instead of assuming callbacks ran. Workspace file changes are not rolled back. Stopping an already exited, retained session returns its final status; repeated stops are safe. Cancelling this tool request does not undo an initiated stop.

Returns state: "exited", exit_code, stdout, stderr, next_cursor and truncated. Only the latest 64 KiB of combined logs is retained; truncated: true reports a gap. An explicitly requested stop may have a nonzero or signal-related exit_code without isError. Inspect the actual code and shutdown logs. Startup, timeout or cleanup failures remain tool errors with error_kind and error; error_kind: "cleanup" means resource release could not be confirmed and must not be reported as successful cleanup. IDs from other connections, previous executions, resumed runs or evicted sessions are rejected. This tool does not stop arbitrary host PIDs.`}, func(ctx context.Context, req *sdk.CallToolRequest, args sessionQuery) (*sdk.CallToolResult, sessionResult, error) {
		if args.Cursor < 0 || args.WaitMS < 0 || args.WaitMS > 1000 {
			return nil, sessionResult{}, fmt.Errorf("cursor must be nonnegative and wait_ms between 0 and 1000")
		}
		s, err := r.findSession(req.Session, args.SessionID)
		if err != nil {
			return nil, sessionResult{}, err
		}
		s.mu.Lock()
		if err := validateSessionCursor(s, args.Cursor); err != nil {
			s.mu.Unlock()
			return nil, sessionResult{}, err
		}
		if !s.exited {
			s.stopped = true
		}
		s.mu.Unlock()
		s.cancel()
		select {
		case <-s.done:
		case <-ctx.Done():
			return nil, sessionResult{}, ctx.Err()
		}
		waitStarted := time.Now()
		s.mu.Lock()
		defer s.mu.Unlock()
		return sessionResponse(buildSessionResult(s, args.Cursor, waitStarted))
	})
}

func sessionResponse(out sessionResult) (*sdk.CallToolResult, sessionResult, error) {
	data, err := json.Marshal(out)
	if err != nil {
		return nil, out, err
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(data)}}, StructuredContent: out, IsError: out.ErrorKind != ""}, out, nil
}
