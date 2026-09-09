package native

import (
	"os"
	"testing"
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
