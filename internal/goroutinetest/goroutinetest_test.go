package goroutinetest

import (
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// parked names the three goroutines the fixture keeps standing, by the names
// the RUNTIME gave them: each one reports its own frame, so this witness rests
// on what the runtime actually prints rather than on names spelled here.
type parked struct {
	worker  string // the frame a caller looks for
	sibling string // carries worker as a PREFIX
	child   string // a named function the worker started
}

// park starts the three goroutines and keeps them standing until the test
// ends, which it JOINS: a generation on its way out while the next reading
// runs is a count this witness cannot tell from the defect it looks for.
//
// Each role reports on its OWN channel — three names arriving on one channel
// would be told apart by their arrival order, which nothing here fixes.
func park(t *testing.T) parked {
	t.Helper()
	release := make(chan struct{})
	worker := make(chan string, 1)
	sibling := make(chan string, 1)
	child := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(3)
	// The FIRST eleven literals of this function: the runtime names a
	// function's literals in source order, so index 0 runs as "<fn>.func1"
	// and index 10 as "<fn>.func11". The nine that do nothing are what pushes
	// the eleventh's index into the prefix relation — the shape a worker's
	// frame takes the day a tenth literal is written above it.
	lits := []func(){
		func() { defer wg.Done(); worker <- selfFrame(); go childPark(&wg, child, release); <-release },
		func() {}, func() {}, func() {}, func() {}, func() {},
		func() {}, func() {}, func() {}, func() {},
		func() { defer wg.Done(); sibling <- selfFrame(); <-release },
	}
	t.Cleanup(func() { close(release); wg.Wait() })
	go lits[0]()
	go lits[10]()
	return parked{worker: <-worker, sibling: <-sibling, child: <-child}
}

// childPark is a goroutine the WORKER starts: its own stack carries its own
// frame, and the worker's frame on its "created by" line alone.
func childPark(wg *sync.WaitGroup, name chan<- string, release <-chan struct{}) {
	defer wg.Done()
	name <- selfFrame()
	<-release
}

// selfFrame names the function the CALLER runs, as the runtime names it.
func selfFrame() string {
	var pcs [1]uintptr
	n := runtime.Callers(2, pcs[:])
	if n == 0 {
		return ""
	}
	f, _ := runtime.CallersFrames(pcs[:n]).Next()
	return f.Function
}

// The callers of this reader convict a worker of outliving its stop, so a
// frame counted for a goroutine that is not that worker is a verdict that
// cannot redden. All of it is read off ONE real dump: the worker is found, the
// sibling whose name carries the worker's as a prefix is not counted as one,
// the worker's child is not either, a frame nothing runs comes back as an
// empty set rather than a missing one, and the single-frame shape answers by
// the same rule as the many-frame one.
func TestByFrameHoldsTheWorkerAloneAmongItsPrefixNamedSiblingAndItsChild(t *testing.T) {
	p := park(t)
	if !strings.HasPrefix(p.sibling, p.worker) || p.sibling == p.worker {
		t.Fatalf("premise: the runtime named the fixture's first and eleventh literals %q and %q, which stand in no prefix relation — the collision this reader exists for cannot be exercised", p.worker, p.sibling)
	}
	const absent = "goroutinetest.noGoroutineRunsThisFrame"
	frames := map[string]string{
		"worker":  p.worker,
		"sibling": p.sibling,
		"child":   p.child,
		"absent":  absent,
	}
	got := ByFrame(t, frames)

	// One goroutine per frame, and three DIFFERENT ones: the sibling and the
	// child are standing, each findable by its own frame, so "one worker" is
	// not one because they never started.
	ids := map[string]int{}
	for _, label := range []string{"worker", "sibling", "child"} {
		set := got[label]
		if len(set) != 1 {
			t.Errorf("%s: goroutines %v carry %q, want exactly the one the fixture parked on it", label, slices.Sorted(maps.Keys(set)), frames[label])
			continue
		}
		for id := range set {
			ids[label] = id
		}
	}
	if len(ids) == 3 && (ids["worker"] == ids["sibling"] || ids["worker"] == ids["child"] || ids["sibling"] == ids["child"]) {
		t.Errorf("the three frames name %v — the fixture parked three goroutines, so a shared id means a frame was bound to another goroutine's block", ids)
	}
	if set, ok := got["absent"]; !ok || len(set) != 0 {
		t.Errorf("the frame %q, which nothing runs, came back as %v (label present: %v) — a caller indexes and iterates every label it asked for", absent, slices.Sorted(maps.Keys(set)), ok)
	}
	if one := Carrying(t, p.worker); !maps.Equal(one, got["worker"]) {
		t.Errorf("Carrying(%q) = %v while ByFrame holds %v for the same frame — the single-frame shape reads by another rule", p.worker, slices.Sorted(maps.Keys(one)), slices.Sorted(maps.Keys(got["worker"])))
	}
}

// deepParkDepth is how far deepPark recurses. A dump's cost is its frames, so
// depth is what lets a fixture outgrow a megabyte buffer with a few hundred
// goroutines instead of a few thousand.
const deepParkDepth = 60

// deepPark recurses to depth, reports the frame the runtime gives it, and
// stands until release. Every frame of the recursion prints in the dump, which
// is what makes the goroutine expensive to read.
func deepPark(depth int, wg *sync.WaitGroup, ready chan<- string, release <-chan struct{}) {
	if depth > 0 {
		deepPark(depth-1, wg, ready, release)
		return
	}
	defer wg.Done()
	ready <- selfFrame()
	<-release
}

// parkDeep starts count goroutines on deepPark, waits for all of them to be
// standing, and returns the frame they report. The caller owns release and wg:
// one close joins every generation it started.
func parkDeep(t *testing.T, release <-chan struct{}, wg *sync.WaitGroup, count int) string {
	t.Helper()
	ready := make(chan string, count)
	wg.Add(count)
	for range count {
		go deepPark(deepParkDepth, wg, ready, release)
	}
	frame := <-ready
	for range count - 1 {
		if other := <-ready; other != frame {
			t.Fatalf("premise: the fixture's goroutines report the frames %q and %q — they run one function", frame, other)
		}
	}
	return frame
}

// trueDumpSize measures the runtime's dump with a buffer this fixture cannot
// fill, so what sizes the fixture is the runtime rather than the growth loop
// under test.
func trueDumpSize(t *testing.T) int {
	t.Helper()
	buf := make([]byte, 64<<20)
	n := runtime.Stack(buf, true)
	if n == len(buf) {
		t.Fatalf("premise: %d bytes did not hold this process's dump", len(buf))
	}
	return n
}

// Every reading of this package rests on a dump read whole: runtime.Stack
// writes what fits and reports only what it wrote, so a buffer the dump
// outgrows cuts it in silence and the goroutines past the cut are absent from
// the answer. The fixture parks until the dump is larger than the buffer Dump
// starts from, then asks for all of them back.
func TestDumpGrowsPastItsStartingBufferAndHoldsEveryGoroutine(t *testing.T) {
	const (
		batch = 32
		// maxPark caps the fixture: this many deep stacks still fit the
		// buffer trueDumpSize measures with.
		maxPark = 1024
		// Past the starting buffer by more than a few blocks, so a reading
		// cut there comes back short rather than merely at the boundary.
		target = initialDumpBuf + 64<<10
	)
	release := make(chan struct{})
	var wg sync.WaitGroup
	t.Cleanup(func() { close(release); wg.Wait() })
	frame, parked := "", 0
	for {
		frame = parkDeep(t, release, &wg, batch)
		parked += batch
		if trueDumpSize(t) > target {
			break
		}
		if parked >= maxPark {
			t.Fatalf("premise: %d parked goroutines left this process's dump at %d bytes, short of the %d this witness reads past", parked, trueDumpSize(t), target)
		}
	}

	// The size and what the size costs are two readings of one defect, so
	// neither stops the other: a cut dump that still answered for every
	// parked goroutine, or a whole dump that lost one, each says something
	// the other cannot.
	if d := Dump(); len(d) <= initialDumpBuf {
		t.Errorf("Dump answered %d bytes for a dump of %d — a reading cut at the %d buffer it starts from", len(d), trueDumpSize(t), initialDumpBuf)
	}
	if ids := Carrying(t, frame); len(ids) != parked {
		t.Errorf("%d goroutines carry %q, want the %d the fixture parked — a reading taken on a cut dump misses the ones past the cut", len(ids), frame, parked)
	}
}
