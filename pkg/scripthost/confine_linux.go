//go:build linux

package scripthost

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Confine bounds the CALLING process and is irreversible. It runs in the child
// after the request is read and before the interpreter exists, so a defect in
// the interpreter or in a future host binding meets a kernel that already said
// no.
//
// It returns what it ACTUALLY applied, because the caller has to be able to
// refuse rather than shrug: a kernel with no Landlock leaves the filesystem
// and every socket open, and a Confine that reported only `error` made that
// indistinguishable from success. [RunChild] turns a zero ABI into
// [RefusalUnavailable].
//
// It is exported because the only honest witness for an irreversible,
// process-wide act is another process doing it: the escape suite runs a helper
// that calls this function and then attempts open, connect and exec. A test
// that asserted against a double would be asserting about the double.
//
// Order matters, and a failure is NOT atomic — an error can come back from a
// process that already carries some of its bounds and no ruleset. The caller
// must treat an error as "do not run the script", never as "retry
// unconfined"; [RunChild] does.
func Confine(l Limits) (Applied, error) {
	// Resolve the local zone before the filesystem closes. time.Local loads
	// the zoneinfo database from disk on first use, and after a deny-all
	// ruleset that read is refused.
	//
	// This has NO behavioural witness, and saying so is the point: without
	// it, the denied read makes Go fall back to UTC, which is the same answer.
	// It is kept to spare a confined child a refused syscall that anyone
	// auditing its behaviour would have to explain — a deliberate belt, not a
	// load-bearing bound. A mutation bench finds it as a survivor by design.
	time.Local = time.UTC

	// EVERY thread, not this one. no_new_privs is a per-THREAD flag, and so is
	// the Landlock domain below: a plain syscall from a goroutine confines
	// whichever OS thread happens to be carrying it and leaves every other
	// thread of the process untouched. Measured before this was fixed —
	// Confine reported LandlockABI 4 while a sibling thread read, wrote,
	// connected and exec'd without a refusal, and an ordinary goroutine
	// landed on an unconfined thread 4 times out of 4.
	allThreads := true
	if err := allThreadsPrctl(unix.PR_SET_NO_NEW_PRIVS, 1); err != nil {
		if !errors.Is(err, errAllThreadsUnsupported) {
			return Applied{}, fmt.Errorf("scripthost: prctl(PR_SET_NO_NEW_PRIVS) on all threads: %w", err)
		}
		// A cgo-linked binary cannot do this (the Go runtime refuses to drive
		// other threads it does not own). Fall back to this thread and SAY so
		// through Applied.AllThreads — never a silent per-thread confinement
		// presented as a per-process one.
		allThreads = false
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return Applied{}, fmt.Errorf("scripthost: prctl(PR_SET_NO_NEW_PRIVS): %w", err)
		}
	}
	// The process's own /proc/<pid>/environ and /proc/<pid>/mem stop being
	// readable by a same-uid process. In the in-pod posture every sibling runs
	// as the same uid, so this is the only thing between a script host and the
	// runner's memory.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return Applied{}, fmt.Errorf("scripthost: prctl(PR_SET_DUMPABLE, 0): %w", err)
	}

	applied := Applied{AllThreads: allThreads}

	var err error
	// RLIMIT_DATA, not RLIMIT_AS: a Go child dies at startup below roughly
	// 2 GiB of address space, which is far above any heap budget worth
	// setting. What this bounds is the Go HEAP — a MAP_SHARED or memfd mapping
	// is not charged to it, so a native payload's real ceiling is the pod's
	// memory cgroup.
	if applied.MemoryBytes, err = setBound(unix.RLIMIT_DATA, "RLIMIT_DATA", uint64(l.Memory)); err != nil {
		return Applied{}, err
	}
	// The only bound that stops a native regex bomb: goja's interrupt does not
	// preempt one. Per PROCESS, like the memory bound — a forked task restarts
	// the accounting at zero, so what bounds a process TREE is the parent's
	// wall deadline and the process-group kill, not this.
	if applied.CPUSeconds, err = setCPUBound(l.CPUSeconds()); err != nil {
		return Applied{}, err
	}

	switch applied.LandlockABI, err = landlockDenyAll(allThreads); {
	case err == nil:
		return applied, nil
	case errors.Is(err, errLandlockAbsent):
		// A kernel with no Landlock leaves the filesystem and every socket
		// open. That is reported, not swallowed: applied.LandlockABI is zero
		// and RunChild refuses on it unless the request carries the explicit
		// allowance.
		return applied, nil
	default:
		// Landlock exists and REFUSED. That is a bug or a policy conflict, and
		// it must not be read as absence — the two were collapsed once, and a
		// seccomp filter answering EPERM for landlock_create_ruleset (the
		// shape a container profile gives a syscall it does not allow) then
		// produced a child with a full filesystem and no error at all.
		return Applied{}, err
	}
}

