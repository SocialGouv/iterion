package native

import (
	"os"
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

// A watch-loss callback may already have passed its stop-channel check when
// Close begins. Once Close has marked the store closed, that late callback
// must not arm a rescanner that outlives the store. (Landed first in #1020
// under a `closing` flag of its own; the store's `closed` is that flag.)
func TestWatchLostAfterCloseBeganArmsNoNet(t *testing.T) {
	setRescanInterval(t, 20*time.Millisecond)
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	watcher, _, werr := s.watchState()
	if watcher == nil {
		_ = s.Close()
		t.Skipf("this host refused a watch (%v); a watch cannot be lost", werr)
	}

	// Stage the exact Close boundary under the same mutex: a loss callback
	// that reaches watchLost after this point is late and must be ignored.
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.watchLost(watcher)

	_, rescanner, _ := s.watchState()
	if rescanner != nil {
		_ = rescanner.Close()
		_ = watcher.Close()
		t.Fatal("a watch lost after Close began armed a fallback rescanner")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
