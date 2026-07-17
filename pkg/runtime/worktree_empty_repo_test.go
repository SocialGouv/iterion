package runtime

import (
	"os/exec"
	"testing"
)

// TestWorkspaceHasCommits distinguishes an unborn-HEAD repo (freshly
// created forge repo after clone) from one with history — the empty
// case must degrade `worktree: auto` to in-place instead of failing
// `git worktree add … HEAD`.
func TestWorkspaceHasCommits(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if workspaceHasCommits(dir) {
		t.Fatal("empty repo must report no commits")
	}
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "seed")
	if !workspaceHasCommits(dir) {
		t.Fatal("repo with a commit must report commits")
	}
}
