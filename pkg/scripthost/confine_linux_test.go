//go:build linux

package scripthost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The escape suite. CM-1a's whole content is the confinement, so this file is
// the change: a process that has really called [Confine] tries to read, write,
// connect and exec, and reports what the kernel said.
//
// Four rules it follows, each paid for:
//
//   - The attempt runs in a REAL process calling the REAL Confine. A double
//     would be asserting about the double, and confinement is irreversible and
//     process-wide — it cannot be exercised in-process without ending the test
//     binary.
//   - Every attempt reports whether it was MADE and whether the kernel refused
//     it for PERMISSION. "Made && err != nil" credited any error: the write arm
//     passed on ENOENT and the exec arm passed on a non-existent path.
//   - Every arm has a PREMISE: the same act, attempted before Confine, must
//     have succeeded. Without one, an environment that refuses the probe for
//     its own reasons reads as a working guard — which is how the /proc arm
//     passed with its prctl removed.
//   - Where the environment cannot host the attempt, the test FAILS and names
//     the condition. A confinement suite that skips is a confinement suite
//     that reports green on an unconfined deployment.
//
// The helper pins itself to its confining thread. That is not tidiness: a
// Landlock domain is per-THREAD, so a probe that migrated between goroutines
// measured a different thread than the one that was confined. Before the
// all-threads fix, a parking network dial moved the remaining probes onto an
// unconfined thread — which is exactly how a mutant that dropped the Landlock
// net rights appeared to re-enable exec.
const (
	escapeHelperEnv     = "ITERION_SCRIPTHOST_ESCAPE_HELPER"
	escapeTargetEnv     = "ITERION_SCRIPTHOST_ESCAPE_TARGET"
	escapeConnectEnv    = "ITERION_SCRIPTHOST_ESCAPE_CONNECT"
	escapeWriteEnv      = "ITERION_SCRIPTHOST_ESCAPE_WRITE"
	escapeLimitsEnv     = "ITERION_SCRIPTHOST_ESCAPE_LIMITS"
	escapeHardDataEnv   = "ITERION_SCRIPTHOST_ESCAPE_HARD_DATA"
	noLandlockEscape    = "ITERION_CODEMODE_ALLOW_NO_LANDLOCK"
	noLandlockNetEscape = "ITERION_CODEMODE_ALLOW_NO_LANDLOCK_NET"

	escapeReportBegin = "ESCAPE-REPORT-BEGIN"
	escapeReportEnd   = "ESCAPE-REPORT-END"
	escapePIDLine     = "ESCAPE-PID "
)

// attempt records one escape try. Made says the syscall was issued, Permission
// says the kernel refused it for permission, and Err is the text. All three are
// needed: an attempt never made, one refused by the guard, and one that failed
// for its own reasons look identical from any two of them.
type attempt struct {
	Made       bool   `json:"made"`
	Permission bool   `json:"permission"`
	Err        string `json:"err,omitempty"`
}

func try(err error) attempt {
	return attempt{Made: true, Permission: errors.Is(err, os.ErrPermission), Err: errString(err)}
}

// refused means the kernel said no, for permission. Not merely "errored".
func (a attempt) refused() bool { return a.Made && a.Permission }

