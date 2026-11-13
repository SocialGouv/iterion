package store

import (
	"os"
	"path/filepath"
	"testing"
)

// A directory the process just wrote into syncs without error on every
// platform. On Windows a read-only handle made this ERROR_ACCESS_DENIED, so
// every atomic write failed after its rename.
func TestSyncDir_SyncsADirectoryJustWrittenInto(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncDir(dir); err != nil {
		t.Fatalf("SyncDir(%s) = %v, want nil", dir, err)
	}
}

func TestSyncDir_RefusesAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := SyncDir(missing); err == nil {
		t.Fatalf("SyncDir(%s) = nil, want an error", missing)
	}
}
