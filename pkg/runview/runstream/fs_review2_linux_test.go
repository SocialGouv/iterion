//go:build linux

package runstream

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The store root's parent atomically exchanged with a regular file, or with a
// symlink looping on itself, while a stream is live: the retained roots no
// longer lead to the run, and the stream is refused as a replaced run instead
// of retrying a lock it can no longer reach.
func TestFileSourceStoreRootSwapIsRefused(t *testing.T) {
	prev := fileTerminalPollInterval
	fileTerminalPollInterval = 100 * time.Millisecond
	t.Cleanup(func() { fileTerminalPollInterval = prev })
	for _, kind := range []string{"file", "symlink loop"} {
		t.Run(kind, func(t *testing.T) {
			outer := t.TempDir()
			parent := filepath.Join(outer, "p")
			dir := filepath.Join(parent, "s")
			st, err := store.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			src := NewFileSource(st, dir, quietLogger())
			defer src.Close()
			seedHintRun(t, st, "x")
			sub, err := src.SubscribeEvents(t.Context(), "x", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			receiveSeq(t, sub, 0, 5*time.Second)
			other := filepath.Join(outer, "other")
			if kind == "file" {
				if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink("p", other); err != nil { // after the exchange: p -> p
				t.Fatal(err)
			}
			if err := unix.Renameat2(unix.AT_FDCWD, parent, unix.AT_FDCWD, other, unix.RENAME_EXCHANGE); err != nil {
				t.Skipf("renameat2 RENAME_EXCHANGE unavailable: %v", err)
			}
			// The exchanged tree goes back before the temporary directories are removed.
			t.Cleanup(func() {
				if err := unix.Renameat2(unix.AT_FDCWD, parent, unix.AT_FDCWD, other, unix.RENAME_EXCHANGE); err != nil {
					t.Errorf("exchange back: %v", err)
				}
			})
			readToEnd(t, sub, 3*time.Second)
			if !queuedError(sub.Errors(), store.ErrRunDataIdentityChanged) {
				t.Fatal("the stream ended without the identity refusal")
			}
		})
	}
}

// swapInUnreadableLocks atomically exchanges parent with a directory holding a
// store root of the same shape whose lock directory cannot be read, and
// undoes both at cleanup. A lock open through it fails with a permission
// error, which on its own is transient.
func swapInUnreadableLocks(t *testing.T, outer, parent string) {
	t.Helper()
	other := filepath.Join(outer, "other")
	locks := filepath.Join(other, "s", ".run-write-locks")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locks, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Renameat2(unix.AT_FDCWD, parent, unix.AT_FDCWD, other, unix.RENAME_EXCHANGE); err != nil {
		t.Skipf("renameat2 RENAME_EXCHANGE unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Renameat2(unix.AT_FDCWD, parent, unix.AT_FDCWD, other, unix.RENAME_EXCHANGE); err != nil {
			t.Errorf("exchange back: %v", err)
		}
		if err := os.Chmod(locks, 0o700); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	})
}

// The retained roots are checked before the lock is opened: a live stream
// whose store root was swapped for one whose locks cannot be read is refused
// as a replaced run, not retried on the permission error.
func TestFileSourceSwappedRootIsCheckedBeforeItsLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	prev := fileTerminalPollInterval
	fileTerminalPollInterval = 100 * time.Millisecond
	t.Cleanup(func() { fileTerminalPollInterval = prev })
	outer := t.TempDir()
	parent := filepath.Join(outer, "p")
	dir := filepath.Join(parent, "s")
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	src := NewFileSource(st, dir, quietLogger())
	defer src.Close()
	seedHintRun(t, st, "x")
	sub, err := src.SubscribeEvents(t.Context(), "x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	swapInUnreadableLocks(t, outer, parent)
	readToEnd(t, sub, 3*time.Second)
	if !queuedError(sub.Errors(), store.ErrRunDataIdentityChanged) {
		t.Fatal("the stream ended without the identity refusal")
	}
}
