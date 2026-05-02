package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed in %s: %v\noutput: %s", name, args, dir, err, string(out))
	}
}

func mustOutput(t *testing.T, dir string, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v failed in %s: %v", name, args, dir, err)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// initBareishRepo creates a fresh repo with one commit, suitable as
// the "main worktree" for finalize tests. Returns the absolute repo
// path and the SHA of the initial commit.
func initBareishRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, dir, "git", "init", "-b", "main")
	mustRun(t, dir, "git", "config", "user.email", "test@example.com")
	mustRun(t, dir, "git", "config", "user.name", "Test")
	mustRun(t, dir, "git", "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "README.md"), "init\n")
	mustRun(t, dir, "git", "add", "README.md")
	mustRun(t, dir, "git", "commit", "-m", "init")
	sha := strings.TrimSpace(string(mustOutput(t, dir, "git", "rev-parse", "HEAD")))
	return dir, sha
}

// addCommit makes a single commit in the worktree at wtPath. Returns the
// new SHA. Used to simulate "the agent committed something during the run".
func addCommit(t *testing.T, wtPath, file, content, msg string) string {
	t.Helper()
	writeFile(t, filepath.Join(wtPath, file), content)
	mustRun(t, wtPath, "git", "add", file)
	mustRun(t, wtPath, "git", "commit", "-m", msg)
	return strings.TrimSpace(string(mustOutput(t, wtPath, "git", "rev-parse", "HEAD")))
}

// TestFinalizeWorktree_NoCommits — a run that produced no commits in
// the worktree should be a no-op: no branch created, no merge attempted.
func TestFinalizeWorktree_NoCommits(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2"}, nil)

	if res.FinalCommit != "" || res.FinalBranch != "" || res.MergedInto != "" {
		t.Fatalf("expected zero finalization for unchanged HEAD, got %+v", res)
	}
	// And no branch was created.
	out, _ := exec.Command("git", "-C", repo, "branch", "--list", "iterion/run/*").Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("no branch should be created when no commits, got: %q", string(out))
	}
}

// TestFinalizeWorktree_HappyPath_FFCurrent — commits in the worktree,
// main is clean, FF is possible → branch created + main fast-forwarded.
func TestFinalizeWorktree_HappyPath_FFCurrent(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	finalSHA := addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2", runID: "run_x"}, nil)

	if res.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q, want %q", res.FinalCommit, finalSHA)
	}
	if res.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("FinalBranch = %q", res.FinalBranch)
	}
	if res.MergedInto != "main" {
		t.Errorf("MergedInto = %q, want main", res.MergedInto)
	}
	// And main really moved.
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip != finalSHA {
		t.Errorf("main tip = %s, want %s", mainTip, finalSHA)
	}
}

// TestFinalizeWorktree_DirtyMain_SkipsFF — commits in the worktree but
// the main checkout has uncommitted changes → branch created (safety
// net) but FF skipped (we don't touch a dirty tree).
func TestFinalizeWorktree_DirtyMain_SkipsFF(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	// Dirty the main worktree before finalize.
	writeFile(t, filepath.Join(repo, "wip.txt"), "uncommitted\n")

	finalSHA := addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2"}, nil)

	if res.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q", res.FinalCommit)
	}
	if res.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("FinalBranch = %q", res.FinalBranch)
	}
	if res.MergedInto != "" {
		t.Errorf("MergedInto = %q, want empty (dirty main blocks FF)", res.MergedInto)
	}
	// Main should still point at the original tip.
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip != originalTip {
		t.Errorf("main tip moved to %s, want still at %s", mainTip, originalTip)
	}
}

