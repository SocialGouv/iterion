// Package storetest exposes the conformance suite that every
// store.RunStore backend must satisfy. It lives in its own package
// (rather than as a *_test.go helper) so backend tests in sibling
// packages — notably pkg/store/mongo — can plug a backend-specific
// factory into the same assertions.
//
// The suite covers: CreateRun→LoadRun round-trip, status transitions
// + FinishedAt clamping at terminals, AppendEvent monotone seq under
// sequential AND concurrent writers, WriteArtifact version ordering,
// LockRun exclusivity across an Unlock boundary, and Capabilities()
// non-emptiness.
package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Factory returns a fresh, empty RunStore for one subtest. Cleanup
// (t.TempDir, drop database, etc.) is the factory's job.
type Factory func(t *testing.T) store.RunStore

// testCtx returns a context carrying a synthetic tenant + owner. The
// mongo backend's fail-closed withTenantFilter guard panics on
// tenant-scoped queries that arrive without a tenant in ctx;
// conformance tests speak the normal calling convention (auth
// middleware stamps a tenant) so they exercise the same code path as
// production. Filesystem backend ignores the values.
func testCtx() context.Context {
	return store.WithIdentity(context.Background(), "_test", "_test")
}

// Opts let backends declare what behaviour the harness should expect
// when it differs from the filesystem default.
type Opts struct {
	// InitialStatus is the status CreateRun is expected to set. FS
	// starts at "running" (engine takes ownership immediately);
	// Mongo starts at "queued" because the runner pod claims the
	// run asynchronously.
	InitialStatus store.RunStatus
}

// Default returns a sensible baseline matching the filesystem
// backend. New backends override per-field as needed.
func Default() Opts {
	return Opts{InitialStatus: store.RunStatusRunning}
}

// Run executes the full conformance suite against factory.
func Run(t *testing.T, factory Factory) {
	RunWithOpts(t, factory, Default())
}

// RunWithOpts executes the full conformance suite with backend-
// specific overrides.
func RunWithOpts(t *testing.T, factory Factory, opts Opts) {
	t.Run("CreateLoadRoundTrip", func(t *testing.T) { testCreateLoad(t, factory(t), opts) })
	t.Run("StatusTransitions", func(t *testing.T) { testStatusTransitions(t, factory(t)) })
	t.Run("EventSeqMonotone", func(t *testing.T) { testEventSeqMonotone(t, factory(t)) })
	t.Run("EventSeqUnderConcurrency", func(t *testing.T) { testEventSeqConcurrent(t, factory(t)) })
	t.Run("ArtifactVersionsMonotone", func(t *testing.T) { testArtifactVersions(t, factory(t)) })
	t.Run("LockExclusivity", func(t *testing.T) { testLockExclusive(t, factory(t)) })
	t.Run("CapabilitiesReported", func(t *testing.T) { testCapabilitiesReported(t, factory(t)) })
	t.Run("UserMessagesInbox", func(t *testing.T) { testUserMessagesInbox(t, factory(t)) })
	t.Run("WatchedIssues", func(t *testing.T) { testWatchedIssues(t, factory(t)) })
	t.Run("DeleteRun", func(t *testing.T) { testDeleteRun(t, factory(t)) })
	t.Run("RunLogStore", func(t *testing.T) { testRunLogStore(t, factory(t)) })
}

