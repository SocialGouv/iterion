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
	}, finalizeOptions{runName: "swift-cedar-a3f2", runID: "run_x", autoMerge: true, mergeStrategy: "merge"}, nil)

	if res.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q, want %q", res.FinalCommit, finalSHA)
	}
	if res.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("FinalBranch = %q", res.FinalBranch)
	}
	if res.MergedInto != "main" {
		t.Errorf("MergedInto = %q, want main", res.MergedInto)
	}
	if res.MergeStatus != "merged" {
		t.Errorf("MergeStatus = %q, want merged", res.MergeStatus)
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
	}, finalizeOptions{runName: "swift-cedar-a3f2", autoMerge: true, mergeStrategy: "merge"}, nil)

	if res.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q", res.FinalCommit)
	}
	if res.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("FinalBranch = %q", res.FinalBranch)
	}
	if res.MergedInto != "" {
		t.Errorf("MergedInto = %q, want empty (dirty main blocks FF)", res.MergedInto)
	}
	if res.MergeStatus != "failed" {
		t.Errorf("MergeStatus = %q, want failed", res.MergeStatus)
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
	}, finalizeOptions{runName: "swift-cedar-a3f2", autoMerge: true, mergeStrategy: "merge"}, nil)

	if res.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q", res.FinalCommit)
	}
	if res.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("FinalBranch = %q", res.FinalBranch)
	}
	if res.MergedInto != "" {
		t.Errorf("MergedInto = %q, want empty (non-FF blocks merge)", res.MergedInto)
	}
	if res.MergeStatus != "failed" {
		t.Errorf("MergeStatus = %q, want failed", res.MergeStatus)
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
	}, finalizeOptions{runName: "swift-cedar-a3f2", mergeInto: "none", autoMerge: true, mergeStrategy: "merge"}, nil)

	if res.FinalCommit != finalSHA || res.FinalBranch == "" {
		t.Errorf("expected branch + commit, got %+v", res)
	}
	if res.MergedInto != "" {
		t.Errorf("MergedInto should be empty with mergeInto=none, got %q", res.MergedInto)
	}
	if res.MergeStatus != "skipped" {
		t.Errorf("MergeStatus = %q, want skipped", res.MergeStatus)
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
	}, finalizeOptions{runName: "swift-cedar-a3f2", branchName: "feat/auto-fixes", autoMerge: true, mergeStrategy: "merge"}, nil)

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
	}, finalizeOptions{runName: "swift-cedar-a3f2", autoMerge: true, mergeStrategy: "merge"}, nil)

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
	}, finalizeOptions{runName: "swift-cedar-a3f2", autoMerge: true, mergeStrategy: "merge"}, nil)

	if res.FinalBranch == "" {
		t.Errorf("branch should still be created as GC guard")
	}
	if res.MergedInto != "" {
		t.Errorf("FF must be skipped when started detached, got merged into %q", res.MergedInto)
	}
}

// TestFinalizeWorktree_DeferredMerge_AutoMergeOff — when autoMerge is
// false (the editor's default), finalize creates the storage branch
// but stops short of touching the user's main branch. The result
// reports MergeStatus=pending so the editor can offer a UI action.
func TestFinalizeWorktree_DeferredMerge_AutoMergeOff(t *testing.T) {
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
	}, finalizeOptions{runName: "swift-cedar-a3f2", runID: "run_x" /* autoMerge omitted = false */}, nil)

	if res.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q, want %q", res.FinalCommit, finalSHA)
	}
	if res.FinalBranch == "" {
		t.Errorf("FinalBranch should still be created as GC guard")
	}
	if res.MergedInto != "" {
		t.Errorf("MergedInto = %q, want empty (deferred)", res.MergedInto)
	}
	if res.MergeStatus != "pending" {
		t.Errorf("MergeStatus = %q, want pending", res.MergeStatus)
	}
	// Main untouched.
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip != originalTip {
		t.Errorf("main tip moved despite deferred merge, %s != %s", mainTip, originalTip)
	}
}

