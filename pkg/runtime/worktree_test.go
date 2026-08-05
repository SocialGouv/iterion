package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
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

// TestFinalizeWorktree_RefusesRepoRootAsWorktree — the safety invariant:
// when the "worktree" path IS the repo root (a phantom-worktree run whose
// WorkDir collapsed to the operator's live checkout), finalize must NEVER
// git-commit there — doing so lands uncommitted work on the operator's
// CURRENT branch. It refuses: no bank commit, no storage branch, and it
// signals PreserveWorktree so the caller won't `git worktree remove` the
// main repo.
func TestFinalizeWorktree_RefusesRepoRootAsWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	// Dirty the live checkout with an uncommitted change — stands in for
	// the operator's unrelated WIP (or the run's own uncommitted output).
	writeFile(t, filepath.Join(repo, "dirty.txt"), "uncommitted\n")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         repo, // the phantom: "worktree" == main checkout
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "phantom-run-0001", runID: "run_p"}, nil)

	if res.FinalCommit != "" || res.FinalBranch != "" {
		t.Fatalf("expected no promotion when wtPath==repoRoot, got %+v", res)
	}
	if !res.PreserveWorktree {
		t.Errorf("PreserveWorktree = false, want true (must not clean the live checkout)")
	}
	// The operator's branch must be UNTOUCHED — no wip commit created.
	if headNow := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "HEAD"))); headNow != originalTip {
		t.Errorf("repo HEAD moved to %s (want %s) — finalize committed on the operator's branch", headNow, originalTip)
	}
	// The uncommitted change must still be uncommitted (not banked away).
	if st := strings.TrimSpace(string(mustOutput(t, repo, "git", "status", "--porcelain"))); st == "" {
		t.Errorf("working tree is clean — finalize banked the uncommitted change instead of leaving it")
	}
	// No storage branch either.
	if br, _ := exec.Command("git", "-C", repo, "branch", "--list", "iterion/run/*").Output(); strings.TrimSpace(string(br)) != "" {
		t.Errorf("storage branch created: %q — expected none", string(br))
	}
}

// TestRecoverFinalize_SkipsPhantomWorktree — RecoverFinalize on a run
// whose persisted WorkDir == RepoRoot (the phantom) must be a no-op: it
// must not bank the live checkout's uncommitted changes onto the
// operator's branch, and must not stamp FinalCommit/FinalBranch.
func TestRecoverFinalize_SkipsPhantomWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	writeFile(t, filepath.Join(repo, "dirty.txt"), "uncommitted\n")

	r := &store.Run{
		ID:         "run_phantom",
		Worktree:   true,
		WorkDir:    repo,
		RepoRoot:   repo, // collapsed to the operator's live checkout
		BaseCommit: originalTip,
		Status:     store.RunStatusCancelled,
	}
	if err := RecoverFinalize(context.Background(), tmpStore(t), r, nil); err != nil {
		t.Fatalf("RecoverFinalize returned error: %v", err)
	}
	if r.FinalCommit != "" || r.FinalBranch != "" {
		t.Errorf("RecoverFinalize stamped finalization (%q/%q) — expected skip", r.FinalCommit, r.FinalBranch)
	}
	if headNow := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "HEAD"))); headNow != originalTip {
		t.Errorf("repo HEAD moved to %s (want %s) — recovery committed on the operator's branch", headNow, originalTip)
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
// false (the studio's default), finalize creates the storage branch
// but stops short of touching the user's main branch. The result
// reports MergeStatus=pending so the studio can offer a UI action.
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
// commit, the squash uses that commit's full message (subject + body)
// verbatim. No information is lost vs. a non-squash merge: the
// detailed conventional-commit body the workflow authored survives
// the squash onto main.
func TestBuildSquashMessage_SingleCommit(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	fullMessage := "feat(privacy): add pure-Go privacy_filter tools\n\nDetect and redact 5 PII categories.\nNo Python, no ONNX."
	writeFile(t, filepath.Join(wt, "a.go"), "package main\n// a\n")
	mustRun(t, wt, "git", "add", "a.go")
	mustRun(t, wt, "git", "commit", "-m", fullMessage)
	finalSHA := strings.TrimSpace(string(mustOutput(t, wt, "git", "rev-parse", "HEAD")))

	got := buildSquashMessage(repo, originalTip, finalSHA, "plain-basalt-0d49")
	want := fullMessage + "\n"
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

// TestRecoverFinalize_HappyPath simulates a run that reached
// status=finished but lost its finalization metadata (daemon SIGTERM
// between "Run finished" and SaveRun(final_*)). RecoverFinalize on
// startup should detect the orphan, promote the worktree HEAD to a
// persistent branch, and persist FinalCommit/FinalBranch/MergeStatus
// to run.json. Reproduces the 2026-05-14 run_1778749561103 scenario.
func TestRecoverFinalize_HappyPath(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	finalSHA := addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	// Filesystem store with a temp root.
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	r := &store.Run{
		ID:         "run_test_recover_finalize",
		Name:       "swift-cedar-a3f2",
		Status:     store.RunStatusFinished, // engine got this far
		Worktree:   true,
		WorkDir:    wt,
		RepoRoot:   repo,
		BaseCommit: originalTip,
		// FinalCommit / FinalBranch deliberately empty — the failure
		// mode RecoverFinalize is meant to repair.
	}
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if r.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q, want %q", r.FinalCommit, finalSHA)
	}
	if r.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("FinalBranch = %q", r.FinalBranch)
	}
	// And the run was persisted back: re-load and check.
	r2, err := st.LoadRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if r2.FinalCommit != finalSHA || r2.FinalBranch != "iterion/run/swift-cedar-a3f2" {
		t.Errorf("persisted final_* mismatch: %+v", r2)
	}
	// And the branch actually exists in the repo.
	out, _ := exec.Command("git", "-C", repo, "rev-parse", "iterion/run/swift-cedar-a3f2").Output()
	if got := strings.TrimSpace(string(out)); got != finalSHA {
		t.Errorf("branch tip = %q, want %q", got, finalSHA)
	}
}

