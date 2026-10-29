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

// workerKinds names the kinds in a fixed order, so a reading that errors on
// several of them says so in the same order every time.
func workerKinds() []string {
	kinds := make([]string, 0, len(periodicWorkerFrames))
	for kind := range periodicWorkerFrames {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// workerSettleCeiling bounds how long a reading waits for the workers a switch
// signalled to return — a ceiling, not an expectation: the wait ends the
// instant the set read is the one expected, so load changes how long this test
// takes and never what it decides. ONE ceiling covers every kind, because one
// reading covers every kind: a leak of all three is reported after one ceiling
// and not after three.
const workerSettleCeiling = 30 * time.Second

// workersByKind returns, per kind, the ids of the goroutines whose OWN stack
// carries that kind's frame. Every kind comes out of ONE stop-the-world
// snapshot, so the readings describe the same instant and cost one dump. The
// runtime never reuses a goroutine id, so two snapshots are comparable as
// SETS: a goroutine the first does not hold and the second does was started in
// between, whatever the rest of the process did meanwhile.
func workersByKind(t *testing.T) map[string]map[int]bool {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	for n == len(buf) {
		buf = make([]byte, 2*len(buf))
		n = runtime.Stack(buf, true)
	}
	byKind := map[string]map[int]bool{}
	for _, kind := range workerKinds() {
		byKind[kind] = map[int]bool{}
	}
	for _, block := range strings.Split(string(buf[:n]), "\n\n") {
		head, stack, ok := strings.Cut(block, "\n")
		if !ok || !strings.HasPrefix(head, "goroutine ") {
			continue
		}
		// A "created by" line names the frame of the goroutine's PARENT: the
		// worker's own children carry it without being workers.
		own, _, _ := strings.Cut(stack, "\ncreated by ")
		var carries []string
		for kind, frame := range periodicWorkerFrames {
			if strings.Contains(own, frame) {
				carries = append(carries, kind)
			}
		}
		if len(carries) == 0 {
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
		for _, kind := range carries {
			byKind[kind][id] = true
		}
	}
	return byKind
}

// ownWorkers returns, per kind, the goroutines carrying that kind's frame that
// ambient does not hold, i.e. the ones the caller is answerable for.
func ownWorkers(t *testing.T, ambient map[string]map[int]bool) map[string][]int {
	t.Helper()
	mine := map[string][]int{}
	for kind, ids := range workersByKind(t) {
		for id := range ids {
			if !ambient[kind][id] {
				mine[kind] = append(mine[kind], id)
			}
		}
		slices.Sort(mine[kind])
	}
	return mine
}

// settleOwnWorkers polls — one dump per poll, every kind at once — until the
// workers the caller owns satisfy settled for every kind, or the ceiling runs
// out. Returns the last reading.
func settleOwnWorkers(t *testing.T, ambient map[string]map[int]bool, settled func(n int) bool) map[string][]int {
	t.Helper()
	deadline := time.Now().Add(workerSettleCeiling)
	for {
		mine := ownWorkers(t, ambient)
		done := true
		for _, kind := range workerKinds() {
			if !settled(len(mine[kind])) {
				done = false
			}
		}
		if done || !time.Now().Before(deadline) {
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
	ambient := workersByKind(t)

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

	// Every worker standing when a switch starts belongs to a service that
	// switch replaces — or to one an earlier switch already replaced and left
	// running. Their union is the set the one survivor may NOT be drawn from:
	// a worker in it scans a store the server no longer serves, and its being
	// the ONLY one alive says the switch stopped the service the server now
	// serves. Reading ids and judging a POPULATION cannot say that — one
	// worker standing is one worker standing, whichever service it belongs
	// to, so a switch that stops the WRONG service reads exactly like a
	// switch that stops the right one.
	replaced := map[int]bool{}
	for i := 1; i <= 4; i++ {
		before := settleOwnWorkers(t, ambient, func(n int) bool { return n >= 1 })
		for _, kind := range workerKinds() {
			if len(before[kind]) == 0 {
				t.Fatalf("premise, within %s: no %s worker of this test's own services is live before switch %d — the set this test's verdict is drawn from would be empty", workerSettleCeiling, kind, i)
			}
			for _, id := range before[kind] {
				replaced[id] = true
			}
		}
		next := filepath.Join(dir, "p"+string(rune('0'+i)))
		mkProjectDir(t, next)
		if err := srv.swapWorkDir(context.Background(), next); err != nil {
			t.Fatalf("swap %d: %v", i, err)
		}
	}

	// One worker per kind, and it is the SERVED service's. Five is the leak
	// this test exists for — the initial service and the four the switches
	// replaced; zero is the opposite defect, a server serving a store nothing
	// scans; one that a switch REPLACED is both at once, and is what a switch
	// stopping the wrong service leaves behind.
	mine := settleOwnWorkers(t, ambient, func(n int) bool { return n == 1 })
	for _, kind := range workerKinds() {
		live := mine[kind]
		switch {
		case len(live) > 1:
			t.Errorf("%s: %d workers of this test's own services live after 4 project switches (goroutines %v), want 1 — every switch left the previous service's worker scanning a store the server no longer serves", kind, len(live), live)
		case len(live) == 0:
			t.Errorf("%s: not one worker of this test's own services is live after 4 project switches — the service the server serves scans nothing", kind)
		case replaced[live[0]]:
			t.Errorf("%s: the one live worker (goroutine %d) belongs to a service a switch REPLACED — the switch stopped the service the server now serves, which scans nothing, and the store it no longer serves is still scanned once per interval", kind, live[0])
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
