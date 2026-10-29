//go:build linux

package proc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// These tests start real processes and stop them. Every process they stop
// carries a token drawn for the test alone, or descends from one that does,
// and each test refuses to sweep when the processes it finds are not the ones
// it started. Run them in a throwaway PID namespace, never on a desktop.

// sweeper is one of the two implementations of the sweep: SweepNodeSession
// here, and the program a sandbox runs.
type sweeper struct {
	name  string
	sweep func(t *testing.T, w *nodeWorld) (SweepReport, error)
	// hold arms the implementation's scan gate: the seam a witness holds a
	// sweep at, to stage what must show between two of its scans.
	hold func(t *testing.T, w *nodeWorld) *scanWindow
}

var sweepers = []sweeper{
	{
		name:  "go",
		sweep: func(_ *testing.T, w *nodeWorld) (SweepReport, error) { return SweepNodeSession(w.token, w.since) },
		hold:  holdGoSweep,
	},
	{
		name:  "sh",
		sweep: func(t *testing.T, w *nodeWorld) (SweepReport, error) { return shSweep(t, w, nil) },
		hold:  holdShSweep,
	},
	// The program's other way to read an environment, for a container whose
	// grep does not read NUL-separated records.
	{
		name: "sh-without-grep-z",
		sweep: func(t *testing.T, w *nodeWorld) (SweepReport, error) {
			return shSweep(t, w, grepWithoutZ(t))
		},
		hold: holdShSweep,
	},
}

// premiseWait bounds how long a witness waits for a fact it needs before it
// gives up — a ceiling, not an expectation: every such wait ends the instant
// the fact is true.
const premiseWait = 30 * time.Second