// TestFinalizeWorktree_NonFF_SkipsFF — main has commits the worktree
// doesn't, so no FF possible → branch created, FF skipped, main unchanged.
func TestFinalizeWorktree_NonFF_SkipsFF(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	// Main advances independently (e.g. user committed in another tab).
	writeFile(t, filepath.Join(repo, "side.txt"), "side\n")
	mustRun(t, repo, "git", "add", "side.txt")
	mustRun(t, repo, "git", "commit", "-m", "side commit")
	mainTipAfter := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))

	finalSHA := addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2"}, nil)

	if res.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q", res.FinalCommit)
	}
	if res.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("FinalBranch = %q", res.FinalBranch)
	}
	if res.MergedInto != "" {
		t.Errorf("MergedInto = %q, want empty (non-FF blocks merge)", res.MergedInto)
	}
	// Main should still point at the side commit, not at the run's commit.
	cur := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if cur != mainTipAfter {
		t.Errorf("main tip = %s, want %s (unchanged)", cur, mainTipAfter)
	}
}

// TestFinalizeWorktree_OptOutNone — mergeInto="none" disables the FF
// even when it would otherwise succeed. Branch is still created.
func TestFinalizeWorktree_OptOutNone(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	finalSHA := addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2", mergeInto: "none"}, nil)

	if res.FinalCommit != finalSHA || res.FinalBranch == "" {
		t.Errorf("expected branch + commit, got %+v", res)
	}
	if res.MergedInto != "" {
		t.Errorf("MergedInto should be empty with mergeInto=none, got %q", res.MergedInto)
	}
	// Main untouched.
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip != originalTip {
		t.Errorf("main tip moved despite none, %s != %s", mainTip, originalTip)
	}
}

// TestFinalizeWorktree_BranchNameOverride — when branchName is set,
// the storage branch uses that exact name (no iterion/run/ prefix).
func TestFinalizeWorktree_BranchNameOverride(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2", branchName: "feat/auto-fixes"}, nil)

	if res.FinalBranch != "feat/auto-fixes" {
		t.Errorf("FinalBranch = %q, want feat/auto-fixes", res.FinalBranch)
	}
	out, _ := exec.Command("git", "-C", repo, "branch", "--list", "feat/auto-fixes").Output()
	if !strings.Contains(string(out), "feat/auto-fixes") {
		t.Errorf("override branch not created: %q", string(out))
	}
}

// TestFinalizeWorktree_BranchNameCollision — when the default branch
// already exists, finalize should fall back to a numeric suffix
// instead of failing or overwriting.
func TestFinalizeWorktree_BranchNameCollision(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	// Pre-create the would-be default branch on some earlier commit.
	mustRun(t, repo, "git", "branch", "iterion/run/swift-cedar-a3f2", originalTip)

	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	finalSHA := addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2"}, nil)

	if res.FinalBranch == "" {
		t.Fatal("expected fallback branch on collision")
	}
	if !strings.HasPrefix(res.FinalBranch, "iterion/run/swift-cedar-a3f2-") {
		t.Errorf("expected suffixed fallback, got %q", res.FinalBranch)
	}
	// And the fallback branch points at the run's commit.
	tip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", res.FinalBranch)))
	if tip != finalSHA {
		t.Errorf("fallback branch tip = %s, want %s", tip, finalSHA)
	}
}

// TestFinalizeWorktree_DetachedAtStart — when originalBranch is empty
// (the main repo was on a detached HEAD at run start), the FF must be
// skipped — there's no branch to advance.
func TestFinalizeWorktree_DetachedAtStart(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	mustRun(t, repo, "git", "checkout", "--detach", "HEAD")

	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "", // detached
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2"}, nil)

	if res.FinalBranch == "" {
		t.Errorf("branch should still be created as GC guard")
	}
	if res.MergedInto != "" {
		t.Errorf("FF must be skipped when started detached, got merged into %q", res.MergedInto)
	}
}

// TestResolveMergeTarget — small unit test on the value parsing.
func TestResolveMergeTarget(t *testing.T) {
	cases := []struct {
		name           string
		mergeInto      string
		originalBranch string
		want           string
	}{
		{"empty defaults to current", "", "main", "main"},
		{"current alias", "current", "main", "main"},
		{"none opts out", "none", "main", ""},
		{"explicit branch", "release", "main", "release"},
		{"none case-insensitive", "NONE", "main", ""},
		{"current trims spaces", "  current ", "main", "main"},
		{"empty + detached → empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveMergeTarget(tc.mergeInto, tc.originalBranch)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
