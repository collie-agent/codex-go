//go:build linux

package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

func configureSessionStop(cmd *exec.Cmd, status *os.File) func() error {
	var mu sync.Mutex
	pidfd := -1
	var pidfdErr error
	sandboxPID := func() int {
		var info struct {
			PID int `json:"child-pid"`
		}
		_ = json.NewDecoder(io.NewSectionReader(status, 0, 4096)).Decode(&info)
		return info.PID
	}
	openPID := func(pid int) {
		pidfd, pidfdErr = unix.PidfdOpen(pid, 0)
		if errors.Is(pidfdErr, unix.ESRCH) {
			pidfdErr = nil
		}
		if pidfd < 0 {
			return
		}
		// Pin first, then verify ownership while outer bwrap is still alive.
		// A stale status PID must never let cleanup signal a reused host PID.
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		parent := 0
		var state string
		if split := strings.LastIndex(string(data), ") "); split >= 0 {
			_, _ = fmt.Sscanf(string(data)[split+2:], "%s %d", &state, &parent)
		}
		if err != nil || parent != cmd.Process.Pid {
			unix.Close(pidfd)
			pidfd = -1
			pidfdErr = fmt.Errorf("cannot confirm sandbox init ownership")
		}
	}
	// bwrap's --new-session makes the sandbox init the process group leader.
	// Signal that group rather than bwrap: killing bwrap immediately would
	// trigger --die-with-parent before the server can handle SIGTERM.
	cmd.Cancel = func() error {
		if pid := sandboxPID(); pid > 1 {
			mu.Lock()
			openPID(pid)
			alive := pidfd >= 0
			failed := pidfdErr != nil
			mu.Unlock()
			if !alive && !failed {
				return os.ErrProcessDone
			}
			if alive {
				if err := unix.Kill(-pid, unix.SIGTERM); err == nil {
					return nil
				}
			}
		}
		// Cancellation during sandbox setup may precede the PID status record.
		return cmd.Process.Kill()
	}
	// os/exec escalates to killing bwrap after the grace period. Its PID
	// namespace then reaps every descendant, including detached processes.
	cmd.WaitDelay = 2 * time.Second
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if pidfd < 0 {
			// Never acquire a PID from the status file after bwrap exits: the
			// kernel may already have reused it. Normal exit reaps namespace init.
			return pidfdErr
		}
		defer unix.Close(pidfd)
		// During startup bwrap can still be arranging parent-death handling.
		// Kill namespace init explicitly after outer bwrap has exited; killing
		// PID 1 forces the kernel to reap even detached descendants.
		if err := unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
			return err
		}
		// Killing outer bwrap is asynchronous. Wait for namespace init to
		// exit as well, so detached servers have released their listening FDs
		// before exec_stop reports completion. pidfd avoids PID reuse races.
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			n, err := unix.Poll(fds, int(time.Until(deadline).Milliseconds())+1)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
		}
		return fmt.Errorf("sandbox init did not exit after forced stop")
	}
}