// awaitPremise holds the test until the fact a witness needs is true, and ends
// the test when it never comes: a premise that did not happen fails as itself,
// never as a verdict on the sweep.
func awaitPremise(t *testing.T, what string, fact func() bool) {
	t.Helper()
	deadline := time.Now().Add(premiseWait)
	for !fact() {
		if time.Now().After(deadline) {
			t.Fatalf("premise, within %s: %s", premiseWait, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitSessionHolds holds the test until a scan of token would find pid: a
// process carries the entry only once its exec is done.
func awaitSessionHolds(t *testing.T, token, name string, pid int) {
	t.Helper()
	awaitPremise(t, fmt.Sprintf("session %s never held %s (pid %d)", token, name, pid), func() bool {
		found, err := FindNodeSession(token)
		if err != nil {
			t.Fatalf("FindNodeSession: %v", err)
		}
		return slices.ContainsFunc(found, func(p SessionProcess) bool { return p.PID == pid })
	})
}

// scanWindow is a sweep held at a scan it has just finished, before it acts on
// what the scan found: a witness that needs a process to show between two
// scans starts it there instead of guessing when the window opens. The two
// implementations are held through their own seam — sweepScanGate here,
// sweepScanGateFile in the program — and read as one window.
type scanWindow struct {
	// held reports whether the sweep is held at a scan, and what that scan
	// found of the session.
	held func() (found int, ok bool)
	// resume lets the sweep act on that scan and look again.
	resume func()
	// passed reports whether the sweep has already LEFT the gate.
	passed func() bool
}

// await holds the test until the sweep is held at a scan and returns what that
// scan found of the session.
func (win *scanWindow) await(t *testing.T) int {
	t.Helper()
	found := 0
	awaitPremise(t, "the sweep never reached the scan gate it is held at", func() bool {
		n, ok := win.held()
		found = n
		return ok
	})
	return found
}

// holdGoSweep holds SweepNodeSession at the first scan it finishes, through the
// package's sweepScanGate seam. The seam is a package variable, so a witness
// that arms it does not run in parallel with another.
func holdGoSweep(t *testing.T, _ *nodeWorld) *scanWindow {
	var found atomic.Int64
	var paused, left atomic.Bool
	resumed := make(chan struct{})
	resume := sync.OnceFunc(func() { close(resumed) })
	// gated is read and written by the sweep's goroutine alone.
	gated := false
	sweepScanGate = func(n int) {
		if gated {
			return
		}
		gated = true
		found.Store(int64(n))
		paused.Store(true)
		<-resumed
		left.Store(true)
	}
	// The cleanup order is load-bearing: LIFO runs the sweep's join (which
	// resumes and waits for it to return) before this nil, so the sweep never
	// reads the seam after it is cleared.
	t.Cleanup(func() { resume(); sweepScanGate = nil })
	return &scanWindow{
		held: func() (int, bool) {
			if !paused.Load() {
				return 0, false
			}
			return int(found.Load()), true
		},
		resume: resume,
		passed: left.Load,
	}
}

// holdShSweep holds the sweep program at the first scan it finishes, through
// the gate file it reports its scans to.
// nextGateID numbers the scan gates: a world that holds a second sweep must
// not find the first one's release or scan log on its own gate path.
var nextGateID atomic.Uint64

func holdShSweep(t *testing.T, w *nodeWorld) *scanWindow {
	gate := filepath.Join(w.dir, "gate-"+strconv.FormatUint(nextGateID.Add(1), 10))
	sweepScanGateFile = gate
	resume := sync.OnceFunc(func() {
		if err := os.WriteFile(gate+".resume", nil, 0o600); err != nil {
			t.Error(err)
		}
	})
	// The cleanup order is load-bearing, as holdGoSweep's: the join resumes
	// and waits before the path is cleared.
	t.Cleanup(func() { resume(); sweepScanGateFile = "" })
	return &scanWindow{
		held: func() (int, bool) {
			scanned, err := os.ReadFile(gate + ".scanned")
			if err != nil {
				return 0, false
			}
			// A line the program is still writing is no scan yet.
			first, _, whole := strings.Cut(string(scanned), "\n")
			if !whole {
				return 0, false
			}
			return len(strings.Fields(first)), true
		},
		resume: resume,
		passed: func() bool {
			_, err := os.Stat(gate + ".passed")
			return err == nil
		},
	}
}

// shSweep runs the sweep program the way the sandbox exec runs it — its own
// environment does not carry the token — with env added to this process's.
func shSweep(t *testing.T, w *nodeWorld, env []string) (SweepReport, error) {
	t.Helper()
	sinceFile := filepath.Join(w.dir, "since")
	if err := os.WriteFile(sinceFile, []byte(strconv.FormatUint(uint64(w.since), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := NodeSessionSweepScript(w.token, sinceFile)
	if err != nil {
		t.Fatalf("NodeSessionSweepScript: %v", err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return SweepReport{}, fmt.Errorf("sweep program: %v\n%s", err, out)
	}
	return ParseNodeSessionSweep(string(out))
}

// grepWithoutZ returns the environment that puts first on PATH a grep which
// refuses -z, the way a grep that does not read NUL-separated records does.
func grepWithoutZ(t *testing.T) []string {
	t.Helper()
	real, err := exec.LookPath("grep")
	if err != nil {
		t.Fatalf("no grep here: %v", err)
	}
	dir := t.TempDir()
	wrapper := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in -*z*) exit 2;; esac; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "grep"), []byte(wrapper), 0o755); err != nil { // #nosec G306 — the wrapper must be executable
		t.Fatal(err)
	}
	return []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
}

// nodeWorld is a set of processes started for one test: those of the session,
// which a sweep must stop, and controls it must spare.
type nodeWorld struct {
	t              *testing.T
	token          string
	since          BootTicks
	dir            string
	marked         map[string]int // label → pid
	controls       map[string]int
	children       []*exec.Cmd
	afterGuardScan func([]SessionProcess)
}

func newNodeWorld(t *testing.T) *nodeWorld {
	t.Helper()
	since, err := BootTicksNow()
	if err != nil {
		t.Fatal(err)
	}
	w := &nodeWorld{
		t:        t,
		token:    NewNodeSessionToken(),
		since:    since,
		dir:      t.TempDir(),
		marked:   map[string]int{},
		controls: map[string]int{},
	}
	t.Cleanup(w.stopAll)
	return w
}

func (w *nodeWorld) entry() string { return NodeSessionEntry(w.token) }

// start runs argv with the extra environment entries, waits for the pid of the
// process it leaves behind when it does not stay in the foreground (written to
// the file named by its last argument), and returns that pid.
func (w *nodeWorld) start(name string, env []string, foreground, setsid bool, argv ...string) int {
	w.t.Helper()
	pidFile := filepath.Join(w.dir, name+".pid")
	cmd := exec.Command(argv[0], append(argv[1:], pidFile)...)
	cmd.Env = append(os.Environ(), env...)
	if setsid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		w.t.Fatalf("start %s: %v", name, err)
	}
	w.children = append(w.children, cmd)
	if foreground {
		return cmd.Process.Pid
	}
	pid := w.waitPid(name, pidFile)
	_ = cmd.Wait() // the launcher exits; what it left is reparented
	return pid
}

func (w *nodeWorld) sh(name string, env []string, foreground, setsid bool, script string) int {
	w.t.Helper()
	return w.start(name, env, foreground, setsid, "sh", "-c", script, "sh")
}

func (w *nodeWorld) waitPid(name, pidFile string) int {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				w.t.Fatalf("%s: unreadable pid file %q", name, b)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.t.Fatalf("%s never wrote its pid", name)
	return 0
}

// awaitSeeded holds the test until every process this world marked is visible
// to a scan: a foreground seed returns the pid of a launcher whose exec may
// still be running, and a process carries the entry only once its exec is
// done. A witness whose premise is that the sweep sees its seeds awaits them
// here instead of betting on that exec.
func (w *nodeWorld) awaitSeeded() {
	w.t.Helper()
	for name, pid := range w.marked {
		awaitSessionHolds(w.t, w.token, name, pid)
	}
}

// populate starts what a node can leave behind and the controls a sweep must
// spare, and leaves every marked process visible to a scan.
func (w *nodeWorld) populate() {
	mark := []string{w.entry()}
	// A command the agent ran in the foreground, still running.
	w.marked["direct"] = w.sh("direct", mark, true, false, `exec sleep 300`)
	// A server started with nohup … & by a shell that has since exited.
	w.marked["nohup"] = w.sh("nohup", mark, false, false, `nohup sleep 300 >/dev/null 2>&1 & echo $! > "$1"`)
	// A daemon in its own session, reparented once its launcher exited.
	w.marked["setsid"] = w.sh("setsid", mark, false, true, `sleep 300 >/dev/null 2>&1 & echo $! > "$1"`)
	// A process that ignores TERM: only KILL stops it.
	w.marked["ignores-term"] = w.sh("ignores-term", mark, true, false, `trap "" TERM; exec sleep 300`)
	// A supervisor that, on TERM, forks a replacement and exits: the
	// replacement inherits the entry after the first scan.
	w.marked["respawner"] = w.sh("respawner", mark, true, false,
		`trap 'nohup sleep 301 >/dev/null 2>&1 & echo $! > "$1.child"; exit 0' TERM; while :; do sleep 1; done`)
	// A server whose worker rewrites its title over its environment, as a
	// database backend does: the worker no longer carries the entry, its
	// parent does.
	if _, err := exec.LookPath("perl"); err == nil {
		w.marked["server"] = w.sh("server", mark, true, false,
			`perl -e '$0 = "postgres: backend"; sleep 300' & echo $! > "$1.worker"; exec sleep 300`)
		w.marked["titled worker"] = w.waitPid("titled worker", filepath.Join(w.dir, "server.pid.worker"))
	} else {
		w.t.Log("no perl here: the title-rewriting worker is not exercised")
	}

	// A supervisor that named itself the way `echo name > /proc/self/comm`
	// does, with a trailing newline: its stat file spans two lines.
	w.marked["named with a newline"] = w.sh("named with a newline", mark, true, false,
		`echo watchdog > /proc/self/comm; while :; do sleep 300; done`)
	// A process that, on TERM, renames itself with a newline and keeps
	// running: it is still the process the sweep found, and KILL stops it.
	w.marked["renames on TERM"] = w.sh("renames on TERM", mark, true, false,
		`trap 'printf "x\ny" > /proc/self/comm' TERM; while :; do sleep 1; done`)

	w.controls["unmarked"] = w.sh("unmarked", nil, true, false, `exec sleep 300`)
	// A process of no session whose name forges, on the first line of its
	// stat file, a process of the session as its parent.
	w.controls["forged parent"] = w.sh("forged parent", nil, true, false,
		fmt.Sprintf(`printf 'x) S %d 1\n' > /proc/self/comm; while :; do sleep 300; done`, w.marked["direct"]))
	w.controls["longer name"] = w.sh("longer name", []string{NodeSessionEntry(w.token + "x")}, true, false, `exec sleep 300`)
	w.controls["longer value"] = w.sh("longer value", []string{NodeSessionVar(w.token) + "=11"}, true, false, `exec sleep 300`)
	w.controls["entry inside another variable"] = w.sh("entry inside another variable", []string{"OTHER=" + w.entry()}, true, false, `exec sleep 300`)
	w.controls["entry after a newline"] = w.sh("entry after a newline", []string{"OTHER=value\n" + w.entry()}, true, false, `exec sleep 300`)
	w.controls["another node"] = w.sh("another node", []string{NodeSessionEntry(NewNodeSessionToken())}, true, false, `exec sleep 300`)

	w.awaitSeeded()
}

// guardFound stops the test before any signal unless the session found is the
// processes this test marked and their descendants, every marked one included.
func (w *nodeWorld) guardFound() {
	w.t.Helper()
	found, err := FindNodeSession(w.token)
	if err != nil {
		w.t.Fatalf("FindNodeSession: %v", err)
	}
	if w.afterGuardScan != nil {
		w.afterGuardScan(found)
	}
	ours := map[int]bool{}
	for _, pid := range w.marked {
		ours[pid] = true
	}
	got := map[int]bool{}
	for _, p := range found {
		got[p.PID] = true
		if !ours[p.PID] && unknownProcessIsRunning(p.PID, ours) {
			w.t.Fatalf("refusing to sweep: the session holds pid %d (%s), which this test did not start", p.PID, p.Name)
		}
	}
	var missed []string
	for name, pid := range w.marked {
		if !got[pid] {
			missed = append(missed, fmt.Sprintf("%s (pid %d)", name, pid))
		}
	}
	if len(missed) > 0 {
		slices.Sort(missed)
		w.t.Fatalf("refusing to sweep: the session misses %v", missed)
	}
}

func descendsFrom(pid int, ancestors map[int]bool) bool {
	for q, n := pid, 0; q > 1 && n < 64; n++ {
		st, ok := readStat("/proc/" + strconv.Itoa(q))
		if !ok {
			return false
		}
		if ancestors[st.ppid] {
			return true
		}
		q = st.ppid
	}
	return false
}

func unknownProcessIsRunning(pid int, ancestors map[int]bool) bool {
	return !descendsFrom(pid, ancestors) && isRunning(pid)
}

// check asserts the outcome: every marked process — and the respawner's
// replacement — stopped and reported, nothing surviving or unreadable, every
// control running and unreported.
func (w *nodeWorld) check(r SweepReport, err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatalf("sweep: %v", err)
	}
	// A survivor is a member: the test's own. An unreadable process is anyone's
	// of this uid started meanwhile — in a shared PID namespace, a CI runner
	// compiling other packages, one of them is unreadable for the instant of
	// its exec — and is the test's business only if the test started it.
	if len(r.Survivors) > 0 {
		w.t.Errorf("the sweep left survivors: %+v", r.Survivors)
	}
	ours := map[int]string{}
	for name, pid := range w.marked {
		ours[pid] = name
	}
	for name, pid := range w.controls {
		ours[pid] = name
	}
	for _, p := range r.Unreadable {
		if name, ok := ours[p.PID]; ok {
			w.t.Errorf("%s (pid %d) was reported unreadable", name, p.PID)
		} else {
			w.t.Logf("an unreadable process this test did not start: %s (pid %d)", p.Name, p.PID)
		}
	}
	reported := map[int]bool{}
	for _, p := range r.Stopped {
		reported[p.PID] = true
	}
	if b, err := os.ReadFile(filepath.Join(w.dir, "respawner.pid.child")); err == nil {
		child, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		w.marked["respawner's replacement"] = child
	} else if _, ok := w.marked["respawner"]; ok {
		w.t.Errorf("the respawner never forked its replacement: TERM did not reach it")
	}
	waitGone := time.Now().Add(5 * time.Second)
	for name, pid := range w.marked {
		for isRunning(pid) && time.Now().Before(waitGone) {
			time.Sleep(50 * time.Millisecond)
		}
		if isRunning(pid) {
			w.t.Errorf("%s (pid %d) belongs to the session and is still running", name, pid)
		}
		if !reported[pid] {
			w.t.Errorf("%s (pid %d) was stopped but not reported", name, pid)
		}
	}
	for name, pid := range w.controls {
		if !isRunning(pid) {
			w.t.Errorf("control %s (pid %d) is not of the session and was stopped", name, pid)
		}
		if reported[pid] {
			w.t.Errorf("control %s (pid %d) was reported", name, pid)
		}
	}
}

func isRunning(pid int) bool {
	dir := "/proc/" + strconv.Itoa(pid)
	st, ok := readStat(dir)
	return ok && running(dir, st)
}

func (w *nodeWorld) stopAll() {
	for _, pid := range w.controls {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	for _, pid := range w.marked {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	for _, cmd := range w.children {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

// sweepInBackground runs the sweep while the test stages the window the sweep
// must see. The returned function joins it and answers what it did; the join
// also runs when the test ends, with the hold released, so a held sweep never
// outlives the test holding it.
func (w *nodeWorld) sweepInBackground(s sweeper, win *scanWindow) func() (SweepReport, error) {
	w.t.Helper()
	type outcome struct {
		r   SweepReport
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := s.sweep(w.t, w)
		done <- outcome{r, err}
	}()
	var got outcome
	join := sync.OnceFunc(func() {
		win.resume()
		select {
		case got = <-done:
		case <-time.After(premiseWait):
			w.t.Errorf("the sweep did not return within %s of its hold being released", premiseWait)
		}
	})
	w.t.Cleanup(join)
	return func() (SweepReport, error) { join(); return got.r, got.err }
}

func TestSweepStopsTheSessionAndNothingElse(t *testing.T) {
	for _, s := range sweepers {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			w := newNodeWorld(t)
			w.populate()
			w.awaitSeeded()
			w.guardFound()
			w.check(s.sweep(t, w))
		})
	}
}

func TestGuardFoundIgnoresOnlyAnUnknownProcessThatHasLeft(t *testing.T) {
	w := newNodeWorld(t)
	w.marked["parent"] = w.sh("parent", []string{w.entry()}, true, false,
		`sleep 300 & echo $! > "$1.child"; wait; exec sleep 300`)
	child := w.waitPid("snapshot child", filepath.Join(w.dir, "parent.pid.child"))
	w.controls["snapshot child"] = child
	w.awaitSeeded()
	awaitSessionHolds(t, w.token, "snapshot child", child)
	staged := false
	w.afterGuardScan = func(found []SessionProcess) {
		if !slices.ContainsFunc(found, func(p SessionProcess) bool { return p.PID == child }) {
			t.Fatalf("child %d was absent from guardFound's snapshot", child)
		}
		if err := syscall.Kill(child, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		awaitPremise(t, "the snapshot child was never reaped", func() bool {
			_, ok := readStat("/proc/" + strconv.Itoa(child))
			return !ok
		})
		staged = true
	}
	w.guardFound()
	if !staged {
		t.Fatal("guardFound returned without staging the snapshot child")
	}
}

// intruderPIDLine is how the helper half below tells its parent which process
// it started, so the parent asserts the guard refused THAT process and not a
// stranger that happened to be running on the shard.
var intruderPIDLine = regexp.MustCompile(`intruder pid (\d+)`)

func TestGuardFoundRefusesALiveIntruder(t *testing.T) {
	const helper = "ITERION_TEST_GUARD_FOUND_LIVE_INTRUDER"
	if os.Getenv(helper) == "1" {
		w := newNodeWorld(t)
		intruder := w.sh("intruder", []string{w.entry()}, true, false, `exec sleep 300`)
		fmt.Printf("intruder pid %d\n", intruder)
		awaitSessionHolds(t, w.token, "intruder", intruder)
		// The verdict is read off a scan that PROVABLY held the intruder,
		// alive, at the instant the guard judged it: a scan that missed it
		// judged nothing, and a scan CAN miss it — a process in the middle of
		// its exec takes its /proc/<pid>/environ with it for an instant, which
		// lands it in the scan's unreadable set instead of the session, and a
		// loaded shard widens that window. So the premise is awaited at the
		// guard's own seam and the scan is taken again when it was not met,
		// instead of reading the guard's silence as acceptance: load changes
		// how long this half takes, never what it decides.
		judged := false
		w.afterGuardScan = func(found []SessionProcess) {
			judged = slices.ContainsFunc(found, func(p SessionProcess) bool { return p.PID == intruder }) &&
				isRunning(intruder)
		}
		awaitPremise(t, fmt.Sprintf("no scan of session %s held the intruder (pid %d) alive for the guard to judge", w.token, intruder), func() bool {
			w.guardFound() // refuses — and ends this helper — when it judges that scan
			return judged
		})
		// Reached only when the guard ACCEPTED an intruder the scan it judged
		// held alive: the helper exits 0, which is what the half below reads.
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestGuardFoundRefusesALiveIntruder$")
	cmd.Env = append(os.Environ(), helper+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("guardFound accepted a live process this test did not start")
	}
	m := intruderPIDLine.FindSubmatch(out)
	if m == nil {
		t.Fatalf("the live-intruder helper failed before it started its intruder: %v\n%s", err, out)
	}
	refused := fmt.Sprintf("the session holds pid %s", m[1])
	if !strings.Contains(string(out), "which this test did not start") || !strings.Contains(string(out), refused) {
		t.Fatalf("the helper failed for another reason, or refused a process other than the intruder it started (%q): %v\n%s", refused, err, out)
	}
}

// A process that shows after a scan found nothing — the way a child in the
// middle of its exec is missed for an instant — is stopped. The window is
// staged, not awaited: the sweep is held at a scan it has finished, `late`
// starts and joins the session there, and only then does the sweep act on that
// scan. Both facts the witness rests on are asserted — the held scan found
// nothing, read from the sweep's own view, and `late` is in the session before
// the sweep looks again — so a window that did not open fails as itself.
//
// Not parallel: the holds arm a package seam.
func TestSweepStopsAProcessThatShowsAfterAnEmptyScan(t *testing.T) {
	for _, s := range sweepers {
		t.Run(s.name, func(t *testing.T) {
			w := newNodeWorld(t)
			win := s.hold(t, w)
			sweep := w.sweepInBackground(s, win)
			if found := win.await(t); found != 0 {
				t.Fatalf("premise: the scan the sweep is held at found %d process(es) of the session, want a scan that found nothing", found)
			}
			w.marked["late"] = w.sh("late", []string{w.entry()}, true, false, `exec sleep 300`)
			awaitSessionHolds(t, w.token, "late", w.marked["late"])
			if win.passed() {
				t.Fatal("premise: the sweep left the scan gate before `late` joined the session: the window was not staged")
			}
			win.resume()
			awaitPremise(t, "the gate never recorded the sweep passing", win.passed)
			w.check(sweep())
		})
	}
}

// With no time left, a sweep still signals what it found before it gives up:
// a deadline that passes during a scan must not leave the scan's finds alone.
// The deadlines here are already past when the sweep starts — the program
// counts in whole seconds, so a zero one would pass only at the next second.
func TestSweepSignalsBeforeItsDeadline(t *testing.T) {
	grace, kill := sweepGrace, sweepKillWait
	sweepGrace, sweepKillWait = -time.Second, -time.Second
	t.Cleanup(func() { sweepGrace, sweepKillWait = grace, kill })
	for _, s := range sweepers {
		t.Run(s.name, func(t *testing.T) {
			w := newNodeWorld(t)
			mark := []string{w.entry()}
			w.marked["plain"] = w.sh("plain", mark, true, false, `exec sleep 300`)
			w.marked["ignores-term"] = w.sh("ignores-term", mark, true, false, `trap "" TERM; exec sleep 300`)
			w.awaitSeeded()
			w.check(s.sweep(t, w))
		})
	}
}

// helper compiles a small C program, or skips the test when no compiler is
// at hand.
func helper(t *testing.T, name, src string, flags ...string) string {
	t.Helper()
	cc, err := exec.LookPath("gcc")
	if err != nil {
		t.Skipf("no gcc here: %s is not exercised", name)
	}
	dir := t.TempDir()
	c := filepath.Join(dir, name+".c")
	if err := os.WriteFile(c, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, name)
	if out, err := exec.Command(cc, append([]string{"-O0", "-o", bin, c}, flags...)...).CombinedOutput(); err != nil {
		t.Fatalf("compile %s: %v\n%s", name, err, out)
	}
	return bin
}

// A daemon whose leader thread exited while a worker thread runs: its
// process-wide environment cannot be read, its worker's can.
func TestSweepReadsTheEnvironmentOfALeaderlessProcess(t *testing.T) {
	bin := helper(t, "leaderless", `#include <pthread.h>
#include <unistd.h>
static void *run(void *a) { (void)a; sleep(300); return 0; }
int main(void) { pthread_t t; pthread_create(&t, 0, run, 0); pthread_exit(0); }
`, "-pthread")
	for _, s := range sweepers {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			w := newNodeWorld(t)
			w.marked["leaderless"] = w.start("leaderless", []string{w.entry()}, true, false, bin)
			dir := "/proc/" + strconv.Itoa(w.marked["leaderless"])
			awaitPremise(t, "the leaderless process never lost the environment its leader held", func() bool {
				if _, err := os.ReadFile(dir + "/environ"); err == nil {
					return false // the leader is still there to answer
				}
				_, throughAThread := readEnviron(dir)
				return throughAThread
			})
			w.awaitSeeded()
			w.check(s.sweep(t, w))
		})
	}
}

// awaitUndumpable holds the test until pid runs with an environment nobody of
// this uid can read: its prctl(PR_SET_DUMPABLE, 0) has run.
func awaitUndumpable(t *testing.T, name string, pid int) {
	t.Helper()
	awaitPremise(t, name+" never made its environment unreadable", func() bool {
		_, readable := readEnviron("/proc/" + strconv.Itoa(pid))
		return !readable && isRunning(pid)
	})
}

// A process that made itself non-dumpable, as a setuid program or a hardened
// daemon does: its environment cannot be read by anyone of this uid. Under a
// process of the session it is stopped as a descendant; alone, it is reported
// as unreadable, never counted as clean.
func TestSweepNamesWhatItCannotRead(t *testing.T) {
	bin := helper(t, "undumpable", `#include <sys/prctl.h>
#include <unistd.h>
int main(void) { prctl(PR_SET_DUMPABLE, 0, 0, 0, 0); sleep(300); return 0; }
`)
	// Not parallel: an unreadable process is reported whichever test started
	// it, so these run while no other test of the package does.
	for _, s := range sweepers {
		t.Run(s.name+"/under the session", func(t *testing.T) {
			w := newNodeWorld(t)
			w.marked["parent"] = w.start("parent", []string{w.entry()}, true, false,
				"sh", "-c", `"$0" & echo $! > "$1.child"; exec sleep 300`, bin)
			w.marked["undumpable child"] = w.waitPid("undumpable child", filepath.Join(w.dir, "parent.pid.child"))
			awaitUndumpable(t, "the undumpable child", w.marked["undumpable child"])
			w.awaitSeeded()
			w.check(s.sweep(t, w))
		})
		t.Run(s.name+"/alone", func(t *testing.T) {
			w := newNodeWorld(t)
			pid := w.start("undumpable", []string{w.entry()}, true, false, bin)
			awaitUndumpable(t, "the undumpable process", pid)
			w.controls["undumpable"] = pid
			r, err := s.sweep(t, w)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if n := len(slices.DeleteFunc(slices.Clone(r.Unreadable), func(p SessionProcess) bool { return p.PID != pid })); n != 1 {
				t.Errorf("a process whose environment cannot be read (pid %d) was reported %d time(s), want once: %+v", pid, n, r)
			}
			if slices.ContainsFunc(r.Stopped, func(p SessionProcess) bool { return p.PID == pid }) {
				t.Errorf("a process nobody could read (pid %d) was stopped on a guess", pid)
			}
		})
	}
}

// The variables of nested delegations accumulate: a process of an inner
// delegation belongs to the outer one too.
func TestNestedSessionsAccumulate(t *testing.T) {
	outer, inner := NewNodeSessionToken(), NewNodeSessionToken()
	if NodeSessionVar(outer) == NodeSessionVar(inner) {
		t.Fatal("two tokens share one variable")
	}
	w := newNodeWorld(t)
	pid := w.sh("nested", []string{NodeSessionEntry(outer), NodeSessionEntry(inner)}, true, false, `exec sleep 300`)
	w.controls["nested"] = pid
	// The process is found once its exec is done: until then its environment
	// reads empty.
	awaitSessionHolds(t, outer, "nested", pid)
	awaitSessionHolds(t, inner, "nested", pid)
}

// A gate nobody releases ends the program on its own error line: a held sweep
// must never parse as a sweep that found nothing. The bound here is short, so
// the test pays seconds, not the production default.
func TestSweepProgram_aGateNobodyReleasesIsAnError(t *testing.T) {
	w := newNodeWorld(t)
	gateFile := filepath.Join(w.dir, "gate-error")
	sweepScanGateFile = gateFile
	oldWait := sweepGateWaitSeconds
	sweepGateWaitSeconds = 2
	t.Cleanup(func() { sweepScanGateFile = ""; sweepGateWaitSeconds = oldWait })
	sinceFile := filepath.Join(w.dir, "since")
	if err := os.WriteFile(sinceFile, []byte(strconv.FormatUint(uint64(w.since), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := NodeSessionSweepScript(w.token, sinceFile)
	if err != nil {
		t.Fatalf("NodeSessionSweepScript: %v", err)
	}
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sweep program: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "error the scan gate at "+gateFile+" was never released") {
		t.Errorf("a gate nobody releases answered no release-timeout error:\n%s", out)
	}
	if _, err := ParseNodeSessionSweep(string(out)); err == nil {
		t.Errorf("a sweep the gate killed parsed as a report:\n%s", out)
	}
}

// The scan gate's bound lives inside the witness's ceiling, so a held sweep
// fails its own gate before the witness that holds it gives up on it.
func TestSweepGateWaitIsInsideTheWitnessCeiling(t *testing.T) {
	if sweepGateWaitSeconds >= int(premiseWait/time.Second) {
		t.Fatalf("sweepGateWaitSeconds = %d is not under the witness ceiling %d", sweepGateWaitSeconds, int(premiseWait/time.Second))
	}
}

// Two sweeps of one world hold at their own gate: the second hold's path is
// the one the next program renders, never the first hold's.
func TestSweepGatesArePerSweep(t *testing.T) {
	w := newNodeWorld(t)
	first := holdShSweep(t, w)
	firstPath := sweepScanGateFile
	rendered := func() string {
		sinceFile := filepath.Join(w.dir, "since")
		if err := os.WriteFile(sinceFile, []byte(strconv.FormatUint(uint64(w.since), 10)), 0o600); err != nil {
			t.Fatal(err)
		}
		script, err := NodeSessionSweepScript(w.token, sinceFile)
		if err != nil {
			t.Fatalf("NodeSessionSweepScript: %v", err)
		}
		return script
	}
	if !strings.Contains(rendered(), "GATE='"+firstPath+"'") {
		t.Errorf("the first render does not gate at the first sweep's path %s", firstPath)
	}
	second := holdShSweep(t, w)
	secondPath := sweepScanGateFile
	if secondPath == firstPath {
		t.Fatalf("two sweeps of one world hold at one gate path %s", firstPath)
	}
	if !strings.Contains(rendered(), "GATE='"+secondPath+"'") {
		t.Errorf("the second render does not gate at the second sweep's path %s", secondPath)
	}
	if strings.Contains(rendered(), "GATE='"+firstPath+"'") {
		t.Errorf("the second render still gates at the first sweep's path %s", firstPath)
	}
	first.resume()
	second.resume()
}

func TestSweepWithNothingLeftStopsNothing(t *testing.T) {
	t.Parallel()
	r, err := SweepNodeSession(NewNodeSessionToken(), 0)
	if err != nil || len(r.Stopped)+len(r.Survivors) != 0 {
		t.Fatalf("SweepNodeSession on an unused token = %+v, %v; want nothing", r, err)
	}
}

func TestNodeSessionTokenRefusesWhatAShellOrAMatcherCouldMisread(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "short", "with space-1234", "quote'1234567", "UPPERCASE123", "semi;colon1234", "x*2345678"} {
		if _, err := NodeSessionSweepScript(bad, ""); err == nil {
			t.Errorf("NodeSessionSweepScript(%q) accepted the token", bad)
		}
		if _, err := FindNodeSession(bad); err == nil {
			t.Errorf("FindNodeSession(%q) accepted the token", bad)
		}
	}
	if _, err := NodeSessionSweepScript(NewNodeSessionToken(), "/tmp/it's"); err == nil {
		t.Errorf("a since file holding a quote was accepted")
	}
	for i := 0; i < 50; i++ {
		tok := NewNodeSessionToken()
		if checkNodeSessionToken(tok) != nil {
			t.Fatalf("NewNodeSessionToken drew %q, which checkNodeSessionToken refuses", tok)
		}
		if v := NodeSessionVar(tok); strings.ContainsAny(v, "-=") {
			t.Fatalf("NodeSessionVar(%q) = %q is no shell name", tok, v)
		}
	}
	// A delegate mark is a token too: node id, iteration, random suffix.
	mark := "-p2-campaign---3-" + NewNodeSessionToken()
	if err := checkNodeSessionToken(mark); err != nil {
		t.Errorf("a mark with a leading dash is refused: %v", err)
	}
	if strings.Contains(NodeSessionVar(mark), "-") {
		t.Errorf("NodeSessionVar(%q) keeps a dash", mark)
	}
}

func TestParseNodeSessionSweep(t *testing.T) {
	t.Parallel()
	out := sweepLineStopped + "41 java\n" +
		"noise from the container\n" +
		sweepLineSurvivor + "42 mysqld\n" +
		sweepLineUnreadable + "43 \n" +
		sweepLineDone + "\n"
	r, err := ParseNodeSessionSweep(out)
	if err != nil {
		t.Fatalf("ParseNodeSessionSweep: %v", err)
	}
	want := SweepReport{
		Stopped:    []SessionProcess{{41, "java"}},
		Survivors:  []SessionProcess{{42, "mysqld"}},
		Unreadable: []SessionProcess{{43, "?"}},
	}
	if fmt.Sprint(r) != fmt.Sprint(want) {
		t.Fatalf("report = %+v, want %+v", r, want)
	}
	if p := r.Problems(); p == nil || !strings.Contains(p.Error(), "still running after KILL: mysqld (pid 42)") ||
		!strings.Contains(p.Error(), "may carry the token: ? (pid 43)") {
		t.Fatalf("Problems() = %v", p)
	}
	if labels := r.Labels(); len(labels) != 1 || !strings.Contains(labels[0], "process java (pid 41)") {
		t.Fatalf("Labels() = %q, want the stopped process only", labels)
	}
	if _, err := ParseNodeSessionSweep(sweepLineError + "no readable /proc in this container\n"); err == nil ||
		!strings.Contains(err.Error(), "no readable /proc") {
		t.Fatalf("an error line must surface as an error, got %v", err)
	}
	if _, err := ParseNodeSessionSweep(sweepLineStopped + "41 java\n"); err == nil || !strings.Contains(err.Error(), "before its last line") {
		t.Fatalf("a report without its last line must surface as an error, got %v", err)
	}
	if _, err := ParseNodeSessionSweep(sweepLineStopped + "notapid java\n" + sweepLineDone + "\n"); err == nil {
		t.Fatalf("an unreadable stopped line must surface as an error")
	}
}

// A sweep that dies on its way says so: its last line is missing, and what it
// leaves is an error, never a clean "nothing found".
func TestADeadSweepIsNotReportedAsClean(t *testing.T) {
	w := newNodeWorld(t)
	w.marked["plain"] = w.sh("plain", []string{w.entry()}, true, false, `exec sleep 300`)
	dir := t.TempDir()
	// The program's first pause kills it.
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/bin/sh\nkill -KILL $PPID\n"), 0o755); err != nil { // #nosec G306 — the shim must be executable
		t.Fatal(err)
	}
	script, err := NodeSessionSweepScript(w.token, "")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("scenario broken: the program was not killed: %q", out)
	}
	if _, err := ParseNodeSessionSweep(string(out)); err == nil {
		t.Errorf("a sweep killed on its way parsed as complete: %q", out)
	}
}

// A since file that is no regular file — a FIFO left at its path — does not
// hold the sweep.
func TestSweepDoesNotWaitOnASinceFileThatIsNoFile(t *testing.T) {
	w := newNodeWorld(t)
	w.marked["plain"] = w.sh("plain", []string{w.entry()}, true, false, `exec sleep 300`)
	w.awaitSeeded()
	fifo := filepath.Join(w.dir, "since")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := NodeSessionSweepScript(w.token, fifo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "sh", "-c", script).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the sweep waited on a FIFO at its since file: %q", out)
	}
	w.check(ParseNodeSessionSweep(string(out)))
}

// The variable's name is the prefix and hex alone, whatever the token spells:
// a sandbox mark carries a node's id, and a filter that drops variables named
// after a credential (KEY, TOKEN, SECRET) must find no such word in it.
func TestNodeSessionVarSpellsNoWordAFilterDrops(t *testing.T) {
	t.Parallel()
	name := regexp.MustCompile(`^` + NodeSessionPrefix + `[0-9a-f]{32}$`)
	for _, tok := range []string{
		NewNodeSessionToken(),
		"rotate-api-key-0-" + NewNodeSessionToken(),
		"refresh-token-3-" + NewNodeSessionToken(),
		"read-secret-1-" + NewNodeSessionToken(),
	} {
		v := NodeSessionVar(tok)
		if !name.MatchString(v) {
			t.Errorf("NodeSessionVar(%q) = %q, want the prefix and 32 hex digits", tok, v)
		}
		for _, word := range []string{"KEY", "TOKEN", "SECRET"} {
			if strings.Contains(strings.ToUpper(strings.TrimPrefix(v, NodeSessionPrefix)), word) {
				t.Errorf("NodeSessionVar(%q) = %q spells %s", tok, v, word)
			}
		}
	}
	if NodeSessionVar("a-token-one") == NodeSessionVar("a-token-two") {
		t.Error("two tokens share one variable")
	}
}
