package botinstall

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

// installedCopy is a workspace copy an install replaces: the files the new
// source also ships (with bytes of its own, so a replacement is visible) and
// one file the new source no longer ships, which the install must remove.
// The origin record is the installer's own, so --force is allowed on it.
func installedCopy(t *testing.T, dest, name string) string {
	t.Helper()
	target := filepath.Join(dest, name)
	writeBundle(t, target, name, 0)
	for rel, body := range map[string]string{
		"main.bot":           "# the copy that is there\nworkflow w:\n",
		"skills/kept.md":     "# the copy that is there\n",
		"skills/leftover.md": "# a file the new source does not ship\n",
	} {
		path := filepath.Join(target, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeOrigin(target, Origin{Source: "test", InstalledAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return target
}

// sourceBundle is the bundle an install copies FROM: the same names as the
// installed copy minus its leftover, plus an executable, so what a
// replacement writes and what it keeps are both observable.
func sourceBundle(t *testing.T, name string) string {
	t.Helper()
	repo := t.TempDir()
	writeBundle(t, repo, name, 0)
	if err := os.WriteFile(filepath.Join(repo, "main.bot"), []byte("# the source\nworkflow w:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "skills", "kept.md"), []byte("# the source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "skills", "tool.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo
}

func forceInstall(ctx context.Context, repo, dest, name string) error {
	_, err := Install(ctx, Options{
		Source: repo, Dest: dest, Name: name, Workdir: dest,
		Force: true, ReplaceUnrecorded: true, SkipCatalog: true,
	})
	return err
}

// TestAnInstallTakesTheLockOfEveryFileItWritesOrRemoves is the class, not an
// example: a --force install used to remove the whole directory and copy
// over it with no lock at all, so a save of ANY file of that bundle was
// lost. Every file the install writes, and every file it removes, is held
// to the same rule — the table is the install's whole write set.
func TestAnInstallTakesTheLockOfEveryFileItWritesOrRemoves(t *testing.T) {
	for _, rel := range []string{"main.bot", "manifest.yaml", "skills/kept.md", "skills/leftover.md"} {
		t.Run(rel, func(t *testing.T) {
			repo := sourceBundle(t, "mybot")
			dest := t.TempDir()
			target := installedCopy(t, dest, "mybot")
			saved := filepath.Join(target, filepath.FromSlash(rel))
			before, err := os.ReadFile(saved)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := authoringlock.Take(context.Background(), saved)
			if err != nil {
				t.Fatalf("stand in for an editor's save: %v", err)
			}
			defer func() {
				if err := lock.Release(); err != nil {
					t.Errorf("release: %v", err)
				}
			}()
			previous := installLockWait
			installLockWait = 300 * time.Millisecond
			t.Cleanup(func() { installLockWait = previous })

			// In a goroutine so a wait with no bound fails this test's own
			// assertion rather than the harness's timeout: an install that
			// sits behind a save forever is the other half of the bound.
			// The deferred release above ends the goroutine either way.
			done := make(chan error, 1)
			go func() { done <- forceInstall(context.Background(), repo, dest, "mybot") }()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the install did not come back within 10s though its wait is bounded at 300ms: it sits behind a save for as long as the save lasts")
			}
			if err == nil {
				t.Fatal("the install went through over a save in flight")
			}
			if !errors.Is(err, ErrInstallTargetLocked) {
				t.Fatalf("install = %v, want ErrInstallTargetLocked", err)
			}
			if !strings.Contains(err.Error(), saved) {
				t.Errorf("the refusal does not name the file being saved: %v", err)
			}
			after, readErr := os.ReadFile(saved)
			if readErr != nil {
				t.Fatalf("the file a save held is gone: %v", readErr)
			}
			if string(after) != string(before) {
				t.Errorf("the bytes of a save in flight were replaced:\n%s", after)
			}
		})
	}
}

// TestARefusedInstallLeavesTheBundleStandingRatherThanEmptied is the other
// half of dropping os.RemoveAll: the bundle is never missing. A refused
// install leaves the copy that is there — it does not leave a hole where a
// bot used to be.
func TestARefusedInstallLeavesTheBundleStandingRatherThanEmptied(t *testing.T) {
	repo := sourceBundle(t, "mybot")
	dest := t.TempDir()
	target := installedCopy(t, dest, "mybot")
	lock, err := authoringlock.Take(context.Background(), filepath.Join(target, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	previous := installLockWait
	installLockWait = 300 * time.Millisecond
	t.Cleanup(func() { installLockWait = previous })

	// Watchdogged like the table above: a wait with no bound would block
	// here with the save's lock held by this very test, and a test that
	// deadlocks takes its whole package down instead of reddening.
	done := make(chan error, 1)
	go func() { done <- forceInstall(context.Background(), repo, dest, "mybot") }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInstallTargetLocked) {
			t.Fatalf("install = %v, want ErrInstallTargetLocked", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the install did not come back within 10s though its wait is bounded at 300ms: it sits behind a save for as long as the save lasts")
	}
	for _, rel := range []string{"main.bot", "manifest.yaml", "skills/kept.md"} {
		path := filepath.Join(target, filepath.FromSlash(rel))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("a refused install left the bundle without %s: %v", rel, err)
		}
		if rel != "manifest.yaml" && !strings.Contains(string(body), "the copy that is there") {
			t.Errorf("%s is neither the copy that was there nor refused: %s", rel, body)
		}
	}
}

// TestAnInstallMakesTheWorkspaceCopyWhatTheSourceShips is the install's own
// contract, kept while the lock arrives: exactly the shipped set, modes
// included, nothing staged left behind — and the control directory of a
// file's lock SURVIVES the replacement, because the next save must find the
// same lock and not a new one beside a live hold.
func TestAnInstallMakesTheWorkspaceCopyWhatTheSourceShips(t *testing.T) {
	repo := sourceBundle(t, "mybot")
	dest := t.TempDir()
	target := installedCopy(t, dest, "mybot")
	// A save happened here once, so the lock's control directory is there.
	lock, err := authoringlock.Take(context.Background(), filepath.Join(target, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	control := lock.Identity().Control
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("no control directory to test with: %v", err)
	}

	if err := forceInstall(context.Background(), repo, dest, "mybot"); err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, rel := range []string{"main.bot", "skills/kept.md"} {
		body, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "the source") {
			t.Errorf("%s was not replaced by the source: %s", rel, body)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "skills", "leftover.md")); !os.IsNotExist(err) {
		t.Errorf("a file the source does not ship survived the install: %v", err)
	}
	info, err := os.Stat(filepath.Join(target, "skills", "tool.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("an executable the bundle ships came out as %v", info.Mode().Perm())
	}
	if _, err := os.Stat(control); err != nil {
		t.Errorf("the install removed the authoring lock's control directory: %v", err)
	}
	staged, _ := filepath.Glob(filepath.Join(target, ".*install-*"))
	more, _ := filepath.Glob(filepath.Join(target, "skills", ".*install-*"))
	if len(staged)+len(more) > 0 {
		t.Errorf("staged copies left behind: %v %v", staged, more)
	}
}

// TestAReplacedFileIsANewInodeNotATruncatedOne pins HOW a file is replaced:
// staged beside it and renamed, so a reader that takes no lock — a run
// mirroring a bundle's skills at its start — sees the file that was there or
// the one that replaced it, never half of either.
func TestAReplacedFileIsANewInodeNotATruncatedOne(t *testing.T) {
	repo := sourceBundle(t, "mybot")
	dest := t.TempDir()
	target := installedCopy(t, dest, "mybot")
	path := filepath.Join(target, "main.bot")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := forceInstall(context.Background(), repo, dest, "mybot"); err != nil {
		t.Fatalf("install: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the file was written in place: a reader could meet it half replaced")
	}
}

// TestTheRemovalRefusesAPathOutsideTheBundle holds the guard in front of
// every removal: the paths come from a walk of the bundle directory, and a
// path that is not under it means something moved under that walk. A
// removal is not a gesture to make on a guess.
func TestTheRemovalRefusesAPathOutsideTheBundle(t *testing.T) {
	target := t.TempDir()
	outside := t.TempDir()
	for _, path := range []string{
		filepath.Join(outside, "elsewhere.md"),
		filepath.Join(target, "..", "sibling.md"),
		target,
	} {
		err := ownedUnder(path, target)
		if path == target {
			if err != nil {
				t.Errorf("the bundle directory itself is owned: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("ownedUnder(%q) allowed a path outside %q", path, target)
		}
	}
	if err := removeUnderLock(context.Background(), filepath.Join(outside, "elsewhere.md"), target); err == nil {
		t.Error("a removal outside the bundle went through")
	}
}
