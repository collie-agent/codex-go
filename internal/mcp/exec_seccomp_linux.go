package mcp

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// commandSocketFilter denies creation of Unix sockets, including datagram
// sockets which can reach a host service without connect(). socketpair remains
// available for local IPC. No caller-provided descriptors reach the command.
func commandSocketFilter() (*os.File, error) {
	architectures := map[string]uint32{
		"386": unix.AUDIT_ARCH_I386, "amd64": unix.AUDIT_ARCH_X86_64,
		"arm": unix.AUDIT_ARCH_ARM, "arm64": unix.AUDIT_ARCH_AARCH64,
		"loong64": unix.AUDIT_ARCH_LOONGARCH64, "riscv64": unix.AUDIT_ARCH_RISCV64,
		"mips": unix.AUDIT_ARCH_MIPS, "mipsle": unix.AUDIT_ARCH_MIPSEL,
		"mips64": unix.AUDIT_ARCH_MIPS64, "mips64le": unix.AUDIT_ARCH_MIPSEL64,
		"ppc64": unix.AUDIT_ARCH_PPC64, "ppc64le": unix.AUDIT_ARCH_PPC64LE,
		"s390x": unix.AUDIT_ARCH_S390X,
	}
	arch, ok := architectures[runtime.GOARCH]
	if !ok {
		return nil, fmt.Errorf("unsupported seccomp architecture: %s", runtime.GOARCH)
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4}, // seccomp_data.arch
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}, // syscall number
	}
	// Reject alternate ABIs rather than letting their syscall numbers bypass
	// the native filter. x32 shares the amd64 audit architecture identifier.
	if runtime.GOARCH == "amd64" {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: 0x40000000, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	denied := []uint32{unix.SYS_IO_URING_SETUP}
	// Legacy socketcall hides the socket domain behind a userspace pointer.
	// io_uring can also create sockets without invoking the socket syscall.
	switch runtime.GOARCH {
	case "386", "ppc64", "ppc64le", "s390x":
		denied = append(denied, 102)
	case "mips", "mipsle":
		denied = append(denied, 4102)
	}
	for _, number := range denied {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: number, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	// The low word of args[0] holds the socket domain; big endian ABIs put it
	// four bytes later in the 64-bit seccomp argument slot.
	domainOffset := uint32(16)
	if binary.NativeEndian.Uint16([]byte{0, 1}) == 1 {
		domainOffset += 4
	}
	filter = append(filter,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_SOCKET, Jf: 3},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: domainOffset},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_UNIX, Jf: 1},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	)
	file, err := os.CreateTemp("", "command-sandbox-seccomp-*")
	if err != nil {
		return nil, err
	}
	// Unlink before handing bwrap the descriptor so no filter file survives a
	// crash or becomes accessible through a mounted temporary directory.
	err = os.Remove(file.Name())
	if err == nil {
		err = binary.Write(file, binary.NativeEndian, filter)
	}
	if err == nil {
		_, err = file.Seek(0, io.SeekStart)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
