package runtime

// The package's git chokepoint (runGit) hands its callers git's STDOUT, and
// these two witnesses are the production parses that read a value out of it:
// the review gate's sequence number, read from a refname list, and the fast
// path that decides whether a worktree is clean, read from porcelain. Both
// triggers are git's own, measured on git 2.43: a broken ref under the
// listed prefix ("warning: ignoring broken ref <name>", exit 0, the sound
// refs still on stdout), and a config path git cannot read ("warning: unable
// to access '<xdg>/git/attributes': Permission denied", exit 0, stdout
// empty on a clean tree). The first is used as-is; the second is played by a
// shim in front of the real git, because an unreadable directory is no
// obstacle to a test running as root.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// initRepoForGitChokepoint builds a one-commit repository whose worktree is
// clean. The values come through gittest.Run, whose contract is the trimmed
// COMBINED output, so it is called before any talkative git goes up.
func initRepoForGitChokepoint(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gittest.Run(t, dir, "init", "-q", "-b", "main")
	gittest.Run(t, dir, "config", "user.email", "t@t.t")
	gittest.Run(t, dir, "config", "user.name", "t")
	gittest.Run(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, dir, "add", "--", "a.txt")
	gittest.Run(t, dir, "commit", "-qm", "base")
	return dir
}

// A resumed run continues the gate sequence read from git. One broken ref
// under the gate prefix — a ref whose object git cannot resolve, which a
// crashed write or a pruned object leaves behind — makes git warn on stderr
// and still list the sound refs on stdout. Read through the combined stream
// the warning's own last path segment IS a gate number, so the sequence
// jumps to it: gate/7 named in a warning becomes "the next gate is 8" in a
// repository whose only gate is 0, and every later gate of that run is
// numbered past the refs the studio enumerates.
func TestNextReviewGateSeq_CountsTheRefsGitListsNotTheOnesItWarnsAbout(t *testing.T) {
	dir := initRepoForGitChokepoint(t)
	gittest.Run(t, dir, "update-ref", store.ReviewGateRef("r1", 0), "HEAD")

	broken := filepath.Join(dir, ".git", filepath.FromSlash(store.ReviewGateRef("r1", 7)))
	if err := os.MkdirAll(filepath.Dir(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("not-a-sha\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The trigger, asserted so this witness cannot pass by having nothing
	// to ignore: git talks about the broken ref, and keeps exit 0.
	prefix := strings.TrimSuffix(store.ReviewGateRef("r1", 0), "0")
	combined, err := runGitCombined(dir, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		t.Fatalf("for-each-ref over %s failed: %v\noutput: %s", prefix, err, combined)
	}
	if !strings.Contains(combined, "ignoring broken ref") {
		t.Fatalf("this git does not warn about a broken ref, so the witness would prove nothing; combined output: %q", combined)
	}

	if got := nextReviewGateSeq(dir, "r1"); got != 1 {
		t.Fatalf("nextReviewGateSeq = %d, want 1 — the gate number came out of git's warning line, not out of the refs it listed", got)
	}
}

// The engine's fast path at every node boundary: a clean worktree needs no
// index walk and no snapshot. Read through the combined stream, one line git
// writes about the environment IS the porcelain output, so the same probe
// reports a clean worktree as dirty — and the parse one hop further on
// (porcelainRecords) reads that line's first two bytes as a status field and
// the rest as a PATH, which is what `git add --` is then asked to stage.
func TestWorktreeStateClean_AGitWarningOnStderrIsNotADirtyWorktree(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	dir := initRepoForGitChokepoint(t)

	shimDir := t.TempDir()
	shim := "#!/bin/sh\n" +
		"printf \"warning: unable to access '/nonexistent/git/attributes': Permission denied\\n\" >&2\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The trigger, asserted: git talks, and still answers with an empty
	// porcelain because the worktree is clean.
	combined, err := runGitCombined(dir, "status", "--porcelain")
	if err != nil {
		t.Fatalf("status through the shim failed: %v\noutput: %s", err, combined)
	}
	if !strings.Contains(combined, "warning: unable to access") {
		t.Fatalf("the shim did not reach git's caller, so the witness would prove nothing; combined output: %q", combined)
	}

	clean, untracked, err := worktreeStateClean(dir)
	if err != nil {
		t.Fatalf("worktreeStateClean: %v", err)
	}
	if !clean || untracked {
		t.Fatalf("worktreeStateClean = (clean=%v, untracked=%v), want (true, false) — the warning git wrote on stderr was read as the state of the worktree", clean, untracked)
	}
}
