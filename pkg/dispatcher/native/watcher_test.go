package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// refuseWatch makes startIndexWatcher see the errno a CI runner at
// fs.inotify.max_user_watches / max_user_instances really returns, so the
// degraded path is exercised on any host instead of only on an exhausted one.
func refuseWatch(t *testing.T) {
	t.Helper()
	setSeam(t, &newFSWatcher, func() (*fsnotify.Watcher, error) {
		return nil, &os.SyscallError{Syscall: "inotify_init1", Err: syscall.EMFILE}
	})
}

// fastPathBudget is how long waitForIndex gives the fsnotify fast path
// before it stops waiting and asks the deterministic oracle instead. It
// is NOT the pass/fail line — see waitForIndex. Measured propagation on
// an idle Linux host is p99 1.3ms, so this is ~7000x the observed cost;
// widening it further has never turned a red run green, because the
// failure it guards is an event that is never sent, not one that is late.
const fastPathBudget = 10 * time.Second

// waitForIndex asserts the invariant the index machinery guarantees:
//
//	a write another process makes under issues/ becomes visible
//	through the store's index.
//
// The store keeps that promise with two carriers — the fsnotify fast
// path, and the Reconcile net when the host refused to arm a watch — and
// this helper follows whichever carrier the store actually selected, so
// the assertion never evaporates into a skip and never rests on a clock
// band.
//
//   - No watch armed (a CI runner at fs.inotify.max_user_watches):
//     the net is the carrier. One Reconcile, one assertion, zero clock.
//   - Watch armed: wait for the event, then use Reconcile as the ORACLE
//     that tells the two failure modes apart — "the write landed and the
//     fast path dropped it" vs "the write or the id mapping is wrong".
//     Either way the verdict names a mechanism, not a duration.
func waitForIndex(t *testing.T, s *Store, cond func() bool, label string) {
	t.Helper()
	check := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return cond()
	}

	if w, _, werr := s.watchState(); w == nil {
		if werr == nil {
			t.Fatalf("watcher: %s: no watcher and no recorded reason — startIndexWatcher returned (nil, nil)", label)
		}
		// The host would not give us a watch. That says nothing about
		// iterion's code, but the store still owes visibility, so assert
		// the net instead of skipping the guard.
		if err := s.Reconcile(); err != nil {
			t.Fatalf("watcher: %s: host refused a watch (%v) and the reconcile net failed: %v", label, werr, err)
		}
		if !check() {
			t.Fatalf("watcher: %s: host refused a watch (%v) AND the reconcile net did not make the change visible", label, werr)
		}
		return
	}

	deadline := time.Now().Add(fastPathBudget)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The fast path did not deliver. Ask the oracle what is actually
	// wrong rather than reporting the clock.
	if err := s.Reconcile(); err != nil {
		t.Fatalf("watcher: %s: no fast-path delivery in %s, and the reconcile oracle failed: %v", label, fastPathBudget, err)
	}
	if check() {
		t.Fatalf("watcher: %s: the watch was armed but no event reached the index in %s — a reconcile made the change visible, so the write landed and the fsnotify path dropped it (loop / relevantEvent / idFromIssuePath / applyEvent)", label, fastPathBudget)
	}
	t.Fatalf("watcher: %s: still not visible after a full reconcile — the write or the id encoding is wrong, not the watcher", label)
}

// TestWatcher_PicksUpExternalCreate is the bug-fix regression guard:
// a sibling process (whats-next's `iterion __mcp-board` MCP subprocess
// in production, an os.WriteFile here) drops an issue JSON file in
// issues/ behind the parent Store's back. Without the watcher the
// new issue stays invisible to List/Get until the daemon restarts.
func TestWatcher_PicksUpExternalCreate(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	iss := Issue{
		ID:        "native:ext-create-1",
		Title:     "External create",
		State:     "backlog",
		CreatedAt: now,
		UpdatedAt: now,
	}
	data, err := json.MarshalIndent(&iss, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(dir, issuesDir, encodeID(iss.ID)+".json")
	if err := os.WriteFile(path, data, filePerm); err != nil {
		t.Fatalf("write external issue: %v", err)
	}

	waitForIndex(t, s, func() bool {
		_, ok := s.index[iss.ID]
		return ok
	}, "external create")

	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get post-watch: %v", err)
	}
	if got.Title != iss.Title {
		t.Errorf("Get title = %q, want %q", got.Title, iss.Title)
	}
}

