package botregistry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/authoringlock"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// editorSave stands in for an authoring editor's save of path: it holds the
// same authoring lock the server's editors take, for as long as the test
// keeps it. The returned func releases it (and is safe to call twice).
func editorSave(t *testing.T, path string) (*authoringlock.Lock, func()) {
	t.Helper()
	lock, err := authoringlock.Take(context.Background(), path)
	if err != nil {
		t.Fatalf("stand in for an editor's save of %s: %v", path, err)
	}
	released := false
	return lock, func() {
		if released {
			return
		}
		released = true
		if err := lock.Release(); err != nil {
			t.Errorf("release the editor's lock: %v", err)
		}
	}
}

// TestARegenerationLeavesTheBytesOfASaveInFlightAloneAndNamesWhy is the
// refusal the ticket asks for: the catalogue regeneration used to replace a
// file whose save was decided elsewhere. Now the save holds the lock, so
// the regeneration leaves the file whole and says what it left and why.
func TestARegenerationLeavesTheBytesOfASaveInFlightAloneAndNamesWhy(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	edited := generatedIn(ws, "whats-next")
	theirs := "---\nname: iterion-bot-catalog\n---\nAN EDITOR DECIDED ON THESE BYTES\n"
	if err := os.WriteFile(edited, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}
	_, release := editorSave(t, edited)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err := RegenerateCatalogForBundle(ctx, ws, filepath.Join(ws, "bots", "whats-next"))
	if err == nil {
		t.Fatal("a regeneration over a save in flight succeeded; want the owner's refusal")
	}
	if !strings.Contains(err.Error(), edited) {
		t.Errorf("the refusal does not name the file it left: %v", err)
	}
	if !strings.Contains(err.Error(), "authoring lock was not taken") {
		t.Errorf("the refusal does not name the lock as its cause: %v", err)
	}
	body, rerr := os.ReadFile(edited)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) != theirs {
		t.Errorf("the editor's bytes were replaced:\n%s", body)
	}
}

// TestARegenerationWaitsForASaveToFinishAndThenWrites holds the other half:
// the lock is a WAIT, not a loss. A save that ends inside the budget the
// caller allowed is followed by the refresh, not by a stale catalogue.
func TestARegenerationWaitsForASaveToFinishAndThenWrites(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	owner := filepath.Join(ws, "bots", "whats-next")
	dest := generatedIn(ws, "whats-next")
	_, release := editorSave(t, dest)
	var once sync.Once
	releaseOnce := func() { once.Do(release) }
	defer releaseOnce()
	go func() {
		time.Sleep(100 * time.Millisecond)
		releaseOnce()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	written, err := RegenerateCatalogForBundle(ctx, ws, owner)
	if err != nil {
		t.Fatalf("a regeneration behind a save that finished = %v, want the refresh", err)
	}
	if written != dest {
		t.Fatalf("written = %q, want %q", written, dest)
	}
	body, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) == catalogSentinel || !strings.Contains(string(body), "whats-next") {
		t.Errorf("the catalogue was not refreshed after the save ended:\n%s", body)
	}
}