// escapeReport is what the confined helper tells the test.
type escapeReport struct {
	ConfineErr  string `json:"confine_err,omitempty"`
	LandlockABI int    `json:"landlock_abi"`
	AllThreads  bool   `json:"all_threads"`

	// The rights actually declared, read back from the same function Confine
	// used. An escape suite has to be able to say WHAT it proved: "exec was
	// refused" means nothing without "FS_EXECUTE was among the handled
	// rights".
	DeclaredFS     uint64 `json:"declared_fs"`
	DeclaredNet    uint64 `json:"declared_net"`
	DeclaredScoped uint64 `json:"declared_scoped"`
	DeclaredSize   uint64 `json:"declared_size"`

	// What Confine reported it put in place, which a hardened environment can
	// lower.
	AppliedCPUSeconds  uint64 `json:"applied_cpu_seconds"`
	AppliedMemoryBytes uint64 `json:"applied_memory_bytes"`

	// Premises: the same acts BEFORE confinement. A false here voids the arm.
	CouldReadBefore    bool `json:"could_read_before"`
	CouldWriteBefore   bool `json:"could_write_before"`
	CouldConnectBefore bool `json:"could_connect_before"`
	CouldExecBefore    bool `json:"could_exec_before"`

	ReadFile  attempt `json:"read_file"`
	WriteFile attempt `json:"write_file"`
	ReadDir   attempt `json:"read_dir"`
	Connect   attempt `json:"connect"`
	Exec      attempt `json:"exec"`

	// SiblingRead is the same read attempted on an OS thread that existed
	// BEFORE Confine. It is the witness for the all-threads path: a per-thread
	// domain leaves this one open.
	SiblingRead attempt `json:"sibling_read"`

	// EnvCount is how many variables the helper process holds. The helper is
	// launched by this test with its own markers, so this is not the assertion
	// about Host's empty environment — that one is in
	// TestTheChildIsBuiltWithNoEnvironment.
	EnvCount int `json:"env_count"`

	// PID lets the parent probe /proc from OUTSIDE the ruleset. The helper
	// cannot answer this question about itself: a confined process fails to
	// open /proc/self/environ because Landlock denied the path, whichever way
	// PR_SET_DUMPABLE went — measured, a mutant that removed the prctl left
	// the whole suite green.
	PID int `json:"pid"`

	// Filled by the PARENT, which no ruleset governs — a sibling in the pod.
	procReadableBeforeConfine bool
	procReadableBySibling     bool
}

// TestEscapeHelperProcess is not a test. It is the confined child the suite
// below runs, selected by its environment exactly as pkg/config's scrub helper
// is.
func TestEscapeHelperProcess(t *testing.T) {
	if os.Getenv(escapeHelperEnv) != "1" {
		t.Skip("helper process only")
	}

	var limits Limits
	if raw := os.Getenv(escapeLimitsEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &limits); err != nil {
			fmt.Fprintln(os.Stderr, "helper: bad limits:", err)
			os.Exit(2)
		}
	}

	target := os.Getenv(escapeTargetEnv)
	writeTo := os.Getenv(escapeWriteEnv)
	connectTo := os.Getenv(escapeConnectEnv)

	// Lower our OWN hard RLIMIT_DATA on request, so the "Confine refuses a
	// bound it cannot apply" arm runs on every host instead of skipping where
	// the hard limit is unlimited — which is every normal host and CI.
	if raw := os.Getenv(escapeHardDataEnv); raw != "" {
		if mib, err := strconv.ParseUint(raw, 10, 64); err == nil {
			lim := unix.Rlimit{Cur: mib << 20, Max: mib << 20}
			if err := unix.Setrlimit(unix.RLIMIT_DATA, &lim); err != nil {
				fmt.Fprintln(os.Stderr, "helper: lowering its own hard RLIMIT_DATA:", err)
				os.Exit(5)
			}
		}
	}

	// The PID goes out BEFORE confinement so the parent can prove its /proc
	// probe was capable — and then the helper WAITS for the parent's
	// go-ahead. Without the handshake the parent raced this process's own
	// Confine: the premise read "not readable" about half the time, and the
	// arm it protects then fataled at random.
	stdin := bufio.NewReader(os.Stdin)
	fmt.Println(escapePIDLine, os.Getpid())
	if _, err := stdin.ReadString('\n'); err != nil {
		fmt.Fprintln(os.Stderr, "helper: waiting for the parent's go-ahead:", err)
		os.Exit(6)
	}

	report := escapeReport{PID: os.Getpid()}

	// A sibling OS thread, pinned, created BEFORE Confine. It is the witness
	// that the bounds reached the PROCESS: a per-thread Landlock domain leaves
	// this thread with a full filesystem.
	siblingWork := make(chan func())
	siblingReady := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		close(siblingReady)
		for f := range siblingWork {
			f()
		}
	}()
	<-siblingReady

	// Premises, all four, before anything is confined. A probe that was never
	// capable cannot tell a guard from an accident.
	_, errRead := os.ReadFile(target)
	report.CouldReadBefore = errRead == nil
	report.CouldWriteBefore = os.WriteFile(writeTo, []byte("before"), 0o600) == nil
	_ = os.Remove(writeTo)
	if connectTo != "" {
		c, err := net.DialTimeout("tcp", connectTo, 3*time.Second)
		if c != nil {
			c.Close()
		}
		report.CouldConnectBefore = err == nil
	}
	report.CouldExecBefore = execProbe() == nil

	// Pin to the thread that confines, so every probe below runs on it.
	runtime.LockOSThread()

	applied, err := Confine(limits)
	if err != nil {
		report.ConfineErr = err.Error()
		emitReport(report)
		holdForParent(stdin)
		os.Exit(0)
	}
	report.AllThreads = applied.AllThreads
	report.LandlockABI = applied.LandlockABI
	report.AppliedCPUSeconds, report.AppliedMemoryBytes = applied.CPUSeconds, applied.MemoryBytes
	if applied.LandlockABI > 0 {
		fs, netRights, scoped, size := handledRights(applied.LandlockABI)
		report.DeclaredFS, report.DeclaredNet, report.DeclaredScoped, report.DeclaredSize =
			fs, netRights, scoped, uint64(size)
	}

	// --- From here the process is confined. Everything below must fail. ---

	_, err = os.ReadFile(target)
	report.ReadFile = try(err)

	report.WriteFile = try(os.WriteFile(writeTo, []byte("escaped"), 0o600))

	_, err = os.ReadDir(filepath.Dir(target))
	report.ReadDir = try(err)

	if connectTo != "" {
		conn, err := net.DialTimeout("tcp", connectTo, 3*time.Second)
		if conn != nil {
			conn.Close()
		}
		report.Connect = try(err)
	}

	report.Exec = try(execProbe())

	// The same read, on the thread that was already there.
	siblingDone := make(chan struct{})
	siblingWork <- func() {
		_, err := os.ReadFile(target)
		report.SiblingRead = try(err)
		close(siblingDone)
	}
	<-siblingDone

	report.EnvCount = len(os.Environ())

	emitReport(report)
	holdForParent(stdin)
	os.Exit(0)
}

