package git

// SnapshotWorkingTree answers with two SHAs, and its callers build
// delegation worktrees and bases on them. Its helper hands those callers
// git's STDOUT alone, because git writes warnings, hints and advice on
// stderr whenever the environment gives it a reason to — a config file it
// cannot read, an advice.* nobody disabled — and read through the combined
// stream one such line IS the SHA the next command is asked to resolve.
// The shim here is the real git with one warning line in front of every
// answer: the production path, the real binary, one term changed.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

func TestSnapshotWorkingTreeGitTalkingOnStderrDoesNotReachTheShas(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	repo := gitRepo(t)

	shimDir := t.TempDir()
	shim := "#!/bin/sh\n" +
		"printf \"warning: unable to access '/nonexistent/git/attributes': Permission denied\\n\" >&2\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	commit, tree, err := SnapshotWorkingTree(repo, "test/talkative")
	if err != nil {
		t.Fatalf("a git warning on stderr killed the snapshot: %v", err)
	}
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if !sha.MatchString(commit) || !sha.MatchString(tree) {
		t.Fatalf("the snapshot carries git's prose in its identifiers: commit %q tree %q, want bare 40-hex SHAs", commit, tree)
	}
}
