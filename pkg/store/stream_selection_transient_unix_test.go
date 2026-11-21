//go:build unix

package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockDir removes every permission from dir until the returned restore runs
// (also at cleanup).
func lockDir(t *testing.T, dir string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore permissions of %s: %v", dir, err)
		}
	})
	t.Cleanup(restore)
	return restore
}

// A permission failure on the run's directory is not a replacement of that
// directory: opening the stream reports the failure as it is, transient,
// instead of the permanent identity refusal.
func TestFilesystemStreamOpenPermissionIsNotAnIdentityChange(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	restore := lockDir(t, filepath.Join(dir, "runs", "r"))
	n, _, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if n != nil {
		_ = n.Close()
	}
	restore()
	if errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("open = %v: a permission failure was reported as a replaced run", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("open = %v, want the permission failure", err)
	}
}

// The same holds for the revalidation of a retained selection: losing access
// to the runs directory is not losing the run.
func TestFilesystemStreamValidatePermissionIsNotAnIdentityChange(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	n, observed, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if err != nil || !observed {
		t.Fatalf("open = %v (observed %v)", err, observed)
	}
	defer n.Close()
	restore := lockDir(t, filepath.Join(dir, "runs"))
	err = n.Validate(t.Context())
	restore()
	if errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("validate = %v: a permission failure was reported as a replaced run", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("validate = %v, want the permission failure", err)
	}
	if err := n.Validate(t.Context()); err != nil {
		t.Fatalf("validate after access returned = %v", err)
	}
}

// storeUnderParent creates a store at <tmp>/p/s with a run "r" and returns it
// with its parent and its root.
func storeUnderParent(t *testing.T) (*FilesystemRunStore, string, string) {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "p")
	dir := filepath.Join(parent, "s")
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	return s, parent, dir
}

// chmodUntilRestored sets mode on p until the returned restore runs (also at
// cleanup).
func chmodUntilRestored(t *testing.T, p string, mode os.FileMode) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	restore := sync.OnceFunc(func() {
		if err := os.Chmod(p, 0o700); err != nil {
			t.Errorf("restore permissions of %s: %v", p, err)
		}
	})
	t.Cleanup(restore)
	return restore
}

// requireTransient fails unless err is a failure that is not a replacement.
func requireTransient(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, the permission change did not reach the site", what)
	}
	if errors.Is(err, ErrRunDataIdentityChanged) {
		t.Fatalf("%s = %v: a permission failure was reported as a replaced run", what, err)
	}
}

// Revalidation's first site, the store root itself: an ancestor that loses
// search permission is a transient failure, and access returning heals it.
func TestFilesystemStreamValidateRootPermissionIsNotAnIdentityChange(t *testing.T) {
	s, parent, _ := storeUnderParent(t)
	n, observed, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if err != nil || !observed {
		t.Fatalf("open = %v (observed %v)", err, observed)
	}
	defer n.Close()
	restore := chmodUntilRestored(t, parent, 0)
	err = n.Validate(t.Context())
	restore()
	requireTransient(t, "validate", err)
	if err := n.Validate(t.Context()); err != nil {
		t.Fatalf("validate after access returned = %v", err)
	}
}

// Opening the run's directory: readable but not searchable, the open succeeds
// and its own status read fails, transiently.
func TestFilesystemStreamOpenRunStatPermissionIsNotAnIdentityChange(t *testing.T) {
	s, _, dir := storeUnderParent(t)
	restore := chmodUntilRestored(t, filepath.Join(dir, "runs", "r"), 0o400)
	n, _, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if n != nil {
		_ = n.Close()
	}
	restore()
	requireTransient(t, "open", err)
}

// Capturing the store root: readable but not searchable, transiently.
func TestFilesystemStreamOpenRootStatPermissionIsNotAnIdentityChange(t *testing.T) {
	s, _, dir := storeUnderParent(t)
	restore := chmodUntilRestored(t, dir, 0o400)
	n, _, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if n != nil {
		_ = n.Close()
	}
	restore()
	requireTransient(t, "open", err)
}