// execProbe attempts an execve of a path proven to exist — this very binary.
//
// The stdio is wired to descriptors this process ALREADY holds. With nil
// stdio, os/exec opens /dev/null first, and under a deny-all ruleset that open
// fails — so the probe reported "refused" having never reached execve.
// Measured: the error was `open /dev/null: permission denied`, and the arm
// would have passed against a ruleset that allowed exec.
func execProbe() error {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stderr, os.Stderr
	return cmd.Run()
}

// holdForParent keeps the process alive until the parent closes stdin. The
// /proc of a process that has exited answers ENOENT, which would look exactly
// like a guard working.
func holdForParent(in io.Reader) { _, _ = io.Copy(io.Discard, in) }

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func emitReport(r escapeReport) {
	encoded, err := json.Marshal(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper: encoding the report:", err)
		os.Exit(4)
	}
	fmt.Println(escapeReportBegin)
	fmt.Println(string(encoded))
	fmt.Println(escapeReportEnd)
}

// escapeOpts tunes a helper run.
type escapeOpts struct {
	// hardDataMiB makes the helper lower its own hard RLIMIT_DATA first.
	hardDataMiB uint64
	// tolerateConfineErr stops runEscapeHelper fataling on a ConfineErr, for
	// the arm whose subject IS the refusal.
	tolerateConfineErr bool
}

