package liveledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeTB drives Track through a test-double that implements the narrow
// TB interface. Unlike testing.TB, the narrow interface has no
// unexported methods, so it can be implemented here — and the cleanup
// closure Track registers runs when finish() replays it, exactly as the
// testing framework would after a body completes.
type fakeTB struct {
	failed   bool
	skipped  bool
	cleanups []func()
}

func (f *fakeTB) Cleanup(fn func())   { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Failed() bool        { return f.failed }
func (f *fakeTB) Skipped() bool       { return f.skipped }
func (f *fakeTB) Helper()             {}
func (f *fakeTB) Logf(string, ...any) {}
func (f *fakeTB) Name() string        { return "fakeTB" }

// finish replays the cleanups the way the testing framework does after
// the test body returns.
func (f *fakeTB) finish() {
	for _, fn := range f.cleanups {
		fn()
	}
}

// TestTrack_CleanupWritesPassRowFromRealSubtest drives the REAL testing
// framework: the subtest registers a t.Cleanup through Track and the
// framework runs it when the subtest completes. This is the writer path
// the feature rests on — env in, row out.
func TestTrack_CleanupWritesPassRowFromRealSubtest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	t.Run("body passes", func(t *testing.T) {
		Track(t)
	})

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger not written by the subtest's cleanup: %v", err)
	}
	row, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("no row for the probe target — the cleanup did not write")
	}
	if row.Verdict != VerdictPass {
		t.Fatalf("passing subtest recorded %q, want pass", row.Verdict)
	}
	if row.Target != "test:live:probe" {
		t.Fatalf("row keyed %q, want the env-provided target", row.Target)
	}
}

// A body that failed must record fail. The verdict comes from the
// framework's Failed() at cleanup time, not from the row's previous
// state.
func TestTrack_CleanupWritesFailRowWhenBodyFailed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	fake := &fakeTB{}
	Track(fake)
	fake.failed = true
	fake.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	row, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("no row written")
	}
	if row.Verdict != VerdictFail {
		t.Fatalf("failing body recorded %q, want fail", row.Verdict)
	}
}

// A skipped test never ran, so it records NOTHING: a `pass` row would
// mean "the harness verified this target", which a skip never did, and
// a `fail` row would blame the target for a missing credential.
func TestTrack_CleanupWritesNothingWhenBodySkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:probe")
	resetProcessWritten()

	fake := &fakeTB{}
	Track(fake)
	fake.skipped = true
	fake.finish()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("skipped body wrote a ledger row — a skip is neither pass nor fail (stat err: %v)", err)
	}
}

// One target ran several test functions in ONE process (the aggregate
// targets): the row must merge fail-sticky. fail-then-pass must report
// fail — last-write-wins would let a suite that failed report `pass`
// because its last member happened to pass — and duration/cost
// accumulate to the suite's totals.
func TestTrack_AggregateTargetMergesFailSticky(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:aggregate-probe")
	resetProcessWritten()

	start := time.Now().UTC()
	first := &fakeTB{}
	firstTr := Track(first)
	firstTr.SetStartedAt(start)
	firstTr.SetCost(0.25)
	first.failed = true
	first.finish()

	second := &fakeTB{}
	secondTr := Track(second)
	secondTr.SetStartedAt(start.Add(5 * time.Second))
	secondTr.SetCost(0.10)
	second.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger file not written: %v", err)
	}
	row, ok := l.Get("test:live:aggregate-probe")
	if !ok {
		t.Fatal("no row written")
	}
	if row.Verdict != VerdictFail {
		t.Fatalf("fail-then-pass recorded %q — last-write-wins hid the suite's failure", row.Verdict)
	}
	if row.CostUSD < 0.34 || row.CostUSD > 0.36 {
		t.Fatalf("costs did not accumulate: %v, want ~0.25+0.10", row.CostUSD)
	}
}

// The merge must not depend on test order: pass-then-fail fails too
// (the same two-meanings bug, read from the other end).
func TestTrack_AggregateTargetMergesPassThenFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "test:live:aggregate-probe")
	resetProcessWritten()

	first := &fakeTB{}
	firstTr := Track(first)
	firstTr.SetCost(0.10)
	first.finish()

	second := &fakeTB{}
	secondTr := Track(second)
	secondTr.SetCost(0.10)
	second.failed = true
	second.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	row, _ := l.Get("test:live:aggregate-probe")
	if row.Verdict != VerdictFail {
		t.Fatalf("pass-then-fail recorded %q, want fail", row.Verdict)
	}
	if row.CostUSD < 0.19 || row.CostUSD > 0.21 {
		t.Fatalf("costs did not accumulate: %v, want ~0.20", row.CostUSD)
	}
}

