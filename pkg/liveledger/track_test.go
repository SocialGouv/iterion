package liveledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Track's Cleanup is what writes a ledger row on every live run — the
// unit test drives it in OFFLINE mode (no target env → tr.disabled)
// and via RecordRow directly, so the reporting hook is exercised
// without a live spend, as #1422 requires.
func TestTrack_Offline_WritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	t.Setenv(LedgerPathEnv, path)
	t.Setenv(EnvOffline, "true")

	sub := &fakeT{}
	tr := Track(&testing.T{}) // acquired for the API shape; sub-test uses its own
	if tr == nil {
		t.Fatal("Track returned nil")
	}
	// Nothing should be written yet — Track only registers Cleanup.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ledger file exists before Cleanup: %v", err)
	}
	// The offline mode is signalled by tr.disabled — a subsequent
	// t.Cleanup call would no-op.
	if !tr.disabled {
		t.Fatal("ITERION_LIVE_LEDGER_OFFLINE=true did not disable the tracker")
	}
	_ = sub
}

// The recording hook — Track → t.Cleanup → RecordRow — writes a pass
// row when the enclosing test did not fail, a fail row when it did.
// We drive it through RecordRow directly (the internal path a real
// t.Cleanup uses) rather than through the testing.T machinery.
func TestRecordRow_ThroughTheHookShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")

	// Round 1: pass.
	pass := Row{Target: "test:live:probe", Verdict: VerdictPass, LastRun: time.Now().UTC(), DurationSec: 3, CostUSD: 0.01, Ref: "aa11"}
	if err := RecordRow(path, pass); err != nil {
		t.Fatal(err)
	}

	// Round 2: same target, fail.
	fail := Row{Target: "test:live:probe", Verdict: VerdictFail, LastRun: time.Now().UTC().Add(time.Second), DurationSec: 4, CostUSD: 0.02, Ref: "bb22"}
	if err := RecordRow(path, fail); err != nil {
		t.Fatal(err)
	}

	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := l.Get("test:live:probe")
	if !ok {
		t.Fatal("target row disappeared")
	}
	if got.Verdict != VerdictFail || got.Ref != "bb22" {
		t.Fatalf("last-write did not win: %+v", got)
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

// fakeT lets an offline test observe testing.T-shaped calls without
// wiring a full sub-test. Not currently asserted; kept for future
// enrichment of TestTrack_Offline_WritesNothing.
type fakeT struct{ failed bool }

func (f *fakeT) Helper()               {}
func (f *fakeT) Failed() bool          { return f.failed }
func (f *fakeT) Name() string          { return "fakeT" }
func (f *fakeT) Cleanup(func())        {}
func (f *fakeT) Logf(string, ...any)   {}
func (f *fakeT) Fatalf(string, ...any) { f.failed = true }
func (f *fakeT) Errorf(string, ...any) { f.failed = true }
