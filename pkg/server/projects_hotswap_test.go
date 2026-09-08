package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// periodicWorkerFrames names the goroutine a runview.Service starts and keeps
// for its whole life, one entry per kind. Counting stack frames rather than
// instrumenting the production code keeps the oracle on the artefact that
// actually runs.
var periodicWorkerFrames = map[string]string{
	"orphan reconcile":   "runview.(*Service).startPeriodicReconcile.func1",
	"pipeline scheduler": "runview.(*Service).startPipelineScheduler.func1",
	"alert stall poll":   "alert.(*Manager).Start.func1",
}

// liveGoroutines counts the goroutines whose stack carries frame. The dump is
// a stop-the-world snapshot, so the count is consistent.
func liveGoroutines(frame string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), frame)
		}
		buf = make([]byte, 2*len(buf))
	}
}

// The hot-swap replaces the run service without draining it: the previous
// service's ENGINE goroutines keep writing to the store they started on, and
// that is deliberate. Its PERIODIC workers have no such claim — left running
// they scan a store the server no longer serves, once per interval, for the
// process lifetime, one more scanner per switch.
func TestSwapWorkDir_LeavesOnePeriodicWorkerPerKind(t *testing.T) {
	// Baselines first: another test's service (should one ever appear) must
	// not be read as this one's leak. Taken once the reading is STABLE — a
	// worker stopped by a previous test is signalled before it returns, so a
	// single sample counts goroutines that are already on their way out and
	// sets a baseline nothing can come back to.
	base := map[string]int{}
	for kind, frame := range periodicWorkerFrames {
		base[kind] = stableGoroutines(frame)
	}

	dir := t.TempDir()
	first := filepath.Join(dir, "p0")
	mkProjectDir(t, first)
	srv := New(Config{
		WorkDir:                 first,
		StoreDir:                filepath.Join(first, ".iterion"),
		SkipProjectRegistration: true,
		MaxConcurrentPipelines:  2,
		Alerts:                  &runview.AlertSettings{StallTimeout: time.Minute, BaseURL: "http://127.0.0.1:0"},
	}, iterlog.New(iterlog.LevelError, nil))
	if srv.runs == nil {
		t.Fatal("no run service was wired — the test would assert on nothing")
	}
	t.Cleanup(func() { srv.runs.Stop(context.Background()) })

	for i := 1; i <= 4; i++ {
		next := filepath.Join(dir, "p"+string(rune('0'+i)))
		mkProjectDir(t, next)
		if err := srv.swapWorkDir(context.Background(), next); err != nil {
			t.Fatalf("swap %d: %v", i, err)
		}
	}

	for kind, frame := range periodicWorkerFrames {
		want := base[kind] + 1 // the service the server actually serves
		if got := settleGoroutines(frame, want); got != want {
			t.Errorf("%s: %d live after 4 project switches, want %d — every switch left the previous service's worker scanning a store the server no longer serves", kind, got, want)
		}
	}
}

// settleGoroutines polls until the count reaches want or the budget runs out —
// a stopped goroutine returns shortly after its signal, not at the instant of
// it. Returns the last count read.
func settleGoroutines(frame string, want int) int {
	deadline := time.Now().Add(3 * time.Second)
	got := liveGoroutines(frame)
	for got != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		got = liveGoroutines(frame)
	}
	return got
}

// stableGoroutines returns a count that two readings apart agree on, so a
// worker already winding down from an earlier test is not counted as
// standing. Falls back to the last reading when nothing settles in budget.
func stableGoroutines(frame string) int {
	deadline := time.Now().Add(3 * time.Second)
	prev := liveGoroutines(frame)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		got := liveGoroutines(frame)
		if got == prev {
			return got
		}
		prev = got
	}
	return prev
}

// mkProjectDir makes a directory swapWorkDir accepts as a project.
func mkProjectDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}