// Different targets on the same ledger file are independent rows — the
// merge registry is keyed per target, and a target run alone never sees
// another target's verdict or metrics.
func TestTrack_DifferentTargetsStayIndependent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	resetProcessWritten()

	t.Setenv(EnvTarget, "test:live:probe-a")
	a := &fakeTB{}
	aTr := Track(a)
	aTr.SetCost(0.10)
	a.finish()

	t.Setenv(EnvTarget, "test:live:probe-b")
	b := &fakeTB{}
	bTr := Track(b)
	bTr.SetCost(0.20)
	b.failed = true
	b.finish()

	l, err := Load(path)
	if err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
	rowA, okA := l.Get("test:live:probe-a")
	rowB, okB := l.Get("test:live:probe-b")
	if !okA || !okB {
		t.Fatalf("missing rows: a=%v b=%v", okA, okB)
	}
	if rowA.Verdict != VerdictPass || rowA.CostUSD < 0.09 || rowA.CostUSD > 0.11 {
		t.Fatalf("probe-a contaminated: %+v", rowA)
	}
	if rowB.Verdict != VerdictFail || rowB.CostUSD < 0.19 || rowB.CostUSD > 0.21 {
		t.Fatalf("probe-b contaminated: %+v", rowB)
	}
}

// OFFLINE mode (unit tests, offline probes) disables the writer: no
// cleanup registration, no file.
func TestTrack_OfflineDisablesWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvOffline, "true")
	t.Setenv(EnvTarget, "test:live:probe")

	tr := Track(t)
	if !tr.disabled {
		t.Fatal("ITERION_LIVE_LEDGER_OFFLINE=true did not disable the tracker")
	}
}

// A missing target env leaves the tracker disabled and writes nothing —
// the Logf line is the only trace, so the operator can see why the row
// was not updated.
func TestTrack_MissingTargetEnvWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvTarget, "")
	resetProcessWritten()

	fake := &fakeTB{}
	tr := Track(fake)
	if !tr.disabled {
		t.Fatal("missing target env did not disable the tracker")
	}
	fake.finish()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled tracker wrote a file: %v", err)
	}
}

// mergeRows folds two rows of one target from one process; the exact
// arithmetic is what the aggregate rows rely on: verdicts fail-dominant,
// durations sum, and a never carries no verdict to contaminate the pair.
func TestMergeRows_Arithmetic(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := time.Unix(1_700_000_100, 0).UTC()
	fail5 := Row{Target: "t", Verdict: VerdictFail, LastRun: t0, DurationSec: 5, CostUSD: 0.5}
	pass7 := Row{Target: "t", Verdict: VerdictPass, LastRun: t1, DurationSec: 7, CostUSD: 0.7}
	got := mergeRows(fail5, pass7)
	if got.Verdict != VerdictFail || got.DurationSec != 12 || got.CostUSD < 1.19 || got.CostUSD > 1.21 || !got.LastRun.Equal(t1) {
		t.Fatalf("fail5+pass7 = %+v, want fail, dur 12, cost ~1.2, lastRun t1", got)
	}
	// Order-independent verdict: pass7+fail5 fails too.
	if got := mergeRows(pass7, fail5); got.Verdict != VerdictFail {
		t.Fatalf("pass7+fail5 = %q, want fail", got.Verdict)
	}
	never := Row{Target: "t", Verdict: VerdictNever}
	if got := mergeRows(never, pass7); got.Verdict != VerdictPass {
		t.Fatalf("never+pass7 = %q, want pass", got.Verdict)
	}
}

// The Tracker's SetCost / SetStartedAt must be safe under the harness
// pattern of "update as the run progresses", including from goroutines
// (a metrics collector may run off the main test goroutine). The race
// detector is what proves this — this test's value is being run under
// -race with a concurrent SetCost.
func TestTracker_SetCost_ConcurrentIsRaceSafe(t *testing.T) {
	tr := &Tracker{target: "test:live:probe"}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			tr.SetCost(float64(i) * 0.01)
		}
		close(done)
	}()
	for i := 0; i < 200; i++ {
		tr.SetCost(float64(i) * 0.02)
	}
	<-done
}
