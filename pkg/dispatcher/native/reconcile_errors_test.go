package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A card whose file is present but momentarily unreadable (EACCES here;
// EIO or EMFILE on the fd-starved host the net is armed on) must not
// vanish from the index at the next rescan: the rebuild keeps the entry it
// already holds, as applyEvent does — a stale-but-readable card beats a
// forced 404. Only a file that is GONE leaves the index.
func TestReconcile_KeepsACardWhoseFileIsMomentarilyUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 000 file; the unreadable case cannot be staged")
	}
	refuseWatch(t)
	setRescanInterval(t, 0)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	kept, err := s.Create(Issue{Title: "Unreadable for a moment", State: "backlog"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gone, err := s.Create(Issue{Title: "Removed behind the store's back", State: "backlog"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := os.Chmod(s.issuePath(kept.ID), 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.issuePath(kept.ID), filePerm) })
	if err := os.Remove(s.issuePath(gone.ID)); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := s.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := s.Get(kept.ID); err != nil {
		t.Fatalf("a card whose file is momentarily unreadable was dropped from the index: %v", err)
	}
	if _, err := s.Get(gone.ID); err == nil {
		t.Fatal("a card whose file is gone is still served from the index")
	}
}

// A watch lost mid-life — fsnotify's channels closing under the loop —
// arms the same net as a watch refused at startup, instead of leaving the
// store blind until restart.
func TestWatcher_ArmsTheNetWhenTheWatchIsLost(t *testing.T) {
	setRescanInterval(t, 20*time.Millisecond)

	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.watcher == nil {
		t.Skipf("this host refused a watch (%v); a watch cannot be lost", s.watcherErr)
	}

	// The kernel side goes away under the loop's feet.
	if err := s.watcher.w.Close(); err != nil {
		t.Fatalf("close the fsnotify watcher: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		armed := s.watcher == nil && s.rescanner != nil && s.watcherErr != nil
		s.mu.Unlock()
		if armed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	armed := s.watcher == nil && s.rescanner != nil
	s.mu.Unlock()
	if !armed {
		t.Fatal("the lost watch did not arm the fallback net: the store is blind until restart")
	}

	now := time.Now().UTC().Truncate(time.Second)
	iss := Issue{ID: "native:after-the-loss", Title: "Seen by the net", State: "backlog", CreatedAt: now, UpdatedAt: now}
	data, err := json.MarshalIndent(&iss, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, issuesDir, encodeID(iss.ID)+".json"), data, filePerm); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline = time.Now().Add(fastPathBudget)
	for time.Now().Before(deadline) {
		if _, err := s.Get(iss.ID); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("after the watch was lost, an out-of-process create never became visible")
}