// testRunLogStore exercises the optional RunLogStore surface (ADR-053)
// when the backend implements it: append/read/size round-trip, offset
// windows crossing chunk boundaries, idempotent duplicate-offset
// redelivery, and empty-log semantics.
func testRunLogStore(t *testing.T, s store.RunStore) {
	t.Helper()
	ls := store.AsRunLogStore(s)
	if ls == nil {
		t.Skip("backend does not implement RunLogStore")
	}
	ctx := testCtx()
	const runID = "run_logstore"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Empty log: size 0, nil range.
	if n, err := ls.RunLogSize(ctx, runID); err != nil || n != 0 {
		t.Fatalf("RunLogSize(empty) = %d, %v; want 0, nil", n, err)
	}
	if data, err := ls.ReadRunLogRange(ctx, runID, 0, 0); err != nil || len(data) != 0 {
		t.Fatalf("ReadRunLogRange(empty) = %q, %v; want empty, nil", data, err)
	}

	// Sequential appends.
	if err := ls.AppendRunLog(ctx, runID, 0, []byte("hello ")); err != nil {
		t.Fatalf("AppendRunLog #1: %v", err)
	}
	if err := ls.AppendRunLog(ctx, runID, 6, []byte("world")); err != nil {
		t.Fatalf("AppendRunLog #2: %v", err)
	}
	if n, err := ls.RunLogSize(ctx, runID); err != nil || n != 11 {
		t.Fatalf("RunLogSize = %d, %v; want 11, nil", n, err)
	}
	if data, err := ls.ReadRunLogRange(ctx, runID, 0, 0); err != nil || string(data) != "hello world" {
		t.Fatalf("ReadRunLogRange(all) = %q, %v; want %q", data, err, "hello world")
	}
	// Window crossing the chunk boundary, sliced on both edges.
	if data, err := ls.ReadRunLogRange(ctx, runID, 4, 9); err != nil || string(data) != "o wor" {
		t.Fatalf("ReadRunLogRange(4,9) = %q, %v; want %q", data, err, "o wor")
	}
	// From-offset to end.
	if data, err := ls.ReadRunLogRange(ctx, runID, 6, 0); err != nil || string(data) != "world" {
		t.Fatalf("ReadRunLogRange(6,0) = %q, %v; want %q", data, err, "world")
	}
	// Past-the-end window.
	if data, err := ls.ReadRunLogRange(ctx, runID, 11, 0); err != nil || len(data) != 0 {
		t.Fatalf("ReadRunLogRange(past end) = %q, %v; want empty, nil", data, err)
	}

	// Idempotent redelivery: re-appending an already-persisted chunk at
	// the same offset must succeed without corrupting the stream.
	if err := ls.AppendRunLog(ctx, runID, 6, []byte("world")); err != nil {
		t.Fatalf("AppendRunLog(duplicate) = %v; want idempotent nil", err)
	}
	if data, err := ls.ReadRunLogRange(ctx, runID, 0, 0); err != nil || string(data) != "hello world" {
		t.Fatalf("after duplicate append: ReadRunLogRange = %q, %v; want %q", data, err, "hello world")
	}
	if n, err := ls.RunLogSize(ctx, runID); err != nil || n != 11 {
		t.Fatalf("after duplicate append: RunLogSize = %d, %v; want 11", n, err)
	}
}