// TestRecoverFinalize_Idempotent — calling RecoverFinalize a second
// time on an already-finalized run must be a no-op (no error, no
// re-creation). Important because reconcileOrphans calls it on every
// run scanned at startup.
func TestRecoverFinalize_Idempotent(t *testing.T) {
	st, _ := store.New(t.TempDir())
	r := &store.Run{
		ID:          "run_already_done",
		Status:      store.RunStatusFinished,
		Worktree:    true,
		WorkDir:     "/tmp/wt",
		RepoRoot:    "/tmp/repo",
		BaseCommit:  "abc",
		FinalCommit: "def",
		FinalBranch: "iterion/run/already",
	}
	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("idempotent call errored: %v", err)
	}
	if r.FinalCommit != "def" || r.FinalBranch != "iterion/run/already" {
		t.Errorf("idempotent call mutated state: %+v", r)
	}
}

// TestRecoverFinalize_SkipsNonWorktree — a run without worktree
// (worktree: none in the workflow, or never set) must be a no-op
// regardless of status — there's no worktree HEAD to promote.
func TestRecoverFinalize_SkipsNonWorktree(t *testing.T) {
	st, _ := store.New(t.TempDir())
	r := &store.Run{
		ID:       "run_no_worktree",
		Status:   store.RunStatusFinished,
		Worktree: false,
	}
	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("non-worktree path errored: %v", err)
	}
	if r.FinalCommit != "" || r.FinalBranch != "" {
		t.Errorf("non-worktree path mutated state: %+v", r)
	}
}

// TestRecoverFinalize_SkipsNonFinished — failed_resumable + failed
// runs must be no-ops. failed_resumable runs keep their worktree for
// resume; pre-finalizing there would leave a stale branch the operator
// then has to disambiguate from the post-resume finalize's branch.
// failed runs are excluded for a different reason: a hard failure
// suggests the commits aren't safe to expose to a one-click merge.
func TestRecoverFinalize_SkipsNonFinished(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunStatusFailedResumable, store.RunStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			st, _ := store.New(t.TempDir())
			r := &store.Run{
				ID:       "run_" + string(status),
				Status:   status,
				Worktree: true,
				WorkDir:  "/tmp/wt",
				RepoRoot: "/tmp/repo",
			}
			if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
				t.Fatalf("non-finished path errored: %v", err)
			}
			if r.FinalCommit != "" || r.FinalBranch != "" {
				t.Errorf("non-finished path mutated state: %+v", r)
			}
		})
	}
}