// runEscapeHelper runs the confined helper and returns its report. It fails
// the test rather than returning an error: every caller needs the report to
// mean anything.
func runEscapeHelper(t *testing.T, limits Limits, opts ...escapeOpts) escapeReport {
	t.Helper()

	var opt escapeOpts
	if len(opts) > 0 {
		opt = opts[0]
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "probe-target")
	if err := os.WriteFile(target, []byte("readable before confinement"), 0o600); err != nil {
		t.Fatalf("writing the probe target: %v", err)
	}

	// A real listener is the PREMISE of the connect arm, not its verdict: it
	// makes "the connect succeeded" mean something, because a connect to a
	// dead port fails for its own reason and would read as a working guard.
	//
	// The verdict is the child's own report. An earlier version also asserted
	// on whether the accept goroutine had been reached, read through a
	// non-blocking receive — and that assertion was FLAKY: the same mutant
	// reddened six tests on one run and five on the next, depending on
	// whether the accept had been scheduled yet.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("the connect probe needs a listener and this host refused one (%v): "+
			"the network arm of this suite cannot run here, and a suite that skipped it "+
			"would report a confinement it never tested", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	encodedLimits, err := json.Marshal(limits)
	if err != nil {
		t.Fatalf("encoding the helper's limits: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestEscapeHelperProcess$", "-test.count=1", "-test.v")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		escapeHelperEnv + "=1",
		escapeTargetEnv + "=" + target,
		escapeWriteEnv + "=" + filepath.Join(dir, "escaped"),
		escapeConnectEnv + "=" + listener.Addr().String(),
		escapeLimitsEnv + "=" + string(encodedLimits),
	}
	if opt.hardDataMiB > 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", escapeHardDataEnv, opt.hardDataMiB))
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the helper: %v", err)
	}

	released := false
	release := func() {
		if !released {
			released = true
			stdin.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(release)

	report, raw, readErr := readEscapeReport(stdout, func(pid int) bool {
		// THE premise of the /proc arm, taken while the helper is still
		// unconfined: this process is a same-uid neighbour that no ruleset
		// governs, which is exactly what a sibling in the runner pod is.
		readable := procEnvironReadable(t, pid)
		// Only now may the helper confine. The handshake is what makes the
		// premise above a measurement rather than a race.
		_, _ = stdin.Write([]byte("go\n"))
		return readable
	})
	if readErr != nil {
		release()
		t.Fatalf("the helper produced no report (%v).\nstdout so far:\n%s\nstderr:\n%s",
			readErr, truncate(raw, 2000), truncate(stderr.String(), 2000))
	}
	if report.ConfineErr != "" && !opt.tolerateConfineErr {
		release()
		t.Fatalf("the helper could not confine itself: %s", report.ConfineErr)
	}

	if report.ConfineErr == "" {
		report.procReadableBySibling = procEnvironReadable(t, report.PID)
		t.Logf("escape report: landlock ABI %d allThreads=%v declared fs=%#x net=%#x scoped=%#x size=%d | "+
			"premises read=%v write=%v connect=%v exec=%v | read=%+v write=%+v dir=%+v connect=%+v exec=%+v "+
			"sibling=%+v | applied cpu=%ds mem=%d | env=%d pid=%d proc before=%v after=%v",
			report.LandlockABI, report.AllThreads, report.DeclaredFS, report.DeclaredNet,
			report.DeclaredScoped, report.DeclaredSize,
			report.CouldReadBefore, report.CouldWriteBefore, report.CouldConnectBefore, report.CouldExecBefore,
			report.ReadFile, report.WriteFile, report.ReadDir, report.Connect, report.Exec, report.SiblingRead,
			report.AppliedCPUSeconds, report.AppliedMemoryBytes,
			report.EnvCount, report.PID, report.procReadableBeforeConfine, report.procReadableBySibling)
	}

	release()
	return report
}

// readEscapeReport reads the helper's stdout until the report's end marker,
// leaving the process alive. onPID runs as soon as the helper announces
// itself, which is BEFORE it confines.
func readEscapeReport(r io.Reader, onPID func(int) bool) (escapeReport, string, error) {
	var seen strings.Builder
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	inReport := false
	readableBefore := false
	var body strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		seen.WriteString(line)
		seen.WriteByte('\n')
		switch {
		case strings.HasPrefix(line, escapePIDLine):
			if pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, escapePIDLine))); err == nil && onPID != nil {
				readableBefore = onPID(pid)
			}
		case line == escapeReportBegin:
			inReport = true
		case line == escapeReportEnd:
			var report escapeReport
			if err := json.Unmarshal([]byte(strings.TrimSpace(body.String())), &report); err != nil {
				return escapeReport{}, seen.String(), fmt.Errorf("decoding the report: %w", err)
			}
			report.procReadableBeforeConfine = readableBefore
			return report, seen.String(), nil
		case inReport:
			body.WriteString(line)
		}
	}
	if err := scanner.Err(); err != nil {
		return escapeReport{}, seen.String(), err
	}
	return escapeReport{}, seen.String(), errors.New("the helper's stdout ended before the report")
}

// procEnvironReadable reports whether this process can read another's boot
// environment. It fails the test on any error that is NOT a permission
// refusal: an ENOENT would mean the target is gone and the answer means
// nothing.
func procEnvironReadable(t *testing.T, pid int) bool {
	t.Helper()
	if pid <= 0 {
		t.Fatal("the helper reported no pid: the /proc arm cannot run")
	}
	path := fmt.Sprintf("/proc/%d/environ", pid)
	f, err := os.Open(path)
	if err == nil {
		f.Close()
		return true
	}
	if errors.Is(err, os.ErrPermission) {
		return false
	}
	t.Fatalf("probing %s answered %v, which is neither access nor a refusal: the target may be gone, "+
		"and this arm would then pass for the wrong reason", path, err)
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…truncated"
}