// TestFinalizeWorktree_SquashStrategy — autoMerge=true + squash
// collapses the run's commits into one commit on top of main.
func TestFinalizeWorktree_SquashStrategy(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	addCommit(t, wt, "a.go", "package main\n// a\n", "feat: add a")
	addCommit(t, wt, "b.go", "package main\n// b\n", "feat: add b")
	finalSHA := addCommit(t, wt, "c.go", "package main\n// c\n", "feat: add c")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "swift-cedar-a3f2", runID: "run_x", autoMerge: true, mergeStrategy: "squash"}, nil)

	if res.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q, want %q", res.FinalCommit, finalSHA)
	}
	if res.MergedInto != "main" {
		t.Errorf("MergedInto = %q, want main", res.MergedInto)
	}
	if res.MergeStatus != "merged" {
		t.Errorf("MergeStatus = %q, want merged", res.MergeStatus)
	}
	if res.MergedCommit == "" || res.MergedCommit == finalSHA {
		t.Errorf("MergedCommit should be a fresh squash SHA distinct from FinalCommit; got %q (final %q)", res.MergedCommit, finalSHA)
	}
	// Main should be one commit ahead of originalTip — not three.
	count := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-list", "--count", originalTip+"..main")))
	if count != "1" {
		t.Errorf("main has %s commits past base, want 1 squash commit", count)
	}
}

// TestBuildSquashMessage_SingleCommit — when the run produced one
// commit, the squash title is that commit's subject (a meaningful
// conventional-commit message), and the body is omitted because
// re-listing the same line would be redundant.
func TestBuildSquashMessage_SingleCommit(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	finalSHA := addCommit(t, wt, "a.go", "package main\n// a\n", "feat(privacy): add pure-Go privacy_filter tools")

	got := buildSquashMessage(repo, originalTip, finalSHA, "plain-basalt-0d49")
	want := "feat(privacy): add pure-Go privacy_filter tools\n"
	if got != want {
		t.Errorf("squash message:\n got: %q\nwant: %q", got, want)
	}
}

// TestBuildSquashMessage_MultipleCommitsListsAll — N commits → title is
// the first commit's subject, body lists every commit chronologically.
// This preserves the per-iteration audit trail when the workflow's
// commit phase produced more than one logical step.
func TestBuildSquashMessage_MultipleCommitsListsAll(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	addCommit(t, wt, "a.go", "package main\n// a\n", "feat(api): add v2 endpoint")
	addCommit(t, wt, "b.go", "package main\n// b\n", "test(api): cover v2 happy path")
	finalSHA := addCommit(t, wt, "c.go", "package main\n// c\n", "docs(api): document v2 contract")

	got := buildSquashMessage(repo, originalTip, finalSHA, "swift-cedar-a3f2")
	if !strings.HasPrefix(got, "feat(api): add v2 endpoint\n\n") {
		t.Errorf("first line should be the first commit's subject + blank, got:\n%s", got)
	}
	for _, want := range []string{
		"- ",
		" feat(api): add v2 endpoint\n",
		" test(api): cover v2 happy path\n",
		" docs(api): document v2 contract\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q:\n%s", want, got)
		}
	}
	// runName must NOT leak into the message when commits are readable.
	if strings.Contains(got, "swift-cedar-a3f2") {
		t.Errorf("runName leaked into message body:\n%s", got)
	}
}

// TestBuildSquashMessage_FallsBackToRunName — when no commits are
// readable in base..head (degenerate: empty range, bad refs), the
// title degrades to the runName so the deferred-merge UI still produces
// a non-empty commit message.
func TestBuildSquashMessage_FallsBackToRunName(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	// Same SHA on both sides → empty `git log` output → fallback path.
	got := buildSquashMessage(repo, originalTip, originalTip, "plain-basalt-0d49")
	if got != "plain-basalt-0d49\n" {
		t.Errorf("squash message: %q, want %q", got, "plain-basalt-0d49\n")
	}
}

// TestBuildSquashMessage_FallsBackToDefault — no commits AND no runName
// (both extremes degraded) → "iterion run" sentinel keeps git happy.
func TestBuildSquashMessage_FallsBackToDefault(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	got := buildSquashMessage(repo, originalTip, originalTip, "")
	if got != "iterion run\n" {
		t.Errorf("squash message: %q, want %q", got, "iterion run\n")
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