// TestRecoverFinalize_CancelledRun — a run the operator cancelled with
// commits in the worktree MUST be finalized so the merge UI can act on
// the partial work. Without this, "Squash and merge" fails with "no
// storage branch" and the operator has to recover by hand.
func TestRecoverFinalize_CancelledRun(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	finalSHA := addCommit(t, wt, "partial.go", "package main\n", "feat: partial work")

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	r := &store.Run{
		ID:         "run_cancelled_partial",
		Name:       "fierce-oak-c9d4",
		Status:     store.RunStatusCancelled,
		Worktree:   true,
		WorkDir:    wt,
		RepoRoot:   repo,
		BaseCommit: originalTip,
	}
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if r.FinalCommit != finalSHA {
		t.Errorf("FinalCommit = %q, want %q", r.FinalCommit, finalSHA)
	}
	if r.FinalBranch != "iterion/run/fierce-oak-c9d4" {
		t.Errorf("FinalBranch = %q", r.FinalBranch)
	}
}

// TestFinalizeWorktree_WipBanksDirtyWorktree — a run that finished with
// UNCOMMITTED changes and an unchanged HEAD (bot exited through a
// non-commit edge). Before the wip-bank fix this was silent total loss:
// finalize no-op'd ("no commits produced") and the force-remove cleanup
// destroyed the files. Now finalize banks the dirty tree as a wip commit
// on the storage branch — and NEVER merges it into the operator's branch.
func TestFinalizeWorktree_WipBanksDirtyWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	// Uncommitted work: one new file + one modified tracked file.
	writeFile(t, filepath.Join(wt, "new_feature.go"), "package main\n")
	writeFile(t, filepath.Join(wt, "README.md"), "init\nmodified\n")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-bank-test", runID: "run_w", autoMerge: true, mergeStrategy: "merge"}, nil)

	if !res.WipBanked {
		t.Fatalf("expected WipBanked=true, got %+v", res)
	}
	if res.PreserveWorktree {
		t.Fatalf("bank succeeded — PreserveWorktree must be false, got %+v", res)
	}
	if res.FinalCommit == "" || res.FinalCommit == originalTip {
		t.Fatalf("expected a banked commit distinct from originalTip, got %+v", res)
	}
	if res.FinalBranch == "" {
		t.Fatalf("expected a storage branch on the banked commit, got %+v", res)
	}
	if res.MergeStatus != "skipped" || res.MergedInto != "" {
		t.Fatalf("a wip-banked HEAD must never merge (want skipped), got %+v", res)
	}
	// The operator's branch must NOT have moved.
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip != originalTip {
		t.Fatalf("main moved to %s — a wip bank must never touch the operator's branch", mainTip)
	}
	// The banked commit really contains the uncommitted work.
	show := string(mustOutput(t, repo, "git", "show", "--stat", "--format=%s", res.FinalCommit))
	if !strings.Contains(show, "wip(iterion)") || !strings.Contains(show, "new_feature.go") || !strings.Contains(show, "README.md") {
		t.Fatalf("banked commit missing expected content:\n%s", show)
	}
}

// TestFinalizeWorktree_WipBankResidueOnTopOfCommits — the run committed
// work AND left extra uncommitted residue. The residue is banked as a
// child wip commit; the storage branch holds both, and because the tip
// is a wip bank the merge is skipped (the real commits stay reviewable
// on the branch, nothing lands silently).
func TestFinalizeWorktree_WipBankResidueOnTopOfCommits(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	agentSHA := addCommit(t, wt, "feature.go", "package main\n", "feat: real work")
	writeFile(t, filepath.Join(wt, "residue.go"), "package main\n")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-residue-test", runID: "run_r", autoMerge: true, mergeStrategy: "merge"}, nil)

	if !res.WipBanked {
		t.Fatalf("expected WipBanked=true, got %+v", res)
	}
	if res.FinalCommit == agentSHA || res.FinalCommit == originalTip {
		t.Fatalf("expected banked tip above the agent commit, got %+v", res)
	}
	// The banked tip's parent is the agent's real commit.
	parent := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", res.FinalCommit+"^")))
	if parent != agentSHA {
		t.Fatalf("banked commit parent = %s, want agent commit %s", parent, agentSHA)
	}
	if res.MergeStatus != "skipped" || res.MergedInto != "" {
		t.Fatalf("wip-banked tip must skip the merge, got %+v", res)
	}
	mainTip := strings.TrimSpace(string(mustOutput(t, repo, "git", "rev-parse", "main")))
	if mainTip != originalTip {
		t.Fatalf("main moved to %s — must stay at %s", mainTip, originalTip)
	}
}

