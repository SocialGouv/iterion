//go:build linux

package proc

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// BootTicksNow is the current time in the unit of a process's start time.
func BootTicksNow() (BootTicks, error) {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, fmt.Errorf("read /proc/uptime: %w", err)
	}
	f := strings.Fields(string(raw))
	if len(f) == 0 {
		return 0, fmt.Errorf("read /proc/uptime: empty")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("read /proc/uptime: %w", err)
	}
	return BootTicks(secs * userHZ), nil
}

// procStat is what /proc/<pid>/stat says of a process.
type procStat struct {
	ppid  int
	start uint64
	state byte
	name  string
}

func readStat(dir string) (procStat, bool) {
	raw, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return procStat{}, false
	}
	// The name is parenthesised and may itself hold spaces or parentheses;
	// every field after its closing one is a number or a state letter.
	open, closing := bytes.IndexByte(raw, '('), bytes.LastIndexByte(raw, ')')
	if open < 0 || closing < open {
		return procStat{}, false
	}
	f := strings.Fields(string(raw[closing+1:]))
	if len(f) < 20 || len(f[0]) == 0 {
		return procStat{}, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	start, err2 := strconv.ParseUint(f[19], 10, 64)
	if err1 != nil || err2 != nil {
		return procStat{}, false
	}
	return procStat{ppid: ppid, start: start, state: f[0][0], name: string(raw[open+1 : closing])}, true
}

// running reports whether the process in dir runs: a leader that exited is a
// zombie while one of its threads still runs.
func running(dir string, st procStat) bool {
	if st.state != 'Z' {
		return true
	}
	tasks, _ := os.ReadDir(dir + "/task")
	for _, t := range tasks {
		if ts, ok := readStat(dir + "/task/" + t.Name()); ok && ts.state != 'Z' {
			return true
		}
	}
	return false
}

// readEnviron reads the environment of the process in dir, through its threads
// when the process-wide file cannot be read — a leader that exited takes it
// with it, its threads do not.
func readEnviron(dir string) ([]byte, bool) {
	if env, err := os.ReadFile(dir + "/environ"); err == nil {
		return env, true
	}
	tasks, _ := os.ReadDir(dir + "/task")
	for _, t := range tasks {
		if env, err := os.ReadFile(dir + "/task/" + t.Name() + "/environ"); err == nil {
			return env, true
		}
	}
	return nil, false
}

// environHolds reports whether a NUL-separated environment holds entry as one
// whole entry — not a prefix of a longer value, not a substring of another
// variable.
func environHolds(environ, entry []byte) bool {
	for _, kv := range bytes.Split(environ, []byte{0}) {
		if bytes.Equal(kv, entry) {
			return true
		}
	}
	return false
}

// ownedBy reports whether the process in dir runs under the real uid uid. The
// directory's owner is the effective uid, and root's for a process that made
// itself non-dumpable: the status file says the real one.
func ownedBy(dir string, uid int) bool {
	raw, err := os.ReadFile(dir + "/status")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			f := strings.Fields(rest)
			return len(f) > 0 && f[0] == strconv.Itoa(uid)
		}
	}
	return false
}

// sessionMember is a process a sweep counts as the session's.
type sessionMember struct {
	start uint64
	name  string
}

// sessionScan is one look at /proc for a node session.
type sessionScan struct {
	// members runs: the carriers of the entry, the processes known from an
	// earlier scan and still the same, and every descendant of either.
	members map[int]sessionMember
	// unreadable runs under this uid, started at or after since, is no
	// member, and its environment could not be read.
	unreadable map[int]sessionMember
}

func scanNodeSession(token string, since BootTicks, known map[int]sessionMember) (sessionScan, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return sessionScan{}, fmt.Errorf("find node session: %w", err)
	}
	want := []byte(NodeSessionEntry(token))
	self, uid := os.Getpid(), os.Getuid()
	all := map[int]procStat{}
	base := map[int]bool{}
	unread := map[int]procStat{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == self {
			continue
		}
		dir := "/proc/" + e.Name()
		st, ok := readStat(dir)
		if !ok || !running(dir, st) {
			continue
		}
		all[pid] = st
		if k, ok := known[pid]; ok && k.start == st.start {
			base[pid] = true
			continue
		}
		env, ok := readEnviron(dir)
		switch {
		case ok && environHolds(env, want):
			base[pid] = true
		case !ok && st.start >= uint64(since) && ownedBy(dir, uid):
			unread[pid] = st
		}
	}
	sc := sessionScan{members: map[int]sessionMember{}, unreadable: map[int]sessionMember{}}
	for pid, st := range all {
		for q, n := pid, 0; q > 1 && n < 512; n++ {
			if base[q] {
				sc.members[pid] = sessionMember{start: st.start, name: st.name}
				break
			}
			anc, ok := all[q]
			if !ok {
				break
			}
			q = anc.ppid
		}
	}
	for pid, st := range unread {
		if _, ok := sc.members[pid]; !ok {
			sc.unreadable[pid] = sessionMember{start: st.start, name: st.name}
		}
	}
	return sc, nil
}

