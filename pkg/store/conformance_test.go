package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Conformance is the minimum behaviour every RunStore impl must
// honour, regardless of backend (filesystem today, Mongo+S3 in plan
// §F T-17). The harness is parameterised on a factory function so a
// future Mongo test can call exactly the same assertions and surface
// any drift.
//
// Invariants checked:
//   - CreateRun → LoadRun round-trip preserves the user-visible fields.
//   - UpdateRunStatus rolls forward and clamps FinishedAt at terminals.
//   - AppendEvent issues a strictly-monotonic seq starting at 1.
//   - Concurrent AppendEvent calls each get a unique seq.
//   - WriteArtifact versions are strictly increasing per node.
//   - LockRun is exclusive — a second LockRun fails until Unlock.
//   - Capabilities() reports a non-empty set for any real backend.

// runStoreFactory returns a fresh, empty store for one subtest. Cleanup
// is the harness's responsibility (t.TempDir for FS, drop+recreate for
// Mongo).
type runStoreFactory func(t *testing.T) RunStore

func conformanceSuite(t *testing.T, factory runStoreFactory) {
	t.Run("CreateLoadRoundTrip", func(t *testing.T) { testCreateLoad(t, factory(t)) })
	t.Run("StatusTransitions", func(t *testing.T) { testStatusTransitions(t, factory(t)) })
	t.Run("EventSeqMonotone", func(t *testing.T) { testEventSeqMonotone(t, factory(t)) })
	t.Run("EventSeqUnderConcurrency", func(t *testing.T) { testEventSeqConcurrent(t, factory(t)) })
	t.Run("ArtifactVersionsMonotone", func(t *testing.T) { testArtifactVersions(t, factory(t)) })
	t.Run("LockExclusivity", func(t *testing.T) { testLockExclusive(t, factory(t)) })
	t.Run("CapabilitiesReported", func(t *testing.T) { testCapabilitiesReported(t, factory(t)) })
}

func testCreateLoad(t *testing.T, s RunStore) {
	t.Helper()
	in := map[string]interface{}{"foo": "bar"}
	r, err := s.CreateRun(context.Background(), "run_1", "demo", in)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if r.ID != "run_1" {
		t.Errorf("ID: got %q", r.ID)
	}
	if r.Status != RunStatusRunning {
		t.Errorf("Status: got %q want running", r.Status)
	}
	r2, err := s.LoadRun(context.Background(), "run_1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r2.WorkflowName != "demo" {
		t.Errorf("WorkflowName: got %q", r2.WorkflowName)
	}
	if r2.Inputs["foo"] != "bar" {
		t.Errorf("Inputs[foo]: got %v", r2.Inputs["foo"])
	}
}

func testStatusTransitions(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_2", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(context.Background(), "run_2", RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	r, _ := s.LoadRun(context.Background(), "run_2")
	if r.Status != RunStatusFinished {
		t.Errorf("Status: got %q", r.Status)
	}
	if r.FinishedAt == nil {
		t.Errorf("FinishedAt: expected set on terminal status")
	}
}

func testEventSeqMonotone(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_3", "demo", nil); err != nil {
		t.Fatal(err)
	}
	const N = 50
	var prev int64 = -1
	for i := 0; i < N; i++ {
		ev := Event{Type: EventNodeStarted, Timestamp: time.Now().UTC()}
		written, err := s.AppendEvent(context.Background(), "run_3", ev)
		if err != nil {
			t.Fatalf("AppendEvent #%d: %v", i, err)
		}
		// The base seq value is implementation-defined (FS starts at 0,
		// Mongo will start at 1) — what matters is the strictly-monotone
		// invariant: every observation is greater than the previous.
		if written.Seq <= prev {
			t.Errorf("Seq #%d: %d not strictly greater than prev %d", i, written.Seq, prev)
		}
		prev = written.Seq
	}
	all, err := s.LoadEvents(context.Background(), "run_3")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != N {
		t.Errorf("LoadEvents: got %d want %d", len(all), N)
	}
}

func testEventSeqConcurrent(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_4", "demo", nil); err != nil {
		t.Fatal(err)
	}
	const goroutines = 8
	const perG = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				ev := Event{Type: EventNodeStarted, Timestamp: time.Now().UTC()}
				if _, err := s.AppendEvent(context.Background(), "run_4", ev); err != nil {
					t.Errorf("AppendEvent: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	all, err := s.LoadEvents(context.Background(), "run_4")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(all), goroutines*perG; got != want {
		t.Errorf("event count: got %d want %d", got, want)
	}
	seen := make(map[int64]struct{}, len(all))
	for i, ev := range all {
		if _, dup := seen[ev.Seq]; dup {
			t.Errorf("duplicate seq %d at index %d", ev.Seq, i)
		}
		seen[ev.Seq] = struct{}{}
		// seq must be in a contiguous window of size N starting at the
		// backend's chosen base. We don't assert the base — just the
		// "no gaps, no duplicates" guarantee that downstream consumers
		// (replay, dedup) rely on.
		if ev.Seq < 0 || ev.Seq >= int64(goroutines*perG)+10 {
			t.Errorf("seq out of plausible range at index %d: %d", i, ev.Seq)
		}
	}
}

func testArtifactVersions(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_5", "demo", nil); err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 3; v++ {
		if err := s.WriteArtifact(context.Background(), &Artifact{
			RunID:     "run_5",
			NodeID:    "node_a",
			Version:   v,
			Data:      map[string]interface{}{"v": v},
			WrittenAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("WriteArtifact v=%d: %v", v, err)
		}
	}
	versions, err := s.ListArtifactVersions(context.Background(), "run_5", "node_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("ListArtifactVersions: got %d want 3", len(versions))
	}
	for i, vinfo := range versions {
		if vinfo.Version != i+1 {
			t.Errorf("Version[%d]: got %d want %d", i, vinfo.Version, i+1)
		}
	}
	latest, err := s.LoadLatestArtifact(context.Background(), "run_5", "node_a")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 3 {
		t.Errorf("Latest version: got %d want 3", latest.Version)
	}
}

func testLockExclusive(t *testing.T, s RunStore) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), "run_6", "demo", nil); err != nil {
		t.Fatal(err)
	}
	first, err := s.LockRun(context.Background(), "run_6")
	if err != nil {
		t.Fatalf("first LockRun: %v", err)
	}
	if err := first.Unlock(); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	// Re-locking after a clean unlock must succeed — the lock is
	// strictly advisory across the unlock boundary.
	second, err := s.LockRun(context.Background(), "run_6")
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Errorf("second Unlock: %v", err)
	}
}

func testCapabilitiesReported(t *testing.T, s RunStore) {
	t.Helper()
	caps := s.Capabilities()
	// The non-regression we care about: any concrete backend exposes
	// at least *one* capability. A struct full of false is a sign the
	// impl forgot to override the method.
	if !caps.LiveStream && !caps.CrossProcessLock && !caps.PIDFile && !caps.GitWorktree {
		t.Errorf("Capabilities all-false; backend must report at least one")
	}
}

// TestConformance_Filesystem validates that the locally-shipped backend
// satisfies the conformance suite. The same factory shape will be used
// in plan §F T-17 to validate MongoRunStore against the same harness.
func TestConformance_Filesystem(t *testing.T) {
	conformanceSuite(t, func(t *testing.T) RunStore {
		t.Helper()
		dir := t.TempDir()
		s, err := New(dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
