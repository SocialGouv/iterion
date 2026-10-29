package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// periodicWorkerFrames names the goroutine a runview.Service starts and keeps
// for its whole life, one entry per kind. Reading stack frames rather than
// instrumenting the production code keeps the oracle on the artefact that
// actually runs.
var periodicWorkerFrames = map[string]string{
	"orphan reconcile":   "runview.(*Service).startPeriodicReconcile.func1",
	"pipeline scheduler": "runview.(*Service).startPipelineScheduler.func1",
	"alert stall poll":   "alert.(*Manager).Start.func1",
}

// workerSettleCeiling bounds how long a reading waits for the workers a switch
// signalled to return — a ceiling, not an expectation: the wait ends the
// instant the set read is the one expected, so load changes how long this test
// takes and never what it decides.
const workerSettleCeiling = 30 * time.Second

// goroutinesCarrying returns the ids of the goroutines whose OWN stack carries
// frame. The dump is a stop-the-world snapshot and the runtime never reuses a
// goroutine id, so two readings are comparable as SETS: a goroutine the first
// does not hold and the second does was started in between, whatever the rest
// of the process did meanwhile.
func goroutinesCarrying(t *testing.T, frame string) map[int]bool {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for n == len(buf) {
		buf = make([]byte, 2*len(buf))
		n = runtime.Stack(buf, true)
	}
	ids := map[int]bool{}
	for _, block := range strings.Split(string(buf[:n]), "\n\n") {
		head, stack, ok := strings.Cut(block, "\n")
		if !ok || !strings.HasPrefix(head, "goroutine ") {
			continue
		}
		// A "created by" line names the frame of the goroutine's PARENT: the
		// worker's own children carry it without being workers.
		own, _, _ := strings.Cut(stack, "\ncreated by ")
		if !strings.Contains(own, frame) {
			continue
		}
		fields := strings.Fields(head)
		if len(fields) < 2 {
			t.Fatalf("unreadable goroutine header %q — this oracle reads ids out of the runtime's dump", head)
		}
		id, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("unreadable goroutine id in %q: %v", head, err)
		}
		ids[id] = true
	}
	return ids
}

// ownWorkers returns the goroutines carrying frame that ambient does not hold,
// i.e. the ones the caller is answerable for.
func ownWorkers(t *testing.T, frame string, ambient map[int]bool) []int {
	t.Helper()
	var mine []int
	for id := range goroutinesCarrying(t, frame) {
		if !ambient[id] {
			mine = append(mine, id)
		}
	}
	slices.Sort(mine)
	return mine
}

// settleOwnWorkers polls until the workers the caller owns number want, or the
// ceiling runs out — a stopped goroutine returns shortly after its signal, not
// at the instant of it. Returns the last reading.
func settleOwnWorkers(t *testing.T, frame string, ambient map[int]bool, want int) []int {
	t.Helper()
	deadline := time.Now().Add(workerSettleCeiling)
	for {
		mine := ownWorkers(t, frame, ambient)
		if len(mine) == want || !time.Now().Before(deadline) {
			return mine
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The hot-swap replaces the run service without draining it: the previous
// service's ENGINE goroutines keep writing to the store they started on, and
// that is deliberate. Its PERIODIC workers have no such claim — left running
// they scan a store the server no longer serves, once per interval, for the
// process lifetime, one more scanner per switch.
func TestSwapWorkDir_LeavesOnePeriodicWorkerPerKind(t *testing.T) {
	// Who is standing BEFORE this test's server exists. Goroutine ids are
	// never reused, so every worker this test is answerable for is one this
	// set does not hold — and an ambient worker that leaves, or lingers,
	// while the switches run cannot move the reading either way. A COUNT
	// cannot say that: by the time this test runs the suite leaves about two
	// hundred of these workers standing, and one of them exiting late reads
	// exactly like a worker this test leaked (it did, on a loaded CI shard:
	// "197 live after 4 project switches, want 198" — one FEWER than the
	// baseline, reported as a leak). The reading rests on this test NOT being
	// parallel: nothing else in the package builds a service while it runs,
	// so a goroutine the set does not hold is this test's own. A t.Parallel()
	// here would hand the reading another test's workers.
	ambient := map[string]map[int]bool{}
	for kind, frame := range periodicWorkerFrames {
		ambient[kind] = goroutinesCarrying(t, frame)
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
	// Every swap starts a mission sweep (swapWorkDir → restartAssistantMissions)
	// whose FS store root is the PROCESS-WIDE hometest home, not this test's
	// TempDir: mkProjectDir makes a bare dir, so ResolveStoreDir falls back to
	// the global projects slot under ITERION_HOME, and the store MkdirAll's
	// that root on every call, reads included. Shutdown (shutdownOnCleanup)
	// cancels the sweep and joins it inside this budget — past the default
	// 500ms, a sweep parked mid-call on a starved runner outlives the join
	// and re-creates the home during hometest.Isolate's removeHome (the
	// "test iterion home left behind" package failure). A wider budget can
	// only make this test WAIT longer.
	srv.bgJoinBudget = 30 * time.Second
	shutdownOnCleanup(t, srv)

	for i := 1; i <= 4; i++ {
		next := filepath.Join(dir, "p"+string(rune('0'+i)))
		mkProjectDir(t, next)
		if err := srv.swapWorkDir(context.Background(), next); err != nil {
			t.Fatalf("swap %d: %v", i, err)
		}
	}

	for kind, frame := range periodicWorkerFrames {
		// One worker per kind: the service the server actually serves. Five
		// is the leak this test exists for — the initial service and the four
		// the switches replaced; zero is the opposite defect, a server
		// serving a store nothing scans.
		mine := settleOwnWorkers(t, frame, ambient[kind], 1)
		switch {
		case len(mine) > 1:
			t.Errorf("%s: %d workers of this test's own services live after 4 project switches (goroutines %v), want 1 — every switch left the previous service's worker scanning a store the server no longer serves", kind, len(mine), mine)
		case len(mine) == 0:
			t.Errorf("%s: not one worker of this test's own services is live after 4 project switches — the service the server serves scans nothing", kind)
		}
	}
}

// mkProjectDir makes a directory swapWorkDir accepts as a project.
func mkProjectDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}
