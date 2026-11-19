//go:build linux

package scripthost

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The network wall. Landlock has exactly two network rights — BIND_TCP and
// CONNECT_TCP, from ABI 4 — so a confined child could always send UDP, open an
// AF_UNIX or netlink socket, and on a kernel below 6.7 open TCP as well:
// measured on the production nodes (kernel 6.1, ABI 2), a compromised child's
// connect SUCCEEDED, and on every kernel a UDP datagram reached a listener.
//
// The child needs no socket at all — its channel to the parent is two pipes
// opened by the spawn — so it denies itself socket CREATION with a seccomp
// filter: socket, socketpair, and the io_uring trio (io_uring can create a
// socket without socket(2), IORING_OP_SOCKET since 5.19). That closes every
// family at once, on every kernel that has seccomp, without a privilege:
// measured under docker's and containerd's default profiles with every
// capability dropped, the filter installs and refuses all of it.
//
// A socket can also be BORROWED rather than created: pidfd_getfd duplicates a
// descriptor out of another process, and ptrace or process_vm_writev can make
// the runner itself act. Each needs ptrace access to the target, which
// Landlock's ptrace scoping refuses from ABI 1 — but a child allowed to run
// without Landlock (ABI 0) has only Yama, a host setting this package does not
// control. The child borrows from nobody, so the same filter denies the four
// cross-process calls too: the wall holds on every posture it is claimed for.
//
// A network namespace per child would be tighter and is refused there: the
// default profiles deny CLONE_NEWUSER without CAP_SYS_ADMIN (EPERM, measured).

// socketDenySpec is one architecture's half of the filter, as DATA, so the
// program can be evaluated for every supported architecture on any host.
type socketDenySpec struct {
	arch   uint32   // the AUDIT_ARCH_* the kernel reports in seccomp_data.arch
	x32    bool     // amd64: a number with the x32 bit set is ANOTHER ABI
	denied []uint32 // socket, socketpair, the io_uring trio, then ptrace, process_vm_readv/writev, pidfd_getfd
}

// socketDenySpecs holds literal numbers because unix.SYS_* only exists for the
// architecture being compiled; a test pins each row to the vendored x/sys
// tables, so a literal cannot drift from the kernel's numbering.
var socketDenySpecs = map[string]socketDenySpec{
	"amd64": {arch: unix.AUDIT_ARCH_X86_64, x32: true, denied: []uint32{41, 53, 425, 426, 427, 101, 310, 311, 438}},
	"arm64": {arch: unix.AUDIT_ARCH_AARCH64, denied: []uint32{198, 199, 425, 426, 427, 117, 270, 271, 438}},
}

const x32SyscallBit = 0x40000000

// errSocketDenialUnsupported marks an architecture this filter has no row
// for. It fails the confinement rather than degrading it: a child without the
// network wall is not what codemode promises, and the release ships only the
// architectures above.
var errSocketDenialUnsupported = errors.New("scripthost: no socket-denial filter for this architecture")

// socketDenyFilter is the classic-BPF program, a pure function of the spec.
//
// A syscall from another architecture, or (amd64) through the x32 ABI, KILLS
// the process: those reach the kernel under a different numbering, where 41
// need not be socket — the standard bypass of a filter that only compares
// numbers. A denied number answers EACCES, the errno Landlock answers with, so
// a refusal reads the same whichever wall stopped it. Everything else is
// allowed: this filter adds one wall, it is not an allowlist.
func socketDenyFilter(s socketDenySpec) []unix.SockFilter {
	const (
		ldAbs = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge   = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		ret   = unix.BPF_RET | unix.BPF_K
		// offsets into struct seccomp_data
		offNr   = 0
		offArch = 4
	)
	f := []unix.SockFilter{
		{Code: ldAbs, K: offArch},
		{Code: jeq, Jt: 1, Jf: 0, K: s.arch},
		{Code: ret, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: ldAbs, K: offNr},
	}
	if s.x32 {
		f = append(f,
			unix.SockFilter{Code: jge, Jt: 0, Jf: 1, K: x32SyscallBit},
			unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_KILL_PROCESS},
		)
	}
	for i, nr := range s.denied {
		// On a match, skip the remaining comparisons and the ALLOW.
		f = append(f, unix.SockFilter{Code: jeq, Jt: uint8(len(s.denied) - i), Jf: 0, K: nr})
	}
	return append(f,
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EACCES)},
	)
}

// denySocketCreation installs the filter on EVERY thread of the process.
//
// SECCOMP_FILTER_FLAG_TSYNC is the kernel's own all-threads path: a plain
// install binds the calling thread and its future children only, the same
// trap landlock_restrict_self is (see Confine). It needs no_new_privs, which
// Confine has already set, and the kernel extends that flag to the threads it
// synchronises — so it holds for the cgo fallback too, where Go itself cannot
// drive the other threads.
func denySocketCreation() error {
	spec, ok := socketDenySpecs[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("%w: GOARCH=%s", errSocketDenialUnsupported, runtime.GOARCH)
	}
	f := socketDenyFilter(spec)
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	r, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(f)
	if errno != 0 {
		return fmt.Errorf("scripthost: seccomp(SET_MODE_FILTER, TSYNC) for the socket denial: %w", errno)
	}
	if r != 0 {
		return fmt.Errorf("scripthost: seccomp TSYNC could not synchronise thread %d, so the socket "+
			"denial would hold on some threads only", r)
	}
	return nil
}

// inheritedSocket names the first descriptor this process holds that is a
// socket, or -1. The filter denies CREATING a socket; an inherited one would
// walk around it, so the child refuses to run with one. The spawn hands it
// pipes only (stdio and the tool channel), and Go closes everything else at
// exec — this checks that instead of trusting it.
//
// It reads /proc/self/fd, so it must run BEFORE Confine: the deny-all ruleset
// refuses that read afterwards.
func inheritedSocket() (int, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1, fmt.Errorf("scripthost: listing this process's descriptors: %w", err)
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			// The directory's own descriptor is closed by the time it is
			// stat'ed; anything else vanishing is equally not a socket.
			continue
		}
		if st.Mode&unix.S_IFMT == unix.S_IFSOCK {
			return fd, nil
		}
	}
	return -1, nil
}
