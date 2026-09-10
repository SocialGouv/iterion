//go:build linux

// Package proctest checks OS resources owned by a test binary.
package proctest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// defaultSettle is how long a child observed alive after the suite is given
// to finish exiting before it counts as a leak. "Cleanups returned" is not
// "the kernel finished the teardown": a helper a cleanup already signalled,
// or one a test joins only by proxy (a pidfile disappearing, a status flip),
// still has its own exit path to run.
const defaultSettle = 500 * time.Millisecond

// reclaimBudget bounds the teardown AFTER a leak verdict: killing a leaked
// parent reparents its children onto us, and each generation is rescanned.
const reclaimBudget = 5 * time.Second

// settleEnv overrides defaultSettle with a Go duration. The guard's own
// fixtures use it to make both sides of the boundary explicit; a slow CI
// runner can widen it without touching the code.
const settleEnv = "ITERION_PROCTEST_SETTLE"

// procKey identifies a process across scans. The PID alone would not: the
// kernel is free to hand it to an unrelated process once ours is reaped, and
// that newcomer must not inherit an already-expired settle window.
type procKey struct {
	pid   int
	start uint64
}

// NoProcessLeaks runs a suite as a Linux child subreaper: an orphaned helper
// remains its descendant instead of escaping to init. After the suite (and
// all t.Cleanup callbacks) returns, a process STILL ALIVE after a short
// settle window (see [settleEnv]) fails the suite and is reclaimed; one that
// finishes its exit path inside that window is forgiven and merely counted,
// so a cleanup that signalled without joining is not reported as a leak.
// Other test binaries and pre-existing session orphans are not descendants
// and cannot be reported or signalled by this guard.
//
// Compose it in TestMain: os.Exit(proctest.NoProcessLeaks(m.Run)). A guard
// around an existing suite wrapper works too. It never reaps during run:
// os/exec owns Wait while tests are executing.
// If the kernel cannot support the guard, it reports that limitation and
// still runs the suite. An inspection failure after activation remains fatal.
func NoProcessLeaks(run func() int) int {
	return noProcessLeaks(run, func() error {
		return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	}, childPIDs)
}

// Keep the capability boundary injectable so unsupported startup and a scan
// failure after activation can be distinguished without changing the host.
func noProcessLeaks(run func() int, enableSubreaper func() error, inspectChildren func() ([]int, error)) int {
	// Probe the scanner before enabling adoption. Otherwise an unsupported
	// /proc leaves the process adopting children it cannot subsequently find.
	if _, err := inspectChildren(); err != nil {
		fmt.Fprintf(os.Stderr, "proctest: leak guard unavailable (child scan: %v); running unguarded\n", err)
		return run()
	}
	if err := enableSubreaper(); err != nil {
		fmt.Fprintf(os.Stderr, "proctest: leak guard unavailable (subreaper: %v); running unguarded\n", err)
		return run()
	}
	settle := settleWindow()
	code := run()
	leaked := false
	forgave := 0
	// When each surviving child was first seen alive, and which ones already
	// produced a verdict. The window is a ceiling, not a wait: a child that
	// exits at 30ms is forgiven at 30ms, and a suite with no children at all
	// breaks out below before any of this is consulted.
	firstSeen := map[procKey]time.Time{}
	reported := map[procKey]bool{}
	// Killing a leaked parent reparents its children to us; rescan until the
	// entire owned tree is gone. This is a teardown bound, not a test budget.
	deadline := time.Now().Add(settle + reclaimBudget)
	for {
		children, err := inspectChildren()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: inspect test descendants: %v\n", err)
			return failing(code)
		}
		if len(children) == 0 {
			// A thread can exit and hand its children to another thread
			// between /proc reads. Only the kernel's process-wide ECHILD
			// verdict proves that no owned descendant remains.
			var status unix.WaitStatus
			_, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if errors.Is(err, unix.ECHILD) {
				// Nothing of ours is left, so anything still awaiting a verdict
				// finished on its own. This is not the same set the loop below
				// settles: a child seen alive by one pass and reaped by that
				// SAME pass (the per-PID Wait4 runs after the state read) stays
				// in firstSeen and is gone from /proc before the next pass, so
				// it never reaches that accounting — and forgiveness would
				// under-count exactly the children that exited fastest.
				forgave += forgiven(firstSeen, nil, reported)
				break
			}
			if err != nil && !errors.Is(err, unix.EINTR) {
				fmt.Fprintf(os.Stderr, "FAIL: inspect remaining test children: %v\n", err)
				return failing(code)
			}
		}
		now := time.Now()
		alive := make(map[procKey]time.Time, len(children))
		for _, pid := range children {
			p, err := os.FindProcess(pid) // Linux uses a pidfd, avoiding PID reuse on Signal.
			if err != nil {
				continue
			}
			stat, err := processState(pid)
			if err != nil || stat.parent != os.Getpid() {
				_ = p.Release()
				continue
			}
			if stat.state != "Z" && stat.state != "X" {
				key := procKey{pid: pid, start: stat.start}
				since, seen := firstSeen[key]
				if !seen {
					since = now
				}
				alive[key] = since
				// Only a child still alive past its window is a leak. Do not
				// kill inside it: killing on first sight would erase the very
				// distinction between "exiting" and "survived".
				if now.Sub(since) >= settle && !reported[key] {
					leaked = true
					reported[key] = true
					reclaim(p)
				}
			}
			_ = p.Release()
			// Nonblocking: a just-killed child may not be waitable yet. Only this
			// known direct child is reaped; never wait on another test's processes.
			var status unix.WaitStatus
			_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
		}
		forgave += forgiven(firstSeen, alive, reported)
		firstSeen = alive
		if time.Now().After(deadline) {
			// The budget is spent, so every window still open is spent with it.
			// Without this sweep a descendant first seen inside the last
			// `settle` of the budget would leave unnamed and alive — the guard
			// killed on sight before the window existed, and the promise that
			// a survivor is always reported and reclaimed must not lapse on
			// the one path where a teardown proved hardest.
			for key := range firstSeen {
				if reported[key] {
					continue // already named, already signalled; it just won't die
				}
				p, err := os.FindProcess(key.pid)
				if err != nil {
					continue
				}
				// Same PID-reuse standard as procKey, and the same definition of
				// alive as the scan above: an entry that died between the last
				// scan and here is not a survivor, and must not be named as one.
				if stat, err := processState(key.pid); err == nil && stat.start == key.start &&
					stat.parent == os.Getpid() && stat.state != "Z" && stat.state != "X" {
					reclaim(p)
				}
				_ = p.Release()
			}
			fmt.Fprintln(os.Stderr, "FAIL: test descendants did not exit after cleanup")
			return failing(code)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if forgave > 0 {
		fmt.Fprintf(os.Stderr, "proctest: forgave %d settling child(ren)\n", forgave)
	}
	if leaked && code == 0 {
		return 1
	}
	return code
}

// reclaim names a survivor and kills it. It takes the *os.Process the caller
// already verified rather than a bare PID: on Linux that handle is a pidfd,
// so the signal cannot land on whoever inherits the number next.
func reclaim(p *os.Process) {
	name, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", p.Pid))
	fmt.Fprintf(os.Stderr, "FAIL: test process survived suite cleanup: pid=%d command=%s\n", p.Pid, strings.TrimSpace(string(name)))
	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		fmt.Fprintf(os.Stderr, "FAIL: reclaim test process %d: %v\n", p.Pid, err)
	}
}