// TestSetupWorktree_AnchorsOnTheLaunchCheckout pins the tree a run actually
// describes. `iterion run` launched from a LINKED worktree used to anchor on
// the MAIN checkout's HEAD, because the repo root is deliberately resolved up
// to the main repository and `HEAD` was then read there.
//
// The failure mode is the dangerous kind: the run succeeds. It reads the main
// branch's files, does whatever they imply, and reports done — while the
// operator is looking at another branch. Nothing in the output said which
// tree was used.
func TestSetupWorktree_AnchorsOnTheLaunchCheckout(t *testing.T) {
	main, firstSHA := initBareishRepo(t)

	// A linked worktree pinned to the FIRST commit, on its own branch.
	linked := filepath.Join(t.TempDir(), "linked")
	mustRun(t, main, "git", "worktree", "add", "-b", "side", linked, firstSHA)

	// main moves on. The two checkouts now disagree, which is the whole point.
	secondSHA := addCommit(t, main, "moved.txt", "on main only\n", "second")
	if secondSHA == firstSHA {
		t.Fatalf("the fixture did not move main: %s", secondSHA)
	}

	wc, cleanup, err := setupWorktree(t.TempDir(), "run-anchor", linked, nil)
	if err != nil {
		t.Fatalf("setupWorktree from a linked worktree: %v", err)
	}
	defer cleanup()

	got := strings.TrimSpace(string(mustOutput(t, wc.wtPath, "git", "rev-parse", "HEAD")))
	if got != firstSHA {
		t.Errorf("run worktree anchored at %s, want the launch checkout's %s (main is at %s)",
			shortSHA(got), shortSHA(firstSHA), shortSHA(secondSHA))
	}
	if wc.originalBranch != "side" {
		t.Errorf("originalBranch = %q, want %q — finalize would fast-forward the wrong branch", wc.originalBranch, "side")
	}
	if wc.originalTip != firstSHA {
		t.Errorf("originalTip = %s, want %s", shortSHA(wc.originalTip), shortSHA(firstSHA))
	}
	if !samePath(wc.anchor(), linked) {
		t.Errorf("anchor() = %q, want the launch checkout %q", wc.anchor(), linked)
	}
	// The file that exists only on main must NOT be there: that is the
	// observable form of "this is the tree you asked for".
	if _, statErr := os.Stat(filepath.Join(wc.wtPath, "moved.txt")); statErr == nil {
		t.Errorf("the run worktree carries main's file — it was checked out from the wrong tree")
	}
}

// TestSetupWorktree_SingleCheckoutUnchanged is the other half: with no linked
// worktree in play, the anchor IS the repo root and nothing about the previous
// behaviour moves.
func TestSetupWorktree_SingleCheckoutUnchanged(t *testing.T) {
	main, firstSHA := initBareishRepo(t)

	wc, cleanup, err := setupWorktree(t.TempDir(), "run-plain", main, nil)
	if err != nil {
		t.Fatalf("setupWorktree: %v", err)
	}
	defer cleanup()

	if !samePath(wc.anchor(), wc.repoRoot) {
		t.Errorf("anchor() = %q, repoRoot = %q — they must coincide for a single checkout", wc.anchor(), wc.repoRoot)
	}
	got := strings.TrimSpace(string(mustOutput(t, wc.wtPath, "git", "rev-parse", "HEAD")))
	if got != firstSHA {
		t.Errorf("run worktree at %s, want %s", shortSHA(got), shortSHA(firstSHA))
	}
	if wc.originalBranch != "main" {
		t.Errorf("originalBranch = %q, want %q", wc.originalBranch, "main")
	}
}
