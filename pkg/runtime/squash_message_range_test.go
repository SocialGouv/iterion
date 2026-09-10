package runtime

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// A run's squash message must describe the RUN, and the range that bounds it
// is the work branch — never the whole repository.
//
// Observed in production (run 01a07013, 2026-09-05): the merge landed the
// right CONTENT (six files, one parent = the previous target head) under the
// wrong MESSAGE — subject "Initial commit", body 204 lines of the target
// repository's own past ending in "… and 5503 more commits". A repo-targeted
// merge materialises its own clone, where the run's recorded base is not
// necessarily resolvable; with no base `git log <head>` walks to the root, and
// --reverse makes the OLDEST commit the title.
func TestBuildSquashMessageForMerge_NeverEnumeratesFromTheRoot(t *testing.T) {
	repo, _ := initBareishRepo(t)
	// A target with real history: the shape the bug needs to be visible.
	addCommit(t, repo, "history1.md", "one\n", "Add new directory")
	addCommit(t, repo, "history2.md", "two\n", "chore: second commit on main")

	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	addCommit(t, wt, "a.go", "package main\n// a\n", "feat(lot-1): the run's first unit")
	finalSHA := addCommit(t, wt, "b.go", "package main\n// b\n", "feat(lot-1): the run's second unit")

	// base = "" is exactly what the merge clone hands the builder.
	got := BuildSquashMessageForMerge(repo, "", "main", finalSHA, "swift-cedar-a3f2")

	if strings.HasPrefix(got, "init\n") || strings.Contains(got, "Initial commit") {
		t.Fatalf("the subject is the repository's own first commit, not the run's delivery:\n%s", got)
	}
	if !strings.HasPrefix(got, "feat(lot-1): the run's first unit\n") {
		t.Errorf("subject = the run's first commit; got:\n%s", got)
	}
	for _, absent := range []string{"Add new directory", "chore: second commit on main", " init\n"} {
		if strings.Contains(got, absent) {
			t.Errorf("body enumerates the target's own history (%q):\n%s", absent, got)
		}
	}
	for _, want := range []string{"the run's first unit", "the run's second unit"} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing the run's commit %q:\n%s", want, got)
		}
	}
}

// A base that IS resolvable stays authoritative — it is the exact commit the
// run started from, which merge-base only approximates once the target moved.
func TestBuildSquashMessageForMerge_KeepsAResolvableBase(t *testing.T) {
	repo, _ := initBareishRepo(t)
	base := addCommit(t, repo, "history.md", "one\n", "chore: main moved on")

	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	addCommit(t, wt, "a.go", "package main\n", "feat: only unit")
	finalSHA := gittest.Run(t, wt, "rev-parse", "HEAD")

	got := BuildSquashMessageForMerge(repo, base, "main", finalSHA, "run")
	if !strings.HasPrefix(got, "feat: only unit") {
		t.Errorf("got:\n%s", got)
	}
	if strings.Contains(got, "chore: main moved on") {
		t.Errorf("body reaches past the run's base:\n%s", got)
	}
}

// A base recorded on the run but ABSENT from this repository (the merge clone
// fetched only the branches it needs) must not degrade to the root walk.
func TestBuildSquashMessageForMerge_UnreachableBaseFallsToMergeBase(t *testing.T) {
	repo, _ := initBareishRepo(t)
	addCommit(t, repo, "history.md", "one\n", "Add new directory")

	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	finalSHA := addCommit(t, wt, "a.go", "package main\n", "feat: the run's unit")

	got := BuildSquashMessageForMerge(repo, "0123456789abcdef0123456789abcdef01234567", "main", finalSHA, "run")
	if !strings.HasPrefix(got, "feat: the run's unit") {
		t.Errorf("got:\n%s", got)
	}
	if strings.Contains(got, "Add new directory") || strings.Contains(got, "init") {
		t.Errorf("body reaches into the target's history:\n%s", got)
	}
}

// A target that does not resolve at all — a greenfield run that `git init`s an
// empty directory and commits from slice 0 — genuinely owns the whole history,
// so the full walk is the right answer there and must be preserved.
func TestBuildSquashMessageForMerge_GreenfieldKeepsTheWholeHistory(t *testing.T) {
	dir := t.TempDir()
	gittest.Run(t, dir, "init", "-b", "main")
	gittest.Run(t, dir, "config", "user.email", "test@example.com")
	gittest.Run(t, dir, "config", "user.name", "Test")
	gittest.Run(t, dir, "config", "commit.gpgsign", "false")
	addCommit(t, dir, "a.go", "package main\n", "feat: scaffold the app")
	finalSHA := addCommit(t, dir, "b.go", "package main\n// b\n", "feat: first endpoint")

	got := BuildSquashMessageForMerge(dir, "", "does-not-exist", finalSHA, "greenfield")
	if !strings.HasPrefix(got, "feat: scaffold the app\n") {
		t.Errorf("a greenfield run owns its whole history:\n%s", got)
	}
	if !strings.Contains(got, "feat: first endpoint") {
		t.Errorf("body missing the second commit:\n%s", got)
	}
}
