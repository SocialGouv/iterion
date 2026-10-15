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
}

var sweepers = []sweeper{
	{"go", func(_ *testing.T, w *nodeWorld) (SweepReport, error) { return SweepNodeSession(w.token, w.since) }},
	{"sh", func(t *testing.T, w *nodeWorld) (SweepReport, error) { return shSweep(t, w, nil) }},
	// The program's other way to read an environment, for a container whose
	// grep does not read NUL-separated records.
	{"sh-without-grep-z", func(t *testing.T, w *nodeWorld) (SweepReport, error) {
		return shSweep(t, w, grepWithoutZ(t))
	}},
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
	t        *testing.T
	token    string
	since    BootTicks
	dir      string
	marked   map[string]int // label → pid
	controls map[string]int
	children []*exec.Cmd
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

// populate starts what a node can leave behind and the controls a sweep must
// spare.
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
}

// guardFound stops the test before any signal unless the session found is the
// processes this test marked and their descendants, every marked one included.
func (w *nodeWorld) guardFound() {
	w.t.Helper()
	found, err := FindNodeSession(w.token)
	if err != nil {
		w.t.Fatalf("FindNodeSession: %v", err)
	}
	ours := map[int]bool{}
	for _, pid := range w.marked {
		ours[pid] = true
	}
	got := map[int]bool{}
	for _, p := range found {
		got[p.PID] = true
		if !ours[p.PID] && !descendsFrom(p.PID, ours) {
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

func TestSweepStopsTheSessionAndNothingElse(t *testing.T) {
	for _, s := range sweepers {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			w := newNodeWorld(t)
			w.populate()
			w.guardFound()
			w.check(s.sweep(t, w))
		})
	}
}

// A process that shows after a scan found nothing — the way a child in the
// middle of its exec is missed for an instant — is stopped.
func TestSweepStopsAProcessThatShowsAfterAnEmptyScan(t *testing.T) {
	for _, s := range sweepers {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			w := newNodeWorld(t)
			type outcome struct {
				r   SweepReport
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				r, err := s.sweep(t, w)
				done <- outcome{r, err}
			}()
			time.Sleep(NodeSessionSettle / 5)
			w.marked["late"] = w.sh("late", []string{w.entry()}, true, false, `exec sleep 300`)
			got := <-done
			w.check(got.r, got.err)
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
			time.Sleep(200 * time.Millisecond) // the leader exits
			w.check(s.sweep(t, w))
		})
	}
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
			time.Sleep(200 * time.Millisecond) // prctl runs
			w.check(s.sweep(t, w))
		})
		t.Run(s.name+"/alone", func(t *testing.T) {
			w := newNodeWorld(t)
			pid := w.start("undumpable", []string{w.entry()}, true, false, bin)
			time.Sleep(200 * time.Millisecond) // prctl runs
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
	for _, tok := range []string{outer, inner} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			found, err := FindNodeSession(tok)
			if err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(found, func(p SessionProcess) bool { return p.PID == pid }) {
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("session %s does not hold the nested process (pid %d)", tok, pid)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
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