// Waiting for a store root that does not exist yet: an ancestor that loses
// search permission during the wait is a transient failure.
func TestFilesystemStreamWaitPermissionIsNotAnIdentityChange(t *testing.T) {
	outer := t.TempDir()
	mid := filepath.Join(outer, "m")
	if err := os.Mkdir(mid, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &FilesystemRunStore{root: filepath.Join(mid, "a", "store")}
	done := make(chan error, 1)
	go func() {
		n, _, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{WaitForInitialDirectory: true})
		if n != nil {
			_ = n.Close()
		}
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	restore := chmodUntilRestored(t, outer, 0)
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		restore()
		t.Fatal("the waiting open did not return")
	}
	restore()
	requireTransient(t, "open while waiting", err)
}

// The lock open is the first descriptor a check allocates (validateRoots only
// stats): a permission or descriptor failure there is transient, not a
// replacement. P1a, review round 3.
func TestFilesystemStreamLockOpenPermissionIsTransient(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	locks := filepath.Join(dir, ".run-write-locks")
	if _, err := os.Stat(filepath.Join(locks, "r")); err != nil {
		t.Fatalf("lock file absent after CreateRun: %v", err)
	}
	n, observed, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if err != nil || !observed {
		t.Fatalf("open = %v (observed %v)", err, observed)
	}
	defer n.Close()
	restore := chmodUntilRestored(t, locks, 0)
	err = n.Validate(t.Context())
	restore()
	t.Logf("validate with an unsearchable lock directory = %v", err)
	requireTransient(t, "validate with an unsearchable lock directory", err)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("validate = %v, want the permission failure of the lock open", err)
	}
	if err := n.Validate(t.Context()); err != nil {
		t.Fatalf("validate after access returned = %v", err)
	}
}

// P1b, review round 3: descriptor exhaustion at the lock open is transient.
func TestFilesystemStreamLockOpenDescriptorExhaustionIsTransient(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(t.Context(), "r", "wf", nil); err != nil {
		t.Fatal(err)
	}
	n, observed, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if err != nil || !observed {
		t.Fatalf("open = %v (observed %v)", err, observed)
	}
	defer n.Close()
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	next := probe.Fd()
	probe.Close()
	lim := old
	lim.Cur = uint64(next)
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Fatal(err)
	}
	err = n.Validate(t.Context())
	if rerr := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old); rerr != nil {
		t.Fatalf("restore rlimit: %v", rerr)
	}
	t.Logf("validate under descriptor exhaustion = %v", err)
	if !errors.Is(err, syscall.EMFILE) {
		t.Fatalf("validate = %v, want EMFILE from the lock open", err)
	}
	requireTransient(t, "validate under descriptor exhaustion", err)
	if err := n.Validate(t.Context()); err != nil {
		t.Fatalf("validate after descriptors returned = %v", err)
	}
}

// P2, review round 3: opening while an ancestor of the store root is not
// searchable fails the root lookup (EvalSymlinks) — transient, not a
// replacement.
func TestFilesystemStreamOpenStoreRootAncestorPermissionIsTransient(t *testing.T) {
	s, parent, _ := storeUnderParent(t)
	restore := chmodUntilRestored(t, parent, 0)
	n, _, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if n != nil {
		_ = n.Close()
	}
	restore()
	t.Logf("open with an unsearchable store-root ancestor = %v", err)
	requireTransient(t, "open with an unsearchable store-root ancestor", err)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("open = %v, want the permission failure", err)
	}
	n, observed, err := s.OpenFilesystemStream(t.Context(), "r", FilesystemStreamOpenOptions{})
	if err != nil || !observed {
		t.Fatalf("open after access returned = %v (observed %v)", err, observed)
	}
	_ = n.Close()
}