// TestWatcher_PicksUpExternalRemove validates the inverse path: an
// out-of-process delete should drop the entry from the index so
// stale-but-cached reads stop returning a tombstoned issue.
func TestWatcher_PicksUpExternalRemove(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	created, err := s.Create(Issue{
		Title: "Will be deleted externally",
		State: "backlog",
	})
	if err != nil {
		t.Fatalf("Create.delete: %v", err)
	}

	if err := os.Remove(s.issuePath(created.ID)); err != nil {
		t.Fatalf("external remove: %v", err)
	}

	waitForIndex(t, s, func() bool {
		_, ok := s.index[created.ID]
		return !ok
	}, "external remove")
}

// TestWatcher_PicksUpExternalUpdate covers the case where an external
// writer overwrites an existing file — fsnotify reports a Write event
// (sometimes Create+Write on Linux) and the watcher must reload the
// fresh state, not keep serving the cached older version.
func TestWatcher_PicksUpExternalUpdate(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	created, err := s.Create(Issue{
		Title: "Pre-update title",
		State: "backlog",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// External writer bumps the title without going through Update.
	created.Title = "Post-update external title"
	data, err := json.MarshalIndent(created, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(s.issuePath(created.ID), data, filePerm); err != nil {
		t.Fatalf("external write: %v", err)
	}

	waitForIndex(t, s, func() bool {
		iss, ok := s.index[created.ID]
		return ok && iss.Title == "Post-update external title"
	}, "external update")
}

// TestWatcher_ExternalCreateVisibleWhenWatchRefused is the guard against
// the regression guards evaporating: on a host that will not arm a watch
// — the CI condition behind this test's history of flakes — the store
// still owes out-of-process visibility, and this asserts it gets kept.
// Before the reconciliation net this scenario could only time out.
func TestWatcher_ExternalCreateVisibleWhenWatchRefused(t *testing.T) {
	refuseWatch(t)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if w, _, _ := s.watchState(); w != nil {
		t.Fatal("test setup: expected the watch to be refused")
	}

	now := time.Now().UTC().Truncate(time.Second)
	iss := Issue{ID: "native:refused-create", Title: "External create", State: "backlog", CreatedAt: now, UpdatedAt: now}
	data, err := json.MarshalIndent(&iss, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, issuesDir, encodeID(iss.ID)+".json"), data, filePerm); err != nil {
		t.Fatalf("write external issue: %v", err)
	}

	waitForIndex(t, s, func() bool {
		_, ok := s.index[iss.ID]
		return ok
	}, "external create (watch refused)")
}

// TestStore_ReconcileSeesExternalWrites pins the reconciliation net on
// its own, with no watcher and no clock: cause, effect, assertion. This
// is the test that can never flake, and it is what makes the fast-path
// guards above safe to keep — a host that cannot arm a watch no longer
// costs the suite its statement about out-of-process visibility.
func TestStore_ReconcileSeesExternalWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Take the fast path out of the picture entirely: the net alone
	// must carry all three shapes of out-of-process change.
	if w, _, _ := s.watchState(); w != nil {
		if err := w.Close(); err != nil {
			t.Fatalf("close watcher: %v", err)
		}
		s.mu.Lock()
		s.watcher = nil
		s.mu.Unlock()
	}

	doomed, err := s.Create(Issue{Title: "Removed externally", State: "backlog"})
	if err != nil {
		t.Fatalf("Create doomed: %v", err)
	}
	edited, err := s.Create(Issue{Title: "Before", State: "backlog"})
	if err != nil {
		t.Fatalf("Create edited: %v", err)
	}

	// create
	now := time.Now().UTC().Truncate(time.Second)
	fresh := Issue{ID: "native:reconcile-new", Title: "Appeared", State: "backlog", CreatedAt: now, UpdatedAt: now}
	data, err := json.MarshalIndent(&fresh, "", "  ")
	if err != nil {
		t.Fatalf("marshal fresh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, issuesDir, encodeID(fresh.ID)+".json"), data, filePerm); err != nil {
		t.Fatalf("write fresh: %v", err)
	}
	// update
	edited.Title = "After"
	data, err = json.MarshalIndent(edited, "", "  ")
	if err != nil {
		t.Fatalf("marshal edited: %v", err)
	}
	if err := os.WriteFile(s.issuePath(edited.ID), data, filePerm); err != nil {
		t.Fatalf("write edited: %v", err)
	}
	// remove
	if err := os.Remove(s.issuePath(doomed.ID)); err != nil {
		t.Fatalf("remove doomed: %v", err)
	}

	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	s.mu.Lock()
	_, sawFresh := s.index[fresh.ID]
	got, sawEdited := s.index[edited.ID]
	_, sawDoomed := s.index[doomed.ID]
	s.mu.Unlock()

	if !sawFresh {
		t.Errorf("reconcile: external create not visible")
	}
	if !sawEdited || got.Title != "After" {
		t.Errorf("reconcile: external update not visible (ok=%v title=%q)", sawEdited, got.Title)
	}
	if sawDoomed {
		t.Errorf("reconcile: external remove left a tombstone in the index")
	}
}

// TestApplyEvent_DropsEntryWhenFileVanished pins applyEvent's
// "file disappeared between the event and our read" branch. It matched on
// fs.ErrNotExist while readIssueFromDisk maps a missing file to
// tracker.ErrNotFound, which does not wrap it — so the branch never fired
// and a vanished issue kept serving from the index.
func TestApplyEvent_DropsEntryWhenFileVanished(t *testing.T) {
	// Isolate applyEvent: no live watcher and no rescan may touch the
	// index, or they would do the dropping this test is here to check.
	refuseWatch(t)
	setRescanInterval(t, 0)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	created, err := s.Create(Issue{Title: "Vanishes", State: "backlog"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.Remove(s.issuePath(created.ID)); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// A Write event for a file that is already gone — the shape a
	// remove racing its own event produces.
	applyEvent(s, created.ID, fsnotify.Write)

	s.mu.Lock()
	_, still := s.index[created.ID]
	s.mu.Unlock()
	if still {
		t.Error("applyEvent kept an index entry whose file is gone")
	}
}

// TestStore_FallbackRescanWhenWatchRefused is the production half: a
// store the host would not give a watch must still converge on its own,
// instead of serving its NewStore snapshot until the daemon restarts.
func TestStore_FallbackRescanWhenWatchRefused(t *testing.T) {
	setRescanInterval(t, 20*time.Millisecond)

	refuseWatch(t)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	w, r, _ := s.watchState()
	if w != nil {
		t.Fatal("test setup: expected the watch to be refused")
	}
	if r == nil {
		t.Fatal("watch refused but no fallback rescan started: the store is blind until restart")
	}

	now := time.Now().UTC().Truncate(time.Second)
	iss := Issue{ID: "native:rescan-1", Title: "Seen by the net", State: "backlog", CreatedAt: now, UpdatedAt: now}
	data, err := json.MarshalIndent(&iss, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, issuesDir, encodeID(iss.ID)+".json"), data, filePerm); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(fastPathBudget)
	for time.Now().Before(deadline) {
		if _, err := s.Get(iss.ID); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fallback rescan never made an out-of-process create visible")
}