// TestTwoOwnersDoNotWaitOnEachOther is the concurrency witness ARB-95 asks
// for: the lock is per FILE, and a run regenerates the catalogue its own
// bundle owns, so a save of one bot's catalogue cannot hold up the start of
// a run of another bot. It would redden against a lock of coarser grain (a
// workspace lock, a bundle lock) even though every byte-wise assertion in
// this file would stay green.
func TestTwoOwnersDoNotWaitOnEachOther(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	_, release := editorSave(t, generatedIn(ws, "whats-next"))
	defer release()

	budget := 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	written, err := RegenerateCatalogForBundle(ctx, ws, filepath.Join(ws, "bots", "issue-triage"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the other owner's refresh = %v, want it refreshed", err)
	}
	if written != generatedIn(ws, "issue-triage") {
		t.Fatalf("written = %q, want the other owner's catalogue", written)
	}
	// A refresh that WAITED on the held lock would sit here for the whole
	// budget: the assertion is about not waiting, so it is generous about
	// how slow a host may be and still exact about the serialisation.
	if elapsed > budget/4 {
		t.Errorf("the other owner's refresh took %s of a %s budget: it waited on a lock held for another file", elapsed, budget)
	}
	// And the held file is untouched: a refresh of one owner never writes
	// another owner's copy.
	body, rerr := os.ReadFile(generatedIn(ws, "whats-next"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) != catalogSentinel {
		t.Errorf("the other owner's catalogue was rewritten:\n%s", body)
	}
}

// TestTheLockIsHeldFromTheReadToTheRename pins the WINDOW, which no byte of
// the result can show: a take released before the read, or taken after it,
// would leave exactly the same file. The hook runs inside the lock, and a
// competing take from there must not win.
func TestTheLockIsHeldFromTheReadToTheRename(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	owner := filepath.Join(ws, "bots", "whats-next")
	dest := generatedIn(ws, "whats-next")

	steps := []string{}
	catalogWriteHook = func(step string) {
		steps = append(steps, step)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		lock, err := authoringlock.Take(ctx, dest)
		if err == nil {
			_ = lock.Release()
			t.Errorf("at step %q another writer took the catalogue's authoring lock: the refresh does not hold it there", step)
		}
	}
	t.Cleanup(func() { catalogWriteHook = nil })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := RegenerateCatalogForBundle(ctx, ws, owner); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if len(steps) != 2 || steps[0] != "locked" || steps[1] != "written" {
		t.Fatalf("steps inside the lock = %v, want [locked written]", steps)
	}
	// The lock the refresh took is the one the bridge names for this file —
	// the same one pkg/server's editors take (proven in that package).
	id, err := authoringlock.Resolve(dest)
	if err != nil {
		t.Fatalf("resolve the catalogue's lock: %v", err)
	}
	if _, err := os.Stat(id.Path()); err != nil {
		t.Errorf("the refresh did not take the lock the bridge names for %s: %v", dest, err)
	}
	if want := filepath.Join(filepath.Dir(dest), authoringlock.RecoveryDirName, authoringlock.ControlDirName); id.Control != want {
		t.Errorf("control directory = %q, want %q (beside the file it governs)", id.Control, want)
	}
}

// TestTheControlDirectoryIsNoMemberOfTheBundle is the pin the lock itself
// needs: the control directory lands INSIDE the bundle (beside the file it
// governs), and every walker of a bundle's content already skips
// .iterion — so a refresh does not move the bundle's content hash and
// cannot make a dependency drift from the hash its bots.lock pins.
func TestTheControlDirectoryIsNoMemberOfTheBundle(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	owner := filepath.Join(ws, "bots", "whats-next")
	// The hash is taken on a bundle whose generated catalogue is already
	// what this build renders, so the only change the refresh can make is
	// the control directory itself.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := RegenerateCatalogForBundle(ctx, ws, owner); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	control := filepath.Join(owner, bundle.DirSkills, authoringlock.RecoveryDirName)
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("the refresh left no control directory to test (%s): %v", control, err)
	}
	before, err := bundle.ContentHashDir(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(control); err != nil {
		t.Fatal(err)
	}
	without, err := bundle.ContentHashDir(owner)
	if err != nil {
		t.Fatal(err)
	}
	if before != without {
		t.Errorf("the authoring control directory is part of the bundle's content hash (%s with, %s without): every refresh would drift a dependency from its bots.lock pin", before, without)
	}
}

// TestTheTemplateIsReadInsideTheLock shows the window by its EFFECT rather
// than by a probe: the template disappears at the first step inside the
// lock, and the refresh fails on reading it. A refresh that had read the
// template BEFORE taking the lock would sail past — and would be the
// original defect, since the bytes it writes were then decided outside the
// window the lock exists to hold.
func TestTheTemplateIsReadInsideTheLock(t *testing.T) {
	ws := twoOwnerWorkspace(t, nil)
	owner := filepath.Join(ws, "bots", "whats-next")
	staticPath := filepath.Join(owner, catalogStaticName)
	catalogWriteHook = func(step string) {
		if step != "locked" {
			return
		}
		if err := os.Remove(staticPath); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove the template from inside the lock: %v", err)
		}
	}
	t.Cleanup(func() { catalogWriteHook = nil })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := RegenerateCatalogForBundle(ctx, ws, owner)
	if err == nil {
		t.Fatal("a template removed inside the lock did not stop the refresh: the bytes it wrote were decided before the lock was taken")
	}
	if !strings.Contains(err.Error(), "read catalog template") {
		t.Errorf("failure = %v, want the template read to be the cause", err)
	}
	body, rerr := os.ReadFile(generatedIn(ws, "whats-next"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(body) != catalogSentinel {
		t.Errorf("the committed catalogue was rewritten although the refresh failed:\n%s", body)
	}
}