// setBound applies one rlimit and returns what the kernel accepted.
//
// It reads the inherited hard limit and never asks to raise it. Asking for
// more than the inherited hard limit is a RAISE, which an unprivileged process
// may not make: it answered EPERM in any deployment that had already set one —
// `systemd LimitDATA=`, `docker --ulimit`, any wrapper — and codemode became
// unavailable for no gain. A stricter environment is a clamp, because
// tightening is the operator's choice and is always safe.
func setBound(resource int, name string, want uint64) (uint64, error) {
	if want == 0 {
		return 0, nil
	}
	var inherited unix.Rlimit
	if err := unix.Getrlimit(resource, &inherited); err != nil {
		return 0, fmt.Errorf("scripthost: getrlimit(%s): %w", name, err)
	}
	if want > inherited.Max {
		want = inherited.Max
	}
	if err := unix.Setrlimit(resource, &unix.Rlimit{Cur: want, Max: want}); err != nil {
		return 0, fmt.Errorf("scripthost: setrlimit(%s, %d): %w", name, want, err)
	}
	return want, nil
}

// setCPUBound applies RLIMIT_CPU with the soft limit at the operator's number
// and the hard limit ONE SECOND above it, clamped to whatever hard limit the
// environment already set.
//
// The gap is not slack, it is what makes the death attributable. The Go
// runtime ignores SIGXCPU, so the soft limit buys nothing and the SIGKILL at
// the hard limit is what kills; with equal limits that kill lands AT the
// bound, and the rusage the parent then reads is quantised just under it —
// measured 987 ms and 999 ms against a 1 s bound, on different runs of the
// same script. A parent comparing "burned >= bound" therefore flipped at
// random between `cpu_time_exceeded` and "nothing names a bound", and a
// verdict that flips at random is worse than one that costs a second.
//
// So the enforced ceiling is ceil(CPU)+1s where the environment allows it, the
// refusal names BOTH numbers — the bound the operator set and the instant the
// kernel kills at — and [Applied.CPUSeconds] reports the soft bound. Where a deployment's own hard limit equals the
// request, the kill lands on the bound and the parent falls back to its honest
// ambiguous verdict, which prints the burned CPU beside the bound.
func setCPUBound(secs uint64) (uint64, error) {
	if secs == 0 {
		return 0, nil
	}
	var inherited unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CPU, &inherited); err != nil {
		return 0, fmt.Errorf("scripthost: getrlimit(RLIMIT_CPU): %w", err)
	}
	soft, hard := secs, secs+1
	if hard > inherited.Max {
		hard = inherited.Max
	}
	if soft > hard {
		soft = hard
	}
	if err := unix.Setrlimit(unix.RLIMIT_CPU, &unix.Rlimit{Cur: soft, Max: hard}); err != nil {
		return 0, fmt.Errorf("scripthost: setrlimit(RLIMIT_CPU, %d/%d): %w", soft, hard, err)
	}
	return soft, nil
}

// errLandlockAbsent marks a kernel that does not OFFER Landlock, as opposed to
// one that offers it and refused. The two must not collapse: the first is a
// deployment fact the caller decides on, the second is a bug that has to stop
// the run.
var errLandlockAbsent = errors.New("scripthost: kernel offers no landlock")

// LandlockABI reports the kernel's Landlock ABI version, or 0 where Landlock
// is absent OR refused. It is the REPORTING form, for a bench that needs to
// record what it proved; the enforcing path uses [landlockABI], which keeps
// those two apart.
func LandlockABI() int {
	abi, _ := landlockABI()
	return abi
}

// landlockABI probes the ABI and tells absence from refusal.
//
// Only ENOSYS (no such syscall) and EOPNOTSUPP (Landlock compiled in but
// disabled at boot) mean absence — that is the pair the kernel documents and
// the pair its own reference sandboxer switches on. Every other errno is a
// refusal and is returned as an error, because a filter that answers EPERM for
// this syscall would otherwise read as "this kernel has no Landlock" and the
// child would run with nothing.
func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	return classifyLandlockProbe(abi, errno)
}

