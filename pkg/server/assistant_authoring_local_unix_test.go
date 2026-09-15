//go:build linux || darwin

package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestAuthoringLocalNativeMoveUsesPinnedParents(t *testing.T) {
	tx, path := authoringLocalFixture(t, "replace")
	parent := filepath.Dir(path)
	moved := parent + "-moved"
	foreign := t.TempDir()
	foreignFile := filepath.Join(foreign, filepath.Base(path))
	if err := os.WriteFile(foreignFile, []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, parent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(parent); _ = os.Rename(moved, parent) })
	// Model namespace substitution AFTER validation and before the syscall.
	// The native primitive must stay on the opened directories, not re-resolve
	// the now-foreign pathname. The caller subsequently detects the change.
	if err := renameAuthoringAt(tx.lock.parent.file, filepath.Base(path), tx.lock.control.file, tx.name("before")); err != nil {
		t.Fatal(err)
	}
	authoringBytes(t, foreignFile, "foreign")
	authoringBytes(t, filepath.Join(moved, ".iterion", "authoring", tx.name("before")), "original\n")
	if err := tx.lock.verify(); err == nil {
		t.Fatal("substituted namespace was not detected")
	}
}

func TestAuthoringLocalSameFilesystemPreflightAndInPlaceSymlinkStorage(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("no second writable filesystem")
	}
	controlRoot, err := os.MkdirTemp("/dev/shm", "iterion-authoring-test-")
	if err != nil {
		t.Skipf("second filesystem unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(controlRoot) })
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(controlRoot, filepath.Join(root, ".iterion")); err != nil {
		t.Fatal(err)
	}
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	if err := requireAuthoringSameFilesystem(locks[0].parent.file, locks[0].control.file); !errors.Is(err, unix.EXDEV) {
		t.Skipf("fixtures are not on separate devices: %v", err)
	}
	tx, err := prepareAuthoringLocal(locks[0], authoringPreviewFile{Operation: "replace", Before: "original", After: "candidate"}, nil, nil)
	if tx != nil {
		tx.close()
	}
	if !errors.Is(err, unix.EXDEV) {
		t.Fatalf("replacement did not preflight cross-device storage: %v", err)
	}
	authoringBytes(t, path, "original")
	if err := locks[0].writeEditorFile([]byte("ordinary save"), false, nil); err != nil {
		t.Fatalf("in-place save unnecessarily required same-device recovery: %v", err)
	}
	authoringBytes(t, path, "ordinary save")
	entries, err := os.ReadDir(locks[0].control.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("replacement stored transaction data before EXDEV refusal: %+v", entries)
	}
}
