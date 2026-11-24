//go:build linux

package scripthost

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// threadMoveReport is what TestThreadMoveHelperProcess tells the test.
type threadMoveReport struct {
	AllThreads bool   `json:"all_threads"`
	FromTID    int    `json:"from_tid"`
	ToTID      int    `json:"to_tid"`
	ConfineErr string `json:"confine_err,omitempty"`
}

// moveToAnotherThread displaces the calling goroutine from its OS thread,
// unless that goroutine is pinned. With one P, a goroutine that locks
// whichever thread runs it and then blocks forever takes that thread out of
// service. If the caller is not pinned, it resumes on another thread; if it
// is pinned, the blocker runs elsewhere and the caller keeps its own.
func moveToAnotherThread(report *threadMoveReport) {
	report.FromTID = unix.Gettid()
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		close(ready)
		select {}
	}()
	<-ready
	runtime.Gosched()
	report.ToTID = unix.Gettid()
}

// spareThreads leaves n idle OS threads in the runtime's pool, created BEFORE
// Confine and so without no_new_privs: each goroutine pins a thread, the next
// one needs a new thread, and all of them then unpin and finish. A goroutine
// displaced later resumes on one of these — the CI case — rather than on a
// thread spawned after no_new_privs, which would inherit the flag and hide
// the defect.
func spareThreads(n int) {
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		started := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			close(started)
			<-release
			runtime.UnlockOSThread()
		}()
		<-started
	}
	close(release)
	wg.Wait()
}

// TestThreadMoveHelperProcess is not a test: unpinned, it confines itself
// while the hook moves it to another OS thread right after no_new_privs.
func TestThreadMoveHelperProcess(t *testing.T) {
	if os.Getenv("ITERION_SCRIPTHOST_THREAD_MOVE_HELPER") != "1" {
		t.Skip("helper process only")
	}
	runtime.GOMAXPROCS(1) // what RunChild runs under
	spareThreads(4)
	var report threadMoveReport
	afterNoNewPrivs = func() { moveToAnotherThread(&report) }
	applied, err := Confine(Limits{Memory: 256 << 20, CPU: 30 * time.Second})
	if err != nil {
		report.ConfineErr = err.Error()
	}
	report.AllThreads = applied.AllThreads
	out, _ := json.Marshal(report)
	fmt.Println("THREAD-MOVE " + string(out))
	os.Exit(0)
}

// Confine's per-thread calls must all run on ONE OS thread. Without the pin, a
// goroutine moved between no_new_privs and the Landlock and seccomp installs
// reaches them from a thread without no_new_privs, and they refuse: seen in
// CI as `seccomp(SET_MODE_FILTER, TSYNC): permission denied`. The hook makes
// that move happen every time instead of once in a hundred runs.
//
// Two assertions, because the failure needs a destination thread WITHOUT
// no_new_privs. Measured with the pin removed: on a plain host, Confine
// fails (`landlock_restrict_self(abi 4): operation not permitted`). Inside a
// process tree that already carries no_new_privs on every thread
// (bubblewrap, a pod with allowPrivilegeEscalation: false), the moved
// goroutine succeeds. There, the thread-id assertion is the one that reddens.
//
// Only the per-thread fallback can fail this way, so only a cgo-linked test
// binary (the race shards, a local default build) exercises it; on the
// CGO_ENABLED=0 leg no_new_privs is on every thread and the arm skips, saying
// so.
func TestConfineRunsOnOneThreadEvenWhenTheGoroutineIsMoved(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(os.Args[0], "-test.run=^TestThreadMoveHelperProcess$", "-test.count=1")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ITERION_SCRIPTHOST_THREAD_MOVE_HELPER=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the helper did not finish (%v): %s", err, out)
	}
	var report threadMoveReport
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "THREAD-MOVE "); ok {
			if err := json.Unmarshal([]byte(rest), &report); err != nil {
				t.Fatalf("unreadable report %q: %v", rest, err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("the helper printed no report: %s", out)
	}
	t.Logf("helper report: %+v", report)
	if report.AllThreads {
		t.Skip("all-threads path: no_new_privs is on every thread, so a move is harmless here; " +
			"the per-thread fallback this arm witnesses runs on a cgo-linked binary (the race shards)")
	}
	if report.ConfineErr != "" {
		t.Fatalf("Confine failed when its goroutine was moved between no_new_privs and the bounds "+
			"(thread %d -> %d): %s", report.FromTID, report.ToTID, report.ConfineErr)
	}
	if report.FromTID != report.ToTID {
		t.Fatalf("the goroutine left its thread mid-Confine (%d -> %d): the pin did not hold",
			report.FromTID, report.ToTID)
	}
}
