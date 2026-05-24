package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitForIndex polls the store until cond returns true or the timeout
// fires. fsnotify delivery latency varies by OS (sub-millisecond on
// Linux inotify, much longer on macOS FSEvents), so we don't pin a
// single duration — we just poll cheaply until the propagation lands.
func waitForIndex(t *testing.T, s *Store, cond func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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