// forgiven counts the children of prev that are gone from cur without ever
// having produced a verdict: each was seen alive and then finished inside its
// window. Counting them is what keeps the forgiveness path observable —
// forgiving is otherwise silent, indistinguishable from never having seen the
// child at all. A nil cur is the end of the scan, where nothing of ours is
// left and every entry still pending is therefore forgiven.
//
// It must be called on every path that drops entries from the tracking map,
// and each key must be dropped exactly once, or the count drifts from the
// number of children the guard actually forgave.
func forgiven(prev, cur map[procKey]time.Time, reported map[procKey]bool) int {
	n := 0
	for key := range prev {
		if _, still := cur[key]; !still && !reported[key] {
			n++
		}
	}
	return n
}

// failing turns a teardown verdict into an exit code without overwriting the
// suite's own: both are failures, and the one the reader needs is the test's.
func failing(code int) int {
	if code != 0 {
		return code
	}
	return 1
}

// settleWindow reads [settleEnv], falling back to [defaultSettle]. A zero
// duration is legitimate — it latches a verdict on first sight.
func settleWindow() time.Duration {
	raw := os.Getenv(settleEnv)
	if raw == "" {
		return defaultSettle
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "proctest: ignoring invalid %s=%q, using %s\n", settleEnv, raw, defaultSettle)
		return defaultSettle
	}
	return d
}

// Children can belong to any OS thread in this Go process, not just the
// thread-group leader. Reading its task entries is narrower than scanning
// the host process table and remains isolated from concurrent test packages.
func childPIDs() ([]int, error) {
	paths, err := filepath.Glob("/proc/self/task/*/children")
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("/proc task children unavailable")
	}
	seen := map[int]bool{}
	var result []int
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		} // an OS thread exited during the scan
		if err != nil {
			return nil, err
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return nil, err
			}
			if !seen[pid] {
				seen[pid] = true
				result = append(result, pid)
			}
		}
	}
	return result, nil
}

type procStat struct {
	state  string
	parent int
	start  uint64 // boot-relative start time; the tiebreaker against PID reuse
}

func processState(pid int) (procStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procStat{}, err // unwrapped: callers test it with os.IsNotExist
	}
	// comm is parenthesized and can itself contain spaces or parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return procStat{}, fmt.Errorf("invalid process stat for %d", pid)
	}
	// Once the parenthesized comm is dropped, stat field N is at index N-3:
	// state (3) and ppid (4) lead, starttime (22) lands on index 19.
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return procStat{}, fmt.Errorf("short process stat for %d", pid)
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return procStat{}, fmt.Errorf("process stat ppid for %d: %w", pid, err)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("process stat starttime for %d: %w", pid, err)
	}
	return procStat{state: fields[0], parent: parent, start: start}, nil
}
