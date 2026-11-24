package authoringlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// authoringFile is a stored file to lock, in a directory this test owns.
func authoringFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func take(t *testing.T, path string) *Lock {
	t.Helper()
	l, err := Take(t.Context(), path)
	if err != nil {
		t.Fatalf("take %s: %v", path, err)
	}
	t.Cleanup(func() { _ = l.Release() })
	return l
}

// The name is the lock's identity: every writer of one file must compute it,
// so it is pinned to a value here, not to the other side's answer.
func TestTheLockNameIsTheHashOfTheFoldedFileName(t *testing.T) {
	const mainBot = "2f3368b180f3665cf45ff94f6595979fbf096ea47d9683311c9f91f033e56b44.lock"
	if got := LockFileName("main.bot"); got != mainBot {
		t.Fatalf("LockFileName(main.bot) = %s, want %s", got, mainBot)
	}
	// Folding: a different case, a decomposed spelling and a Windows-trimmed
	// name are the SAME file on the filesystems that alias them, so they are
	// the same lock. Over-folding only costs contention; under-folding loses
	// the exclusion the lock exists for.
	for _, spelling := range []string{"MAIN.BOT", "Main.Bot"} {
		if got := LockFileName(spelling); got != mainBot {
			t.Errorf("LockFileName(%s) = %s, want the lock of main.bot %s", spelling, got, mainBot)
		}
	}
	if LockFileName("café.bot") != LockFileName("café.bot") {
		t.Error("a decomposed name takes another lock than its composed spelling")
	}
	if LockFileName("other.bot") == mainBot {
		t.Error("two different names fold to one lock")
	}
}

func TestTakeCreatesTheControlDirectoryTheContractDescribes(t *testing.T) {
	path := authoringFile(t, "main.bot")
	l := take(t, path)

	control := filepath.Join(filepath.Dir(path), RecoveryDirName, ControlDirName)
	// The identity the take holds is the one Resolve answers for the path:
	// a writer that only wants to NAME the lock reads the same answer.
	resolved, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != l.Identity() {
		t.Fatalf("Resolve = %+v, the take holds %+v", resolved, l.Identity())
	}
	if l.Identity().Name != LockFileName("main.bot") {
		t.Fatalf("lock name %s is not the name of main.bot", l.Identity().Name)
	}
	if _, err := os.Stat(l.Identity().Path()); err != nil {
		t.Fatalf("the lock file the take holds is not there: %v", err)
	}
	// Equality of the paths is not enough: compare them after resolving the
	// control directory's own symlinks, which Resolve follows.
	if resolvedControl, err := filepath.EvalSymlinks(control); err != nil || resolvedControl != l.Identity().Control {
		t.Fatalf("control directory %s is not the one the take used %s: %v", resolvedControl, l.Identity().Control, err)
	}

	for _, dir := range []string{filepath.Join(filepath.Dir(path), RecoveryDirName), control} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v, want a directory nobody else may enter", dir, info.Mode().Perm())
		}
	}
	ignore := filepath.Join(control, ExclusionName)
	body, err := os.ReadFile(ignore)
	if err != nil || string(body) != ExclusionBody {
		t.Fatalf("%s = %q, want %q: %v", ignore, body, ExclusionBody, err)
	}
	info, err := os.Stat(ignore)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s is %v, want 0600", ignore, info.Mode().Perm())
	}
}

// A file that is not there yet locks under its own name: the writer creating
// it holds the lock the editors of that path hold.
func TestAFileNotThereYetTakesTheLockOfItsOwnName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.bot")
	l := take(t, path)
	if l.Identity().Name != LockFileName("new.bot") {
		t.Fatalf("lock name %s is not the name of new.bot", l.Identity().Name)
	}
}