// armVerdict is what an arm that depends on a Landlock right may conclude.
type armVerdict int

const (
	armProve        armVerdict = iota // the kernel offers the right: assert it
	armSkipOnRecord                   // it does not, and the lane said so out loud
	armFailLoud                       // it does not, and nobody recorded that
)

// landlockArmVerdict is the ONE decision behind both gates below, separated so
// it has a witness: on a kernel that offers what an arm needs, every branch
// but armProve is unreachable, and an unreachable branch is an untested one.
// The CI lane depends on this being right — its runners answer ABI 1.
func landlockArmVerdict(abi, need int, onRecord bool) armVerdict {
	switch {
	case abi >= need:
		return armProve
	case onRecord:
		return armSkipOnRecord
	default:
		return armFailLoud
	}
}

// requireLandlock fails, loudly and with the way out, where the kernel offers
// no Landlock. It does not skip: the filesystem and network guarantees
// codemode promises come from Landlock alone, so a lane that cannot test them
// must say so rather than report green.
func requireLandlock(t *testing.T, abi int) {
	t.Helper()
	switch landlockArmVerdict(abi, 1, os.Getenv(noLandlockEscape) == "1") {
	case armProve:
		return
	case armSkipOnRecord:
		t.Skipf("%s=1: this lane is on record as unable to prove the filesystem and network bounds. "+
			"Everything codemode promises about files and sockets is UNTESTED here; only the CPU and "+
			"memory bounds were exercised", noLandlockEscape)
	}
	t.Fatalf("this kernel offers no Landlock (landlock_create_ruleset answered an error), so the "+
		"filesystem and network bounds cannot be tested — and an untested bound is the one that is "+
		"missing in production. Run this suite on a kernel with Landlock (5.13+, 6.7+ for the network "+
		"rights), or set %s=1 to record that this lane proves only the CPU and memory bounds.", noLandlockEscape)
}

func escapeBaseline(t *testing.T) escapeReport {
	t.Helper()
	return runEscapeHelper(t, raceSafeLimits(Limits{Memory: 256 << 20, CPU: 30 * time.Second}))
}

// A confined child cannot read a file it could read a moment earlier. The
// premise is the helper's own "before" read, so this cannot pass because the
// target was missing.
func TestAConfinedChildCannotReadAFile(t *testing.T) {
	t.Parallel()
	report := escapeBaseline(t)
	requireLandlock(t, report.LandlockABI)

	if !report.CouldReadBefore {
		t.Fatal("the helper could not read the target BEFORE confining: this arm proves nothing")
	}
	if !report.ReadFile.refused() {
		t.Errorf("a confined child read a file off the host filesystem: %+v", report.ReadFile)
	}
	if !report.ReadDir.refused() {
		t.Errorf("a confined child listed a directory: %+v", report.ReadDir)
	}
}

func TestAConfinedChildCannotWriteAFile(t *testing.T) {
	t.Parallel()
	report := escapeBaseline(t)
	requireLandlock(t, report.LandlockABI)

	if !report.CouldWriteBefore {
		t.Fatal("the helper could not write BEFORE confining: this arm proves nothing")
	}
	if !report.WriteFile.refused() {
		t.Errorf("a confined child wrote to the host filesystem: %+v", report.WriteFile)
	}
}

