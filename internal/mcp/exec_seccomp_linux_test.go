package mcp_test

import (
	"context"
	"runtime"
	"strconv"
	"testing"

	mcp "github.com/collie-agent/codex-go/internal/mcp"
	"golang.org/x/sys/unix"
)

func TestCommandExecutorBlocksSocketFilterBypasses(t *testing.T) {
	executor, err := mcp.NewCommandExecutor()
	if err != nil {
		t.Fatal(err)
	}
	// Exercise raw syscalls, bypassing libc/Python wrappers. io_uring must
	// fail before inspecting its arguments, even on kernels without support.
	numbers := []string{strconv.Itoa(unix.SYS_IO_URING_SETUP)}
	if runtime.GOARCH == "amd64" {
		numbers = append(numbers, strconv.Itoa(unix.SYS_SOCKET|0x40000000))
	}
	args := append([]string{"-c", `import ctypes,errno,sys
libc=ctypes.CDLL(None, use_errno=True)
libc.syscall.restype=ctypes.c_long
for number in sys.argv[1:]:
 ctypes.set_errno(0)
 assert libc.syscall(ctypes.c_long(int(number)), 0, 0, 0, 0, 0, 0) == -1
 assert ctypes.get_errno() == errno.EPERM, (number, ctypes.get_errno())
print('blocked')`}, numbers...)
	result, err := executor.Execute(context.Background(), t.TempDir(), "python3", args)
	if err != nil || result.ExitCode != 0 || result.Stdout != "blocked\n" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