// The lock is per FILE. Two writers of two files must not wait on each
// other: the catalogue regeneration runs at EVERY run start, and a lock that
// serialized would serialize the runs of different bots.
func TestTwoDifferentFilesDoNotWaitOnEachOther(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.bot")
	second := filepath.Join(dir, "second.bot")
	for _, p := range []string{first, second} {
		if err := os.WriteFile(p, []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	held := take(t, first)
	// Generous on purpose: the assertion is that the second take does not
	// WAIT on the first, and a take that waited would hold until the end of
	// the test, whatever the budget. A loaded host must not redden this.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	other, err := Take(ctx, second)
	if err != nil {
		t.Fatalf("the lock of %s waited on the lock of %s: %v", second, first, err)
	}
	defer func() { _ = other.Release() }()
	if other.Identity() == held.Identity() {
		t.Fatal("two files took one lock")
	}
}

func TestTheSameFileWaitsAndTheCallerSContextBoundsTheWait(t *testing.T) {
	path := authoringFile(t, "main.bot")
	held, err := Take(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	if second, err := Take(ctx, path); err == nil {
		_ = second.Release()
		_ = held.Release()
		t.Fatal("a second take of the same file got the lock while it was held")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the wait ended with %v, want the caller's deadline", err)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("the take waited %v, less than the deadline it was given", waited)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	// Released is available: the lock file survives, the hold does not.
	after, err := Take(t.Context(), path)
	if err != nil {
		t.Fatalf("the lock was not free after its holder released it: %v", err)
	}
	if err := after.Release(); err != nil {
		t.Fatal(err)
	}
}

// An alias and its target are one file, so they are one lock — otherwise two
// writers of one file both believe they hold it.
func TestAnAliasAndItsTargetTakeOneLock(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.bot")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.bot")
	if err := os.Symlink("target.bot", alias); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
	held := take(t, target)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	if l, err := Take(ctx, alias); err == nil {
		_ = l.Release()
		t.Fatalf("the alias took %+v, another lock than its target %+v", l.Identity(), held.Identity())
	}
	if resolved, err := Resolve(alias); err != nil || resolved != held.Identity() {
		t.Fatalf("Resolve(alias) = %+v (%v), want the target's lock %+v", resolved, err, held.Identity())
	}
}

// A target in ANOTHER directory is not this caller's file: the lock of a
// directory the caller did not open answers for writers it cannot see.
func TestAnAliasPointingOutOfTheDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "target.bot")
	if err := os.WriteFile(elsewhere, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.bot")
	if err := os.Symlink(elsewhere, alias); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
	if _, err := Resolve(alias); !errors.Is(err, ErrAliasParentChanged) {
		t.Fatalf("Resolve = %v, want the alias refused", err)
	}
	if l, err := Take(t.Context(), alias); !errors.Is(err, ErrAliasParentChanged) {
		if err == nil {
			_ = l.Release()
		}
		t.Fatalf("Take = %v, want the alias refused", err)
	}
}

func TestAControlDirectoryTheContractRefuses(t *testing.T) {
	for _, c := range []struct {
		name   string
		break_ func(t *testing.T, control string)
		want   string
	}{
		{"a directory anyone may enter", func(t *testing.T, control string) {
			if err := os.Chmod(control, 0o777); err != nil {
				t.Fatal(err)
			}
		}, "unsafe authoring ownership or permissions"},
		{"an entry the directory may not hold", func(t *testing.T, control string) {
			if err := os.WriteFile(filepath.Join(control, "README"), []byte("mine\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "unexpected authoring control entry"},
		{"an ignore rule that says something else", func(t *testing.T, control string) {
			if err := os.WriteFile(filepath.Join(control, ExclusionName), []byte("!*\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "unsafe authoring ignore file"},
		{"an ignore rule anyone may read", func(t *testing.T, control string) {
			if err := os.Chmod(filepath.Join(control, ExclusionName), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "unsafe authoring ownership or permissions"},
		{"a lock anyone may write", func(t *testing.T, control string) {
			if err := os.Chmod(filepath.Join(control, LockFileName("main.bot")), 0o666); err != nil {
				t.Fatal(err)
			}
		}, "unsafe authoring ownership or permissions"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := authoringFile(t, "main.bot")
			first, err := Take(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			control := first.Identity().Control
			if err := first.Release(); err != nil {
				t.Fatal(err)
			}
			c.break_(t, control)
			l, err := Take(t.Context(), path)
			if err == nil {
				_ = l.Release()
				t.Fatalf("the take accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the take answered %q, want it to name %q", err, c.want)
			}
		})
	}
}

func TestHoldReleasesTheLockItTook(t *testing.T) {
	path := authoringFile(t, "main.bot")
	ran := false
	if err := Hold(t.Context(), path, func() error { ran = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("Hold did not run its writer")
	}
	// The writer's own failure is the caller's, and the lock is given back
	// either way — a second Hold proves it.
	sentinel := errors.New("the writer failed")
	if err := Hold(t.Context(), path, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Hold = %v, want the writer's own error", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("Hold swallowed a panic")
			}
		}()
		_ = Hold(t.Context(), path, func() error { panic("the writer panicked") })
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	l, err := Take(ctx, path)
	if err != nil {
		t.Fatalf("Hold left the lock held: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestTheControlEntryRulesAnswerForTheNamesTheDirectoryHolds(t *testing.T) {
	for _, c := range []struct {
		name                        string
		control, displaced, private bool
	}{
		{ExclusionName, true, false, true},
		{LockFileName("main.bot"), true, false, true},
		{"8cf1f78a-3ba4-4d58-9d9b-2f6d53b2f9f8.record", true, false, true},
		{"8cf1f78a-3ba4-4d58-9d9b-2f6d53b2f9f8.candidate", true, false, false},
		{"8cf1f78a-3ba4-4d58-9d9b-2f6d53b2f9f8.before", true, true, false},
		{"8cf1f78a-3ba4-4d58-9d9b-2f6d53b2f9f8.undo", true, true, false},
		{"README", false, false, false},
		{"main.bot", false, false, false},
		{"2f3368b1.lock", false, false, true},
	} {
		if got := IsControlEntry(c.name); got != c.control {
			t.Errorf("IsControlEntry(%s) = %v, want %v", c.name, got, c.control)
		}
		if got := IsDisplacedEntry(c.name); got != c.displaced {
			t.Errorf("IsDisplacedEntry(%s) = %v, want %v", c.name, got, c.displaced)
		}
		if got := IsPrivateEntry(c.name); got != c.private {
			t.Errorf("IsPrivateEntry(%s) = %v, want %v", c.name, got, c.private)
		}
	}
}