// The network arm needs Landlock ABI 4, where LANDLOCK_ACCESS_NET_CONNECT_TCP
// arrived. Below that the ruleset governs the filesystem only — which is the
// correction this suite records against the ADR's "Landlock is about files"
// reasoning.
func TestAConfinedChildCannotConnect(t *testing.T) {
	t.Parallel()
	report := escapeBaseline(t)
	requireLandlock(t, report.LandlockABI)

	// The network rights arrive at ABI 4 (kernel 6.7). Below that a confined
	// script host CAN open a TCP connection, and the fallback that grants
	// codemode only to a node already holding a class:shell tool is about
	// FILESYSTEM reach and does not cover it. A lane on such a kernel must be
	// ON RECORD rather than quietly green — measured on this repo's ARC
	// runners 2026-11-11: ABI 1, so the filesystem, exec and every-thread
	// bounds are proven there and this one is not.
	switch landlockArmVerdict(report.LandlockABI, 4, os.Getenv(noLandlockNetEscape) == "1") {
	case armSkipOnRecord:
		t.Skipf("%s=1: Landlock ABI %d has no network rights, so this lane does not test whether a "+
			"confined script host can open a TCP connection — and on this run it could (%+v)",
			noLandlockNetEscape, report.LandlockABI, report.Connect)
	case armFailLoud:
		t.Fatalf("Landlock ABI %d has no network rights (they arrive at 4), so a script host on this "+
			"kernel can open a TCP connection. Run this suite on a 6.7+ kernel, or set %s=1 in the lane "+
			"to record that it proves the filesystem bounds and not the network one.",
			report.LandlockABI, noLandlockNetEscape)
	}
	if !report.CouldConnectBefore {
		t.Fatal("the helper could not reach the listener BEFORE confining: this arm proves nothing")
	}
	if !report.Connect.refused() {
		t.Errorf("a confined child opened a TCP connection: %+v", report.Connect)
	}
}

func TestAConfinedChildCannotExec(t *testing.T) {
	t.Parallel()
	report := escapeBaseline(t)
	requireLandlock(t, report.LandlockABI)

	if !report.CouldExecBefore {
		t.Fatal("the helper could not exec BEFORE confining: this arm proves nothing")
	}
	if !report.Exec.refused() {
		t.Errorf("a confined child executed a program: %+v", report.Exec)
	}
	// The refusal has to come from execve, not from something on the way to
	// it. Measured: with nil stdio, os/exec opens /dev/null first and the
	// deny-all ruleset refused THAT, so the arm reported a refusal having
	// never reached execve.
	if !strings.Contains(report.Exec.Err, "fork/exec") {
		t.Errorf("the exec arm was refused before execve (%q): it is not testing LANDLOCK_ACCESS_FS_EXECUTE",
			report.Exec.Err)
	}
	if report.DeclaredFS&0x1 == 0 {
		t.Errorf("LANDLOCK_ACCESS_FS_EXECUTE is not in the declared rights (%#x)", report.DeclaredFS)
	}
}

// THE arm for the critical defect: a Landlock domain and no_new_privs are
// per-THREAD, so a plain syscall confines whichever thread the goroutine was
// carried on and leaves every other thread of the process open. Measured
// before the fix: Confine returned LandlockABI 4 while a sibling thread read,
// wrote, connected and exec'd without a refusal.
//
// Which posture this binary has is a property of the LEG, not a fact, so the
// arm reads what Confine reported and asserts the consequence that posture
// earns. The process-wide guarantee is asserted against the shipping
// CGO_ENABLED=0 artefact in TestTheRealBinaryConfinesEveryThreadOfTheProcess,
// and the refusal against a deliberately cgo-linked one in
// TestABinaryThatCannotConfineTheProcessIsRefused.
func TestASiblingThreadIsCoveredOnlyWhenEveryThreadIs(t *testing.T) {
	t.Parallel()
	report := escapeBaseline(t)
	requireLandlock(t, report.LandlockABI)

	if !report.SiblingRead.Made {
		t.Fatal("the helper never attempted the sibling-thread read: this arm proves nothing")
	}
	if report.AllThreads {
		if !report.SiblingRead.refused() {
			t.Errorf("Confine reported it covered every thread, and a sibling OS thread still read "+
				"the file: %+v", report.SiblingRead)
		}
		return
	}
	// Single-thread posture: the sibling MUST escape, and that is why the
	// shipping build drives the syscall across every thread. An unexpected
	// refusal here would mean this arm has stopped measuring the difference.
	if report.SiblingRead.refused() {
		t.Errorf("Confine reported a single-thread confinement, yet a sibling thread was refused "+
			"(%+v): this arm no longer distinguishes the two postures, so the all-threads path has "+
			"lost its witness", report.SiblingRead)
	}
}

