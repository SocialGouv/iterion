package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// waitForIndex polls the store until cond returns true or the timeout
// fires. fsnotify delivery latency varies by OS (sub-millisecond on
// Linux inotify, much longer on macOS FSEvents), so we don't pin a
// single duration — we just poll cheaply until the propagation lands.
func waitForIndex(t *testing.T, s *Store, cond func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		ok := cond()
		s.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watcher: %s did not propagate within deadline", label)
}

// requireIndexWatcher keeps these behavioural tests honest on hosts where
// fsnotify cannot allocate another watcher (for example a busy CI runner that
// reached its inotify limit). NewStore deliberately degrades to a usable store
// in that situation, so lack of host CAPACITY is a skip.
//
// It is deliberately not "skip whenever the watcher is nil": a code regression
// that stops the watcher from starting produces the very same nil, and would
// silently turn all three regression guards into no-ops — the failure mode a
// skip-on-error test exists to avoid. Only an errno that means "this host
// cannot allocate one right now" skips; anything else fails, including a nil
// watcher with no recorded reason.
func requireIndexWatcher(t *testing.T, s *Store) {
	t.Helper()
	if s.watcher != nil {
		return
	}
	if s.watcherErr == nil {
		t.Fatal("no fsnotify watcher and no recorded reason: startIndexWatcher returned (nil, nil)")
	}
	if isHostWatcherExhaustion(s.watcherErr) {
		t.Skipf("host cannot allocate an fsnotify watcher: %v", s.watcherErr)
	}
	t.Fatalf("fsnotify watcher did not start, and not for want of host capacity: %v", s.watcherErr)
}

// isHostWatcherExhaustion reports the errnos that mean "this host is out of
// watch descriptors / file descriptors / memory" — the only conditions under
// which an absent watcher says nothing about iterion's own code. ENOSPC is the
// one inotify actually returns when /proc/sys/fs/inotify/max_user_watches is
// reached, which is the CI symptom this skip was written for.
func isHostWatcherExhaustion(err error) bool {
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
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
	requireIndexWatcher(t, s)

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
	requireIndexWatcher(t, s)

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
	requireIndexWatcher(t, s)

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

// TestIsHostWatcherExhaustion pins the line the skip rests on. Getting it
// wrong in the permissive direction is what the whole change is about: an
// over-broad predicate turns the three watcher regression guards above into
// no-ops the moment the watcher stops starting for a reason of our own.
func TestIsHostWatcherExhaustion(t *testing.T) {
	// The real inotify symptom on a CI runner at max_user_watches.
	if !isHostWatcherExhaustion(fmt.Errorf("inotify_add_watch: %w", syscall.ENOSPC)) {
		t.Error("ENOSPC is the host running out of inotify watches; it must skip")
	}
	if !isHostWatcherExhaustion(fmt.Errorf("open: %w", syscall.EMFILE)) {
		t.Error("EMFILE is the process running out of descriptors; it must skip")
	}
	// Everything else is ours to answer for.
	if isHostWatcherExhaustion(errors.New("watcher never started")) {
		t.Error("an unclassified error must fail the test, not skip it")
	}
	if isHostWatcherExhaustion(fmt.Errorf("add: %w", syscall.EACCES)) {
		t.Error("a permission error is a real defect (or a misconfigured store), not a host capacity limit")
	}
	if isHostWatcherExhaustion(nil) {
		t.Error("a nil error must not read as host exhaustion")
	}
}