// testDeleteRun proves DeleteRun removes a run (with events) from both
// LoadRun and ListRuns, leaves a sibling run untouched, and is idempotent.
func testDeleteRun(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	for _, id := range []string{"run_del", "run_keep"} {
		if _, err := s.CreateRun(ctx, id, "demo", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if _, err := s.AppendEvent(ctx, id, store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}); err != nil {
			t.Fatalf("AppendEvent %s: %v", id, err)
		}
	}

	if err := s.DeleteRun(ctx, "run_del"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := s.LoadRun(ctx, "run_del"); err == nil {
		t.Error("LoadRun after delete: want error, got nil")
	}
	ids, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == "run_del" {
			t.Errorf("ListRuns still contains deleted run: %v", ids)
		}
	}
	if !containsStr(ids, "run_keep") {
		t.Errorf("ListRuns dropped the sibling run_keep: %v", ids)
	}
	// Sibling's data survives.
	if _, err := s.LoadRun(ctx, "run_keep"); err != nil {
		t.Errorf("sibling run_keep gone after deleting run_del: %v", err)
	}
	// Idempotent: deleting an already-gone run is a no-op.
	if err := s.DeleteRun(ctx, "run_del"); err != nil {
		t.Errorf("second DeleteRun should be a no-op, got: %v", err)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func testWatchedIssues(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_watch", "demo", nil); err != nil {
		t.Fatal(err)
	}

	// Add merges + dedups, preserving insertion order.
	got, err := s.AddWatchedIssues(ctx, "run_watch", []string{"a", "b", "a", ""})
	if err != nil {
		t.Fatalf("AddWatchedIssues: %v", err)
	}
	if !sameSet(got, []string{"a", "b"}) {
		t.Errorf("after add: got %v want [a b]", got)
	}

	// A second add only appends the new entry.
	got, err = s.AddWatchedIssues(ctx, "run_watch", []string{"b", "c"})
	if err != nil {
		t.Fatalf("AddWatchedIssues #2: %v", err)
	}
	if !sameSet(got, []string{"a", "b", "c"}) {
		t.Errorf("after add #2: got %v want [a b c]", got)
	}

	// Persisted on the run record.
	r, err := s.LoadRun(ctx, "run_watch")
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(r.WatchedIssueIDs, []string{"a", "b", "c"}) {
		t.Errorf("LoadRun watched: got %v want [a b c]", r.WatchedIssueIDs)
	}

	// Remove drops the named entries.
	got, err = s.RemoveWatchedIssues(ctx, "run_watch", []string{"b", "missing"})
	if err != nil {
		t.Fatalf("RemoveWatchedIssues: %v", err)
	}
	if !sameSet(got, []string{"a", "c"}) {
		t.Errorf("after remove: got %v want [a c]", got)
	}

	// Removing the rest leaves an empty set.
	got, err = s.RemoveWatchedIssues(ctx, "run_watch", []string{"a", "c"})
	if err != nil {
		t.Fatalf("RemoveWatchedIssues #2: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("after remove all: got %v want empty", got)
	}
}

// sameSet reports whether got and want contain the same elements,
// order-insensitive (Mongo's $addToSet does not guarantee ordering).
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

func testCreateLoad(t *testing.T, s store.RunStore, opts Opts) {
	t.Helper()
	in := map[string]interface{}{"foo": "bar"}
	r, err := s.CreateRun(testCtx(), "run_1", "demo", in)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if r.ID != "run_1" {
		t.Errorf("ID: got %q", r.ID)
	}
	if r.Status != opts.InitialStatus {
		t.Errorf("Status: got %q want %q", r.Status, opts.InitialStatus)
	}
	r2, err := s.LoadRun(testCtx(), "run_1")
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

func testStatusTransitions(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_2", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(testCtx(), "run_2", store.RunStatusFinished, ""); err != nil {
		t.Fatal(err)
	}
	r, _ := s.LoadRun(testCtx(), "run_2")
	if r.Status != store.RunStatusFinished {
		t.Errorf("Status: got %q", r.Status)
	}
	if r.FinishedAt == nil {
		t.Errorf("FinishedAt: expected set on terminal status")
	}
}

func testEventSeqMonotone(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_3", "demo", nil); err != nil {
		t.Fatal(err)
	}
	const N = 50
	var prev int64 = -1
	for i := 0; i < N; i++ {
		ev := store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}
		written, err := s.AppendEvent(testCtx(), "run_3", ev)
		if err != nil {
			t.Fatalf("AppendEvent #%d: %v", i, err)
		}
		if written.Seq <= prev {
			t.Errorf("Seq #%d: %d not strictly greater than prev %d", i, written.Seq, prev)
		}
		prev = written.Seq
	}
	all, err := s.LoadEvents(testCtx(), "run_3")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != N {
		t.Errorf("LoadEvents: got %d want %d", len(all), N)
	}
}

func testEventSeqConcurrent(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_4", "demo", nil); err != nil {
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
				ev := store.Event{Type: store.EventNodeStarted, Timestamp: time.Now().UTC()}
				if _, err := s.AppendEvent(testCtx(), "run_4", ev); err != nil {
					t.Errorf("AppendEvent: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	all, err := s.LoadEvents(testCtx(), "run_4")
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
		if ev.Seq < 0 {
			t.Errorf("negative seq at index %d: %d", i, ev.Seq)
		}
	}
}

func testArtifactVersions(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_5", "demo", nil); err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 3; v++ {
		if err := s.WriteArtifact(testCtx(), &store.Artifact{
			RunID:     "run_5",
			NodeID:    "node_a",
			Version:   v,
			Data:      map[string]interface{}{"v": v},
			WrittenAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("WriteArtifact v=%d: %v", v, err)
		}
	}
	versions, err := s.ListArtifactVersions(testCtx(), "run_5", "node_a")
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
	latest, err := s.LoadLatestArtifact(testCtx(), "run_5", "node_a")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 3 {
		t.Errorf("Latest version: got %d want 3", latest.Version)
	}
}

func testLockExclusive(t *testing.T, s store.RunStore) {
	t.Helper()
	if _, err := s.CreateRun(testCtx(), "run_6", "demo", nil); err != nil {
		t.Fatal(err)
	}
	first, err := s.LockRun(testCtx(), "run_6")
	if err != nil {
		t.Fatalf("first LockRun: %v", err)
	}
	if err := first.Unlock(); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	second, err := s.LockRun(testCtx(), "run_6")
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Errorf("second Unlock: %v", err)
	}
}