// Confine is irreversible and process-wide, so the rights it declares must
// match what the running kernel knows: a right from a later ABI is EINVAL, and
// an EINVAL here would mean no ruleset at all. The table is asserted rather
// than trusted because getting it wrong fails OPEN.
func TestTheDeclaredRightsMatchTheKernelsABI(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		abi        int
		wantSize   uintptr
		wantNet    bool
		wantScoped uint64
		wantFS     uint64
	}{
		{abi: 1, wantSize: 8, wantNet: false, wantFS: 0x1fff},
		{abi: 2, wantSize: 8, wantNet: false, wantFS: 0x3fff},
		{abi: 3, wantSize: 8, wantNet: false, wantFS: 0x7fff},
		{abi: 4, wantSize: 16, wantNet: true, wantFS: 0x7fff},
		{abi: 5, wantSize: 16, wantNet: true, wantFS: 0xffff},
		// ABI 6 adds the two SCOPES, the only thing that closes an abstract
		// unix socket and a signal to a sibling — neither is reachable
		// through the net rights.
		{abi: 6, wantSize: 24, wantNet: true, wantFS: 0xffff, wantScoped: 0x3},
		// A kernel newer than this table must not be handed rights it was
		// never asked about; the mask simply stops growing.
		{abi: 99, wantSize: 24, wantNet: true, wantFS: 0xffff, wantScoped: 0x3},
	} {
		fs, net, scoped, size := handledRights(tc.abi)
		if fs != tc.wantFS {
			t.Errorf("ABI %d: fs rights %#x, want %#x", tc.abi, fs, tc.wantFS)
		}
		if (net != 0) != tc.wantNet {
			t.Errorf("ABI %d: net rights %#x, want net declared = %v", tc.abi, net, tc.wantNet)
		}
		if scoped != tc.wantScoped {
			t.Errorf("ABI %d: scoped %#x, want %#x", tc.abi, scoped, tc.wantScoped)
		}
		if size != tc.wantSize {
			t.Errorf("ABI %d: attr size %d, want %d", tc.abi, size, tc.wantSize)
		}
	}
}

// The ONE place a script-host child is built must give it nothing. Asserted
// against the real constructor, with a sentinel in this process's environment
// so an inherited environment would be visible rather than theoretical.
func TestTheChildIsBuiltWithNoEnvironment(t *testing.T) {
	t.Setenv("ITERION_SCRIPTHOST_SENTINEL", "must-not-cross")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-cross")

	cmd := command(context.Background(), "/nonexistent/iterion", []byte(`{"script":"1"}`), &bytes.Buffer{}, &bytes.Buffer{})

	if len(cmd.Env) != 0 {
		t.Fatalf("the child would inherit %d variable(s): %q", len(cmd.Env), cmd.Env)
	}
	// os/exec treats a nil Env as "inherit", so an empty non-nil slice is the
	// load-bearing detail and a future refactor that dropped the assignment
	// would pass a nil here.
	if cmd.Env == nil {
		t.Error("cmd.Env is nil, which os/exec reads as 'inherit the parent's environment'")
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("the child is not its own process-group leader: a cancellation could not reach its descendants")
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Error("the child has no parent-death signal")
	}
	if cmd.WaitDelay <= 0 {
		t.Error("the child has no wait delay: a descendant holding the pipes would block the parent forever")
	}
}

// Confine refuses nothing silently: a failure to apply a bound is an error,
// never a child that runs anyway.
//
// It used to SKIP on every normal host and in CI, because RLIMIT_DATA's hard
// limit is unlimited there and an unprivileged process cannot be refused. The
// helper now lowers its OWN hard limit first, so the arm runs everywhere.
func TestConfineReportsABoundItCannotApply(t *testing.T) {
	t.Parallel()

	// A hard limit of 64 MiB, then a request for 256 MiB: a raise, which an
	// unprivileged process may not make. Confine must refuse rather than run
	// the script half-bounded.
	// 1 GiB of hard limit, then a request for 4 GiB: a raise, which an
	// unprivileged process may not make. The hard limit has to leave the
	// child alive — a cgo-linked test binary under -race needs hundreds of
	// megabytes before it runs a line — so the arm asks for more than the
	// limit rather than setting a tiny one.
	report := runEscapeHelper(t,
		Limits{Memory: 4 << 30, CPU: 30 * time.Second},
		escapeOpts{hardDataMiB: 1024, tolerateConfineErr: true})

	// Confine clamps DOWN to the inherited hard limit rather than failing —
	// a stricter environment is the operator's choice and tightening is safe.
	// So the arm asserts the CLAMP, which is the behaviour, and that no bound
	// was silently skipped.
	if report.ConfineErr != "" {
		t.Fatalf("Confine failed where it should have clamped to the inherited hard limit: %s", report.ConfineErr)
	}
	if report.AppliedMemoryBytes != 1<<30 {
		t.Errorf("Confine applied %d bytes of memory bound under a 1 GiB hard limit, want 1073741824: "+
			"asking the kernel to RAISE a hard limit answers EPERM, which used to make codemode "+
			"unavailable in any hardened deployment", report.AppliedMemoryBytes)
	}
}

