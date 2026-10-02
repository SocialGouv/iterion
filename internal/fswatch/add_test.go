package fswatch

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Add watches like the raw call, and a refusal that is NOT a resource
// ceiling (a path that does not exist) passes through unchanged — the
// enrichment is for the inotify budget errors, never a disguise for an
// ordinary one.
func TestAddWatchesAndPassesOrdinaryErrorsThrough(t *testing.T) {
	w, err := NewWatcher()
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	defer w.Close()

	if err := Add(w, t.TempDir()); err != nil {
		t.Fatalf("Add on a real directory: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	addErr := Add(w, missing)
	if !errors.Is(addErr, syscall.ENOENT) {
		t.Fatalf("Add on a missing path: lost the original errno: %v", addErr)
	}
	rawErr := w.Add(missing)
	if addErr.Error() != rawErr.Error() {
		t.Fatalf("an ordinary refusal was rewritten:\n Add: %v\n raw: %v", addErr, rawErr)
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, syscall.ENOENT) {
		t.Fatalf("fixture: %s unexpectedly exists", missing)
	}
}