func testCapabilitiesReported(t *testing.T, s store.RunStore) {
	t.Helper()
	caps := s.Capabilities()
	if !caps.LiveStream && !caps.CrossProcessLock && !caps.PIDFile && !caps.GitWorktree {
		t.Errorf("Capabilities all-false; backend must report at least one")
	}
}

func testUserMessagesInbox(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	if _, err := s.CreateRun(ctx, "run_um", "demo", nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// FIFO: append three messages and ensure load returns them in
	// queued_at order even when wall-clock order is reversed.
	msgs := []store.QueuedUserMessage{
		{ID: "m1", Text: "first", QueuedAt: now.Add(0)},
		{ID: "m2", Text: "second", QueuedAt: now.Add(10 * time.Millisecond)},
		{ID: "m3", Text: "third", QueuedAt: now.Add(20 * time.Millisecond)},
	}
	for _, m := range msgs {
		if err := s.AppendQueuedMessage(ctx, "run_um", m); err != nil {
			t.Fatalf("AppendQueuedMessage(%s): %v", m.ID, err)
		}
	}
	pending, err := s.LoadPendingQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("Pending count: got %d want 3", len(pending))
	}
	for i, want := range []string{"m1", "m2", "m3"} {
		if pending[i].ID != want {
			t.Errorf("FIFO[%d]: got %q want %q", i, pending[i].ID, want)
		}
		if pending[i].Status != store.QueuedMessageStatusQueued {
			t.Errorf("Initial status[%s]: got %q want queued", pending[i].ID, pending[i].Status)
		}
	}
	// Deliver m1, m2 and re-check pending: m3 only.
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m1", store.QueuedMessageStatusDelivered); err != nil {
		t.Fatalf("Deliver m1: %v", err)
	}
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m2", store.QueuedMessageStatusDelivered); err != nil {
		t.Fatalf("Deliver m2: %v", err)
	}
	pending, err = s.LoadPendingQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("LoadPending after deliver: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "m3" {
		t.Fatalf("Pending after deliver = %+v, want only m3", pending)
	}
	// ListQueuedMessages returns ALL three, FIFO, with current
	// statuses preserved.
	all, err := s.ListQueuedMessages(ctx, "run_um")
	if err != nil {
		t.Fatalf("ListQueuedMessages: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List count: got %d want 3", len(all))
	}
	if all[0].Status != store.QueuedMessageStatusDelivered ||
		all[1].Status != store.QueuedMessageStatusDelivered ||
		all[2].Status != store.QueuedMessageStatusQueued {
		t.Errorf("statuses: %v / %v / %v", all[0].Status, all[1].Status, all[2].Status)
	}
	// Cancellation only valid from "queued". Cancel m3 OK; cancelling
	// m1 (delivered) must fail with the conflict sentinel.
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "m3", store.QueuedMessageStatusCancelled, store.QueuedMessageStatusQueued); err != nil {
		t.Fatalf("Cancel m3: %v", err)
	}
	err = s.UpdateQueuedMessageStatus(ctx, "run_um", "m1", store.QueuedMessageStatusCancelled, store.QueuedMessageStatusQueued)
	if err == nil {
		t.Fatalf("Cancel of delivered m1: expected error")
	}
	// Updating an unknown ID returns ErrQueuedMessageNotFound.
	if err := s.UpdateQueuedMessageStatus(ctx, "run_um", "nonexistent", store.QueuedMessageStatusDelivered); err == nil {
		t.Fatalf("Update nonexistent: expected error")
	}
}