// FindNodeSession returns the live processes of the session token: the carriers
// of its entry and their descendants.
func FindNodeSession(token string) ([]SessionProcess, error) {
	if err := checkNodeSessionToken(token); err != nil {
		return nil, err
	}
	sc, err := scanNodeSession(token, 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]SessionProcess, 0, len(sc.members))
	for pid, m := range sc.members {
		out = append(out, SessionProcess{PID: pid, Name: m.name})
	}
	return out, nil
}

func anyRunning(known map[int]sessionMember) bool {
	for pid, m := range known {
		if sameProcess(pid, m.start) {
			return true
		}
	}
	return false
}

// sameProcess reports whether pid is still the process that started at start:
// a pid freed and reused in between is another process.
func sameProcess(pid int, start uint64) bool {
	dir := "/proc/" + strconv.Itoa(pid)
	st, ok := readStat(dir)
	return ok && st.start == start && running(dir, st)
}

// sweepScanGate, nil in production, runs after every scan of a sweep in this
// process — with what the scan found of the session, before the sweep acts on
// it — and the sweep waits for it to return. It is the seam a test holds a
// sweep at, to stage a process that shows between two scans instead of
// guessing when the window between them opens; sweepScanGateFile is its
// counterpart in the program a sandbox runs.
var sweepScanGate func(found int)

// SweepNodeSession stops every process of the session token and says what it
// did. Each scan finds the carriers of the entry, the processes earlier scans
// found and that are still the same, and every descendant of either; it sends
// them TERM, once each, every NodeSessionSettle until two scans in a row find
// nothing or NodeSessionGrace is over, then — unless the TERM phase ended
// with two such scans — KILL the same way within NodeSessionKillWait. Every
// signal goes to a pid checked to still be the
// process the scan found. since is when the session began: a process of this
// uid started since then whose environment cannot be read is reported, not
// guessed at.
func SweepNodeSession(token string, since BootTicks) (SweepReport, error) {
	if err := checkNodeSessionToken(token); err != nil {
		return SweepReport{}, err
	}
	known := map[int]sessionMember{}
	unreadable := map[int]sessionMember{}
	termed := map[int]bool{}
	// phase reports whether it ended on two empty scans in a row rather than
	// on its deadline.
	phase := func(sig syscall.Signal, deadline time.Time) (bool, error) {
		quiet := 0
		for {
			sc, err := scanNodeSession(token, since, known)
			if err != nil {
				return false, err
			}
			for pid, m := range sc.unreadable {
				unreadable[pid] = m
			}
			if g := sweepScanGate; g != nil {
				g(len(sc.members))
			}
			if len(sc.members) == 0 {
				quiet++
				if quiet == 2 {
					return true, nil
				}
			} else {
				quiet = 0
				for pid, m := range sc.members {
					if _, ok := known[pid]; !ok {
						known[pid] = m
					}
					if sig == syscall.SIGTERM && termed[pid] {
						continue
					}
					if !sameProcess(pid, m.start) {
						continue
					}
					_ = syscall.Kill(pid, sig)
					if sig == syscall.SIGTERM {
						termed[pid] = true
					}
				}
			}
			if time.Now().After(deadline) {
				return false, nil
			}
			time.Sleep(NodeSessionSettle)
		}
	}
	// A TERM phase that ends quiet leaves nothing for KILL to find.
	quiet, err := phase(syscall.SIGTERM, time.Now().Add(sweepGrace))
	if err == nil && !quiet {
		_, err = phase(syscall.SIGKILL, time.Now().Add(sweepKillWait))
	}
	// A KILL is delivered at once but a process takes a moment to exit: give
	// the last ones that moment before calling them survivors.
	for i := 0; i < 10 && anyRunning(known); i++ {
		time.Sleep(NodeSessionSettle)
	}
	var r SweepReport
	for pid, m := range known {
		p := SessionProcess{PID: pid, Name: m.name}
		if sameProcess(pid, m.start) {
			r.Survivors = append(r.Survivors, p)
		} else {
			r.Stopped = append(r.Stopped, p)
		}
	}
	for pid, m := range unreadable {
		if _, ok := known[pid]; !ok && sameProcess(pid, m.start) {
			r.Unreadable = append(r.Unreadable, SessionProcess{PID: pid, Name: m.name})
		}
	}
	return r, err
}