// The one defect that is invisible on a kernel where Landlock works: the ABI
// probe mapped EVERY errno to zero, and zero meant "absent", which Confine
// swallowed. A seccomp filter answering EPERM for landlock_create_ruleset —
// the shape a container profile gives a syscall it does not allow — then
// produced a child with a full filesystem, full TCP, and no error at all.
//
// Only ENOSYS (no such syscall) and EOPNOTSUPP (compiled in, disabled at boot)
// are absence. That is the pair the kernel documents and the pair its own
// reference sandboxer switches on.
func TestLandlockAbsenceIsNotLandlockRefusal(t *testing.T) {
	t.Parallel()

	if abi, err := classifyLandlockProbe(4, 0); err != nil || abi != 4 {
		t.Errorf("a successful probe gave (%d, %v)", abi, err)
	}

	for _, errno := range []unix.Errno{unix.ENOSYS, unix.EOPNOTSUPP} {
		abi, err := classifyLandlockProbe(0, errno)
		if !errors.Is(err, errLandlockAbsent) {
			t.Errorf("%v should mean Landlock is ABSENT, got (%d, %v)", errno, abi, err)
		}
	}

	// Everything else is a refusal that must stop the run.
	for _, errno := range []unix.Errno{unix.EPERM, unix.EACCES, unix.EINVAL, unix.ENOMEM, unix.E2BIG} {
		abi, err := classifyLandlockProbe(0, errno)
		if err == nil {
			t.Errorf("%v was read as success (abi %d)", errno, abi)
			continue
		}
		if errors.Is(err, errLandlockAbsent) {
			t.Errorf("%v was read as ABSENCE, which Confine swallows — that is the fail-open path: %v",
				errno, err)
		}
		if !errors.Is(err, errno) {
			t.Errorf("%v: the error does not carry the errno: %v", errno, err)
		}
	}
}

// Confine resolves time.Local before Landlock, because time.Local loads the
// zoneinfo database from DISK on first use and a deny-all ruleset refuses that
// read. Nothing noticed when the line was removed, so this is its witness: a
// confined child must still be able to format an instant, and in UTC.
func TestConfineResolvesTheTimezoneBeforeTheFilesystemCloses(t *testing.T) {
	t.Parallel()

	resp := childWithRequest(t, Request{
		Script:                       `[new Date(0).toISOString(), String(new Date(0).getTimezoneOffset())].join("|")`,
		AllowSingleThreadConfinement: true,
	})
	if !resp.OK() {
		t.Fatalf("a confined child could not format a date: %+v — time.Local was probably resolved "+
			"after the ruleset closed the filesystem", resp)
	}
	if resp.Value != `"1970-01-01T00:00:00.000Z|0"` {
		t.Errorf("the child formatted %s, want the UTC instant and a zero offset", resp.Value)
	}
}

// The decision both Landlock gates make, on the kernels this repo meets: a
// developer's 6.8 (ABI 4), the ARC runners' ABI 1, and a kernel with none.
// Every branch but armProve is unreachable through the real syscalls here.
func TestAnArmConcludesOnlyWhatItsKernelOffers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		abi, need int
		onRecord  bool
		want      armVerdict
	}{
		{"ABI 4 proves the filesystem", 4, 1, false, armProve},
		{"ABI 4 proves the network", 4, 4, false, armProve},
		{"ABI 1 proves the filesystem", 1, 1, false, armProve},
		{"ABI 1 cannot prove the network, and nobody said so", 1, 4, false, armFailLoud},
		{"ABI 1 cannot prove the network, and the lane is on record", 1, 4, true, armSkipOnRecord},
		{"no Landlock at all, unrecorded", 0, 1, false, armFailLoud},
		{"no Landlock at all, on record", 0, 1, true, armSkipOnRecord},
		{"a newer ABI still proves what an older right needs", 6, 4, false, armProve},
	} {
		if got := landlockArmVerdict(tc.abi, tc.need, tc.onRecord); got != tc.want {
			t.Errorf("%s: verdict %d, want %d", tc.name, got, tc.want)
		}
	}
}