// classifyLandlockProbe is the decision landlockABI makes, separated from the
// syscall so it can be exercised with every errno a kernel or a seccomp filter
// can answer. It is the seam, not a convenience: the bug it exists to prevent
// is invisible on a kernel where Landlock works.
func classifyLandlockProbe(abi uintptr, errno unix.Errno) (int, error) {
	switch errno {
	case 0:
		return int(abi), nil
	case unix.ENOSYS, unix.EOPNOTSUPP:
		return 0, errLandlockAbsent
	default:
		return 0, fmt.Errorf("scripthost: landlock_create_ruleset(version query): %w", errno)
	}
}

// rulesetAttr mirrors the kernel's struct landlock_ruleset_attr. The net field
// exists from ABI 4 and the scope field from ABI 6; the struct is passed at
// the size its ABI knows.
//
// An oversized struct is not itself refused — the kernel accepts a longer one
// whose unknown tail is all zero, and answers E2BIG only when that tail
// carries bits (measured on ABI 4: size 24 with scoped=0 is accepted, size 24
// with scoped=0x3 is E2BIG). The ladder below is still required, because a
// RIGHT the running ABI does not know is EINVAL whatever the size.
type rulesetAttr struct {
	handledAccessFS  uint64
	handledAccessNet uint64
	scoped           uint64
}

// handledRights returns the access rights to declare for a given ABI, and the
// attr size to pass with them. Declaring a right the running kernel does not
// know is EINVAL, so the mask grows with the version instead of being a single
// constant — and a right added in a later ABI than this table knows is simply
// not denied, which is why [Applied.LandlockABI] is reported rather than
// assumed.
func handledRights(abi int) (fs, net, scoped uint64, size uintptr) {
	// ABI 1: the thirteen original filesystem rights.
	fs = unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	size = 8

	if abi >= 2 {
		fs |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		fs |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 4 {
		// TCP only. UDP, AF_UNIX (pathname and abstract) and netlink are NOT
		// governed by these rights; see codemode-confinement.md.
		net = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
		size = 16
	}
	if abi >= 5 {
		fs |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	if abi >= 6 {
		// The two scopes close the holes the net rights cannot reach: an
		// abstract unix socket the child could otherwise connect to or offer,
		// and a signal it could otherwise send to a sibling in the pod.
		scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
		size = 24
	}
	return fs, net, scoped, size
}

// landlockDenyAll declares every right this kernel knows and adds no rule, so
// every path and every governed endpoint is denied. A ruleset with no rule is
// the deny-all form — there is no separate "deny" verb to call. It returns the
// ABI it applied.
func landlockDenyAll(allThreads bool) (int, error) {
	abi, err := landlockABI()
	if err != nil {
		return 0, err
	}
	if abi <= 0 {
		// A zero ABI with no errno is not a shape the kernel documents;
		// refusing beats guessing which way it meant.
		return 0, fmt.Errorf("scripthost: landlock_create_ruleset(version query) answered ABI %d", abi)
	}

	fs, net, scoped, size := handledRights(abi)
	attr := rulesetAttr{handledAccessFS: fs, handledAccessNet: net, scoped: scoped}

	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), size, 0)
	if errno != 0 {
		return 0, fmt.Errorf("scripthost: landlock_create_ruleset(abi %d, fs %#x, net %#x, scoped %#x): %w",
			abi, fs, net, scoped, errno)
	}
	defer unix.Close(int(fd))

	// restrict_self attaches the domain to the CALLING THREAD. Driving it
	// across every thread is what makes it the process's domain; the Go
	// runtime also replays it onto threads created afterwards.
	if allThreads {
		if _, _, errno := syscall.AllThreadsSyscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
			return 0, fmt.Errorf("scripthost: landlock_restrict_self(abi %d) on all threads: %w", abi, errno)
		}
		return abi, nil
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return 0, fmt.Errorf("scripthost: landlock_restrict_self(abi %d): %w", abi, errno)
	}
	return abi, nil
}

// errAllThreadsUnsupported marks a binary that cannot drive its own threads —
// cgo-linked, which the Go runtime answers with ENOTSUP. Distinct from a real
// refusal, because it decides a posture rather than failing a run.
var errAllThreadsUnsupported = errors.New("scripthost: this binary cannot apply a syscall to all its threads")

// allThreadsPrctl applies a prctl to every OS thread.
func allThreadsPrctl(option, arg2 uintptr) error {
	if _, _, errno := syscall.AllThreadsSyscall(unix.SYS_PRCTL, option, arg2, 0); errno != 0 {
		if errno == unix.ENOTSUP || errno == unix.EOPNOTSUPP {
			return errAllThreadsUnsupported
		}
		return errno
	}
	return nil
}
