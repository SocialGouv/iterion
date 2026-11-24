package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
)

// bridgeLockFile is a stored file in a directory the test owns, with no
// control directory yet: each case decides who creates it.
func bridgeLockFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The agreement: the lock the server's editors take for a file and the one
// pkg/authoringlock computes for it are the SAME lock. Not "the same string"
// — the same flock: the bridge's take must not get it while the server
// holds it, and must get it once the server lets go. The writers of #2689
// (the catalogue regeneration, the CLI writers, the installers) are safe
// only if this holds.
func TestTheServerAndTheBridgeTakeTheSameAuthoringLock(t *testing.T) {
	path := bridgeLockFile(t, "file.txt")
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	served := filepath.Join(locks[0].control.path, locks[0].lockName)
	resolved, err := authoringlock.Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Path() != served {
		t.Fatalf("the bridge names %s, the server took %s", resolved.Path(), served)
	}
	// The name is pinned to the identity both sides owe, so a change to the
	// fold on either side is an edit of this line, not a silent divergence.
	if locks[0].lockName != authoringlock.LockFileName("file.txt") {
		t.Fatalf("the server's lock name %s is not the name of file.txt", locks[0].lockName)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	if held, err := authoringlock.Take(ctx, path); err == nil {
		_ = held.Release()
		closeAuthoringLocalLocks(locks)
		t.Fatal("the bridge took the lock while the server was holding it — the two takes are not the same lock")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		closeAuthoringLocalLocks(locks)
		t.Fatalf("the bridge's take ended with %v, want the wait its context bounds", err)
	}

	closeAuthoringLocalLocks(locks)
	after, err := authoringlock.Take(t.Context(), path)
	if err != nil {
		t.Fatalf("the bridge could not take the lock the server released: %v", err)
	}
	if err := after.Release(); err != nil {
		t.Fatal(err)
	}
}

// An alias and its target are one file: both sides fold the alias to the
// target's name, so a CLI writer formatting `alias.bot` waits on the editor
// saving `target.bot`.
func TestTheServerAndTheBridgeFoldAnAliasToTheSameLock(t *testing.T) {
	target := bridgeLockFile(t, "target.bot")
	alias := filepath.Join(filepath.Dir(target), "alias.bot")
	if err := os.Symlink("target.bot", alias); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{alias})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	if locks[0].lockName != authoringlock.LockFileName("target.bot") {
		t.Fatalf("the server locked the alias under %s, want the target's lock %s", locks[0].lockName, authoringlock.LockFileName("target.bot"))
	}
	resolved, err := authoringlock.Resolve(alias)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Path() != filepath.Join(locks[0].control.path, locks[0].lockName) {
		t.Fatalf("the bridge names %s for the alias, the server took %s", resolved.Path(), filepath.Join(locks[0].control.path, locks[0].lockName))
	}
}

// The reason the contract lives in one package: a writer that creates the
// control directory with the wrong modes makes the server refuse EVERY save
// of that file. A directory the bridge created serves the server's save.
func TestAControlDirectoryTheBridgeCreatedServesTheServerSSave(t *testing.T) {
	path := bridgeLockFile(t, "file.txt")
	if err := authoringlock.Hold(t.Context(), path, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(filepath.Dir(path), ".iterion", "authoring")
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("the bridge's take left no control directory: %v", err)
	}
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatalf("the server refused the control directory the bridge created: %v", err)
	}
	defer closeAuthoringLocalLocks(locks)
	if err := locks[0].writeEditorFile(storedCheck{name: "file.txt", decided: storedAt([]byte("original\n"))}, []byte("saved\n"), false, nil, nil); err != nil {
		t.Fatalf("the save failed in a control directory the bridge created: %v", err)
	}
	authoringBytes(t, path, "saved\n")
}

// And the inverse: a control directory the contract refuses is refused by
// BOTH takes, with the same cause. A bridge that accepted what the server
// refuses would hand its callers a lock the editors cannot share.
func TestAWrongModeControlDirectoryIsRefusedByBothTakes(t *testing.T) {
	path := bridgeLockFile(t, "file.txt")
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	control := locks[0].control.path
	closeAuthoringLocalLocks(locks)
	if err := os.Chmod(control, 0o777); err != nil {
		t.Fatal(err)
	}
	const want = "unsafe authoring ownership or permissions"
	if _, err := acquireAuthoringLocalLocks(t.Context(), []string{path}); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the server answered %v, want it to refuse the directory naming %q", err, want)
	}
	if held, err := authoringlock.Take(t.Context(), path); err == nil || !strings.Contains(err.Error(), want) {
		if err == nil {
			_ = held.Release()
		}
		t.Fatalf("the bridge answered %v, want it to refuse the directory naming %q", err, want)
	}
}
