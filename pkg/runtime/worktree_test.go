package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

type saveFailRunStore struct {
	store.RunStore
}

func (saveFailRunStore) SaveRun(context.Context, *store.Run) error {
	return errors.New("injected SaveRun failure")
}

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

func addOwnedWorktree(t *testing.T, st store.RunStore, repo, runID string) string {
	t.Helper()
	wt := filepath.Join(st.Root(), "worktrees", runID)
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatalf("create owned worktree parent: %v", err)
	}
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })
	return wt
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

func testWorktreeAuthority() time.Time {
	// Most cleanup tests do not launch an inaccessible child. Keep their
	// synthetic boundary close to the census so unrelated processes created by
	// concurrently-running packages do not become false in-scope blockers.
	// Tests for denied children capture an explicit pre-launch boundary.
	return time.Now().UTC()
}

func assumeQuiescentProcessCensus(t *testing.T) {
	t.Helper()
	previous := cleanupProcessReferences
	cleanupProcessReferences = func(string, time.Time) ([]int, error) {
		return nil, nil
	}
	t.Cleanup(func() {
		cleanupProcessReferences = previous
	})
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

// TestFinalizeWorktree_PreservesWhenCleanlinessUnknown proves that a failed
// `git status` probe is not treated as a clean worktree. The linked worktree's
// index is corrupted after adding an untracked output file: rev-parse HEAD
// remains readable, but status fails. Finalization must leave every file and
// ref untouched and signal the caller to skip force-removal.
func TestFinalizeWorktree_PreservesWhenCleanlinessUnknown(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	writeFile(t, filepath.Join(wt, "run-output.txt"), "must survive\n")
	indexPath := strings.TrimSpace(string(mustOutput(t, wt, "git", "rev-parse", "--git-path", "index")))
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(wt, indexPath)
	}
	indexBefore, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read worktree index: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(indexPath, indexBefore, 0o644) })
	if err := os.WriteFile(indexPath, []byte("not a git index"), 0o644); err != nil {
		t.Fatalf("corrupt worktree index: %v", err)
	}

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "status-error", runID: "run_status_error"}, nil)

	if !res.PreserveWorktree {
		t.Fatalf("PreserveWorktree=false, want true after failed cleanliness probe: %+v", res)
	}
	if res.FinalCommit != "" || res.FinalBranch != "" {
		t.Fatalf("cleanliness-unknown finalization must not promote partial state: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(wt, "run-output.txt")); err != nil {
		t.Fatalf("run output was not preserved: %v", err)
	}
}

// TestFinalizeWorktree_PreservesWhenStorageBranchFails creates a ref namespace
// collision (`refs/heads/iterion` blocks `refs/heads/iterion/run/...`). The
// commit has no storage ref, so removing its worktree would leave it reachable
// only through reflog; finalization must fail closed.
func TestFinalizeWorktree_PreservesWhenStorageBranchFails(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	finalSHA := addCommit(t, wt, "feature.go", "package feature\n", "feat: branch collision")
	mustRun(t, repo, "git", "branch", "iterion", originalTip)

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "branch-error", runID: "run_branch_error"}, nil)

	if res.FinalCommit != finalSHA {
		t.Fatalf("FinalCommit=%q, want %q: %+v", res.FinalCommit, finalSHA, res)
	}
	if res.FinalBranch != "" || res.FinalBranchError == "" {
		t.Fatalf("expected an explicit storage-branch failure: %+v", res)
	}
	if !res.PreserveWorktree {
		t.Fatalf("PreserveWorktree=false, want true without a durable storage ref: %+v", res)
	}
}

// TestFinalizeOnExit_ReviewGateProbeErrorPreservesWorktree covers the separate
// idempotency path used after an interaction:review gate already finalized the
// commits. If the final status probe fails, cleanup must not run: the runtime
// cannot prove that post-gate output is absent.
func TestFinalizeOnExit_ReviewGateProbeErrorPreservesWorktree(t *testing.T) {
	st := tmpStore(t)
	const runID = "run_review_gate_probe_error"
	r := &store.Run{
		ID:          runID,
		Status:      store.RunStatusFinished,
		MergeStatus: store.MergeStatusSkipped,
	}
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	nonGitWorkdir := t.TempDir()
	eng := &Engine{store: st}
	cleanupCalled := false
	eng.finalizeOnExit(context.Background(), runID, &worktreeContext{
		repoRoot: t.TempDir(),
		wtPath:   nonGitWorkdir,
	}, func(string, string) (WorktreeCleanupResult, error) {
		cleanupCalled = true
		return WorktreeCleanupResult{}, nil
	}, nil)

	if cleanupCalled {
		t.Fatal("review-gate finalize called cleanup after cleanliness probe failed")
	}
}

// TestFinalizeOnExit_ReviewGatePostCommitPreservesWorktree ensures a clean
// post-review commit is not mistaken for the exact HEAD the review gate
// protected. Cleanliness alone cannot authorize deletion.
func TestFinalizeOnExit_ReviewGatePostCommitPreservesWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })

	reviewedSHA := addCommit(t, wt, "reviewed.go", "package reviewed\n", "feat: reviewed output")
	const finalBranch = "iterion/run/reviewed-output"
	mustRun(t, repo, "git", "branch", finalBranch, reviewedSHA)
	postReviewSHA := addCommit(t, wt, "after.go", "package after\n", "feat: post-review output")

	st := tmpStore(t)
	const runID = "run_review_gate_post_commit"
	if err := st.SaveRun(context.Background(), &store.Run{
		ID:          runID,
		Status:      store.RunStatusFinished,
		MergeStatus: store.MergeStatusSkipped,
		FinalCommit: reviewedSHA,
		FinalBranch: finalBranch,
	}); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	eng := &Engine{store: st}
	cleanupCalled := false
	eng.finalizeOnExit(context.Background(), runID, &worktreeContext{
		repoRoot:    repo,
		wtPath:      wt,
		originalTip: originalTip,
	}, func(string, string) (WorktreeCleanupResult, error) {
		cleanupCalled = true
		return WorktreeCleanupResult{}, nil
	}, nil)

	if cleanupCalled {
		t.Fatal("review-gate finalize removed a clean post-review commit")
	}
	if got := readHEAD(wt); got != postReviewSHA {
		t.Fatalf("post-review HEAD changed: got %q, want %q", got, postReviewSHA)
	}
	if got, err := readBranchCommit(repo, finalBranch); err != nil || got != reviewedSHA {
		t.Fatalf("review branch changed: got=%q err=%v, want %q", got, err, reviewedSHA)
	}
}

// TestFinalizeOnExit_MetadataSaveFailurePreservesWorktree pins the ordering
// between branch promotion, run metadata durability, and removal.
func TestFinalizeOnExit_MetadataSaveFailurePreservesWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	baseStore := tmpStore(t)
	const runID = "run_finalize_metadata_failure"
	wt := addOwnedWorktree(t, baseStore, repo, runID)
	finalSHA := addCommit(t, wt, "durable.go", "package durable\n", "feat: durable output")

	if err := baseStore.SaveRun(context.Background(), &store.Run{
		ID:         runID,
		Name:       "metadata-failure",
		Status:     store.RunStatusFinished,
		Worktree:   true,
		WorkDir:    wt,
		RepoRoot:   repo,
		BaseCommit: originalTip,
	}); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	eng := &Engine{
		store:   saveFailRunStore{RunStore: baseStore},
		runName: "metadata-failure",
	}
	cleanupCalled := false
	eng.finalizeOnExit(context.Background(), runID, &worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, func(string, string) (WorktreeCleanupResult, error) {
		cleanupCalled = true
		return WorktreeCleanupResult{}, nil
	}, nil)

	if cleanupCalled {
		t.Fatal("finalize removed worktree after metadata save failure")
	}
	if got := readHEAD(wt); got != finalSHA {
		t.Fatalf("worktree HEAD changed: got %q, want %q", got, finalSHA)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree not preserved: %v", err)
	}
	if got, err := readBranchCommit(repo, "iterion/run/metadata-failure"); err != nil || got != finalSHA {
		t.Fatalf("storage branch was not durably created: got=%q err=%v, want %q", got, err, finalSHA)
	}
	persisted, err := baseStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if persisted.FinalCommit != "" || persisted.FinalBranch != "" {
		t.Fatalf("failed save leaked finalization metadata: %+v", persisted)
	}
}

// TestRecoverFinalize_ReconcilesAutoMergeAfterMetadataFailure covers the
// two-phase crash window where the squash landed on main but SaveRun failed.
// Recovery must not tell the UI the merge was skipped.
func TestRecoverFinalize_ReconcilesAutoMergeAfterMetadataFailure(t *testing.T) {
	assumeQuiescentProcessCensus(t)
	repo, originalTip := initBareishRepo(t)
	baseStore := tmpStore(t)
	const runID = "run_automerge_metadata_failure"
	wt := addOwnedWorktree(t, baseStore, repo, runID)
	finalSHA := addCommit(t, wt, "landed.go", "package landed\n", "feat: landed output")

	seed := &store.Run{
		ID:                runID,
		Name:              "automerge-save-failure",
		Status:            store.RunStatusFinished,
		Worktree:          true,
		WorkDir:           wt,
		RepoRoot:          repo,
		BaseCommit:        originalTip,
		WorktreeCreatedAt: testWorktreeAuthority(),
		AutoMerge:         true,
		MergeStrategy:     store.MergeStrategySquash,
	}
	if err := baseStore.SaveRun(context.Background(), seed); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	eng := &Engine{
		store:         saveFailRunStore{RunStore: baseStore},
		runName:       seed.Name,
		autoMerge:     true,
		mergeStrategy: string(store.MergeStrategySquash),
	}
	cleanupCalled := false
	eng.finalizeOnExit(context.Background(), runID, &worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, func(string, string) (WorktreeCleanupResult, error) {
		cleanupCalled = true
		return WorktreeCleanupResult{}, nil
	}, nil)
	if cleanupCalled {
		t.Fatal("metadata failure removed worktree after successful Git merge")
	}
	mainAfterMerge := readHEAD(repo)
	if mainAfterMerge == "" || mainAfterMerge == originalTip {
		t.Fatalf("precondition: squash did not advance main (HEAD=%q)", mainAfterMerge)
	}
	mainTree := readGitObject(repo, mainAfterMerge+"^{tree}")
	finalTree := readGitObject(repo, finalSHA+"^{tree}")
	if mainTree == "" || mainTree != finalTree {
		t.Fatalf("precondition: squash tree=%q, finalized tree=%q", mainTree, finalTree)
	}

	persisted, err := baseStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("reload after failed save: %v", err)
	}
	if persisted.MergeStatus != "" || persisted.MergedInto != "" {
		t.Fatalf("failed metadata save unexpectedly persisted merge: %+v", persisted)
	}
	if err := RecoverFinalize(context.Background(), baseStore, persisted, nil); err != nil {
		t.Fatalf("RecoverFinalize: %v", err)
	}
	if persisted.MergeStatus != store.MergeStatusMerged ||
		persisted.MergedInto != "main" ||
		persisted.MergedCommit != mainAfterMerge {
		t.Fatalf("recovered merge metadata mismatch: %+v", persisted)
	}
	if persisted.FinalCommit != finalSHA || persisted.FinalBranch != "iterion/run/automerge-save-failure" {
		t.Fatalf("recovered finalization metadata mismatch: %+v", persisted)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("recovered worktree still exists: %v", err)
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
	assumeQuiescentProcessCensus(t)
	repo, originalTip := initBareishRepo(t)
	// Filesystem store with a temp root.
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const runID = "run_test_recover_finalize"
	wt := addOwnedWorktree(t, st, repo, runID)
	finalSHA := addCommit(t, wt, "feature.go", "package main\n", "feat: add feature")

	r := &store.Run{
		ID:                runID,
		Name:              "swift-cedar-a3f2",
		Status:            store.RunStatusFinished, // engine got this far
		Worktree:          true,
		WorkDir:           wt,
		RepoRoot:          repo,
		BaseCommit:        originalTip,
		WorktreeCreatedAt: testWorktreeAuthority(),
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
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("recovered worktree still exists after durable branch + metadata save: err=%v", err)
	}
	if registered, err := registeredWorktree(repo, wt); err != nil {
		t.Fatalf("list worktrees after recovery: %v", err)
	} else if registered {
		t.Fatalf("recovered worktree remains registered after cleanup: %s", wt)
	}
}

// TestRecoverFinalize_NoCommitsCleansWorktree covers the zero-result case:
// there is no branch metadata to persist, but a clean worktree still at the
// recorded BaseCommit is safe to remove.
func TestRecoverFinalize_NoCommitsCleansWorktree(t *testing.T) {
	assumeQuiescentProcessCensus(t)
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const runID = "run_recover_no_commits"
	wt := addOwnedWorktree(t, st, repo, runID)
	r := &store.Run{
		ID:                runID,
		Status:            store.RunStatusFinished,
		Worktree:          true,
		WorkDir:           wt,
		RepoRoot:          repo,
		BaseCommit:        originalTip,
		WorktreeCreatedAt: testWorktreeAuthority(),
	}
	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("recover no-commit worktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("no-commit recovered worktree still exists: err=%v", err)
	}
}

// TestRecoverFinalize_BranchFailurePersistsDiagnosticAndWorktree verifies both
// halves of fail-closed recovery: the operator gets the exact branch failure in
// run metadata, and the worktree remains registered as the commit's GC guard.
func TestRecoverFinalize_BranchFailurePersistsDiagnosticAndWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const runID = "run_recover_branch_failure"
	wt := addOwnedWorktree(t, st, repo, runID)
	finalSHA := addCommit(t, wt, "partial.go", "package partial\n", "feat: partial")
	mustRun(t, repo, "git", "branch", "iterion", originalTip)
	r := &store.Run{
		ID:                runID,
		Name:              "blocked-storage-ref",
		Status:            store.RunStatusFinished,
		Worktree:          true,
		WorkDir:           wt,
		RepoRoot:          repo,
		BaseCommit:        originalTip,
		WorktreeCreatedAt: testWorktreeAuthority(),
	}
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("recover branch failure: %v", err)
	}
	if r.FinalCommit != finalSHA || r.FinalBranch != "" || r.FinalBranchError == "" {
		t.Fatalf("recovery diagnostic mismatch: %+v", r)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree was removed despite branch failure: %v", err)
	}
	if registered, err := registeredWorktree(repo, wt); err != nil {
		t.Fatalf("list worktrees: %v", err)
	} else if !registered {
		t.Fatal("worktree no longer registered after storage-branch failure")
	}
	reloaded, err := st.LoadRun(context.Background(), r.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.FinalBranchError == "" {
		t.Fatal("FinalBranchError was not persisted")
	}
}

// TestRecoverFinalize_RefusesForeignRegisteredWorktree proves that appearing in
// `git worktree list` is not sufficient authority. Corrupt metadata for one run
// must never promote, commit, or remove another run's registered worktree.
func TestRecoverFinalize_RefusesForeignRegisteredWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const (
		victimID = "run_recover_ownership_victim"
		otherID  = "run_recover_ownership_other"
	)
	victimWT := addOwnedWorktree(t, st, repo, victimID)
	otherWT := addOwnedWorktree(t, st, repo, otherID)
	otherSHA := addCommit(t, otherWT, "foreign.go", "package foreign\n", "feat: foreign output")

	r := &store.Run{
		ID:         victimID,
		Name:       "ownership-victim",
		Status:     store.RunStatusFinished,
		Worktree:   true,
		WorkDir:    otherWT, // corrupt: points at another run's owned worktree
		RepoRoot:   repo,
		BaseCommit: originalTip,
	}
	err = RecoverFinalize(context.Background(), st, r, nil)
	if err == nil || !strings.Contains(err.Error(), "does not own recovered worktree") {
		t.Fatalf("RecoverFinalize error=%v, want ownership refusal", err)
	}
	if r.FinalCommit != "" || r.FinalBranch != "" || r.MergeStatus != "" {
		t.Fatalf("ownership refusal mutated run: %+v", r)
	}
	if got := readHEAD(otherWT); got != otherSHA {
		t.Fatalf("foreign worktree HEAD changed: got %q, want %q", got, otherSHA)
	}
	for _, path := range []string{victimWT, otherWT} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("ownership refusal removed %s: %v", path, statErr)
		}
	}
	if branches := strings.TrimSpace(string(mustOutput(t, repo, "git", "branch", "--list", "iterion/run/ownership-victim*"))); branches != "" {
		t.Fatalf("ownership refusal created branch(es): %q", branches)
	}
}

func TestFinalizeReconstructedWorktreeRefusesForeignRegisteredWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const (
		victimID = "run_resume_ownership_victim"
		otherID  = "run_resume_ownership_other"
	)
	victimWT := addOwnedWorktree(t, st, repo, victimID)
	otherWT := addOwnedWorktree(t, st, repo, otherID)
	foreignSHA := addCommit(t, otherWT, "foreign-resume.go", "package foreignresume\n", "feat: foreign resume output")

	r, err := st.CreateRun(context.Background(), victimID, "wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r.Status = store.RunStatusFinished
	r.Worktree = true
	r.WorkDir = otherWT // corrupt: points at another run's owned worktree
	r.RepoRoot = repo
	r.BaseCommit = originalTip
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	eng := &Engine{store: st, logger: iterlog.Nop()}
	eng.finalizeReconstructedWorktree(context.Background(), victimID, r, nil)

	if got := readHEAD(otherWT); got != foreignSHA {
		t.Fatalf("foreign worktree HEAD changed: got %q, want %q", got, foreignSHA)
	}
	for _, path := range []string{victimWT, otherWT} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("reconstructed ownership refusal removed %s: %v", path, statErr)
		}
	}
}

func TestRecoverFinalizeRefusesDelegatedGitDirIdentityMismatch(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	victimWT := filepath.Join(t.TempDir(), "delegated-victim")
	otherWT := filepath.Join(t.TempDir(), "delegated-other")
	mustRun(t, repo, "git", "worktree", "add", "--detach", victimWT, "HEAD")
	mustRun(t, repo, "git", "worktree", "add", "--detach", otherWT, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", victimWT).Run() })
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", otherWT).Run() })
	otherSHA := addCommit(t, otherWT, "delegated-foreign.go", "package delegatedforeign\n", "feat: delegated foreign output")
	victimGitDir, err := canonicalExistingPath(resolveWorktreeGitDir(repo, victimWT))
	if err != nil {
		t.Fatalf("canonical victim Git dir: %v", err)
	}

	r := &store.Run{
		ID:                "run_delegated_identity_mismatch",
		Name:              "delegated-identity-mismatch",
		Status:            store.RunStatusFinished,
		Worktree:          true,
		WorktreeOwnership: store.WorktreeOwnershipDelegated,
		WorktreeGitDir:    victimGitDir,
		WorkDir:           otherWT, // corrupt: path and persisted Git identity disagree
		RepoRoot:          repo,
		BaseCommit:        originalTip,
	}
	err = RecoverFinalize(context.Background(), st, r, nil)
	if err == nil || !strings.Contains(err.Error(), "private Git directory changed") {
		t.Fatalf("RecoverFinalize error=%v, want delegated identity refusal", err)
	}
	if got := readHEAD(otherWT); got != otherSHA {
		t.Fatalf("foreign delegated HEAD changed: got %q, want %q", got, otherSHA)
	}
	if _, statErr := os.Stat(otherWT); statErr != nil {
		t.Fatalf("foreign delegated worktree was removed: %v", statErr)
	}
}

func TestCommitUncommittedRefusesForeignRegisteredWorktree(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const (
		victimID = "run_commit_ownership_victim"
		otherID  = "run_commit_ownership_other"
	)
	victimWT := addOwnedWorktree(t, st, repo, victimID)
	otherWT := addOwnedWorktree(t, st, repo, otherID)
	writeFile(t, filepath.Join(otherWT, "foreign-uncommitted.go"), "package foreignuncommitted\n")
	before := readHEAD(otherWT)

	r := &store.Run{
		ID:         victimID,
		Name:       "commit-ownership-victim",
		Status:     store.RunStatusFinished,
		Worktree:   true,
		WorkDir:    otherWT, // corrupt: points at another run's owned worktree
		RepoRoot:   repo,
		BaseCommit: originalTip,
	}
	err = CommitUncommittedAndFinalize(context.Background(), st, r, "feat: must not land", nil)
	if err == nil || !strings.Contains(err.Error(), "does not own recovered worktree") {
		t.Fatalf("CommitUncommittedAndFinalize error=%v, want ownership refusal", err)
	}
	if got := readHEAD(otherWT); got != before {
		t.Fatalf("foreign uncommitted worktree was committed: got HEAD %q, want %q", got, before)
	}
	for _, path := range []string{victimWT, otherWT} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("commit ownership refusal removed %s: %v", path, statErr)
		}
	}
}

// TestRecoverFinalize_SaveFailureKeepsWorktree proves the persistence ordering:
// even after a storage branch was created, recovery must not remove the
// worktree until finalization metadata is durably saved.
func TestRecoverFinalize_SaveFailureKeepsWorktree(t *testing.T) {
	assumeQuiescentProcessCensus(t)
	repo, originalTip := initBareishRepo(t)
	baseStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const runID = "run_recover_save_failure"
	wt := addOwnedWorktree(t, baseStore, repo, runID)
	finalSHA := addCommit(t, wt, "save-failure.go", "package saved\n", "feat: survive metadata failure")
	r := &store.Run{
		ID:                runID,
		Name:              "save-failure",
		Status:            store.RunStatusFinished,
		Worktree:          true,
		WorkDir:           wt,
		RepoRoot:          repo,
		BaseCommit:        originalTip,
		WorktreeCreatedAt: testWorktreeAuthority(),
	}
	err = RecoverFinalize(context.Background(), saveFailRunStore{RunStore: baseStore}, r, nil)
	if err == nil || !strings.Contains(err.Error(), "injected SaveRun failure") {
		t.Fatalf("RecoverFinalize error=%v, want injected SaveRun failure", err)
	}
	if r.FinalCommit != "" || r.FinalBranch != "" || r.MergeStatus != "" {
		t.Fatalf("failed metadata save mutated caller-visible run: %+v", r)
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("worktree removed after SaveRun failure: %v", statErr)
	}
	if registered, regErr := registeredWorktree(repo, wt); regErr != nil {
		t.Fatalf("list worktrees: %v", regErr)
	} else if !registered {
		t.Fatal("worktree unregistered after SaveRun failure")
	}

	// Retry with the exact same pointer. The storage branch created before the
	// failed save must be reused (not suffixed), metadata must become durable,
	// and only then may the worktree be removed.
	if err := RecoverFinalize(context.Background(), baseStore, r, nil); err != nil {
		t.Fatalf("retry RecoverFinalize: %v", err)
	}
	if r.FinalCommit != finalSHA || r.FinalBranch != "iterion/run/save-failure" {
		t.Fatalf("retry finalization metadata mismatch: %+v", r)
	}
	if branches := strings.TrimSpace(string(mustOutput(t, repo, "git", "branch", "--list", "iterion/run/save-failure*"))); branches != "iterion/run/save-failure" {
		t.Fatalf("retry created unexpected suffixed branch: %q", branches)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Fatalf("worktree remains after successful retry: %v", statErr)
	}
}

// TestCleanupRecoveredWorktreeRefusesDirty is the final race/backstop: even
// with a matching HEAD, recovery uses non-forced removal and rechecks status.
func TestCleanupRecoveredWorktreeRefusesDirty(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })
	writeFile(t, filepath.Join(wt, "late-output.txt"), "arrived after finalize\n")

	err := cleanupRecoveredWorktree(repo, wt, "", originalTip, testWorktreeAuthority())
	if err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("cleanup error=%v, want dirty-worktree refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(wt, "late-output.txt")); statErr != nil {
		t.Fatalf("dirty output was removed: %v", statErr)
	}
}

func TestCleanupRecoveredWorktreePreservesIgnoredOutput(t *testing.T) {
	repo, _ := initBareishRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "generated-export.zip\nnode_modules/\n")
	mustRun(t, repo, "git", "add", ".gitignore")
	mustRun(t, repo, "git", "commit", "-m", "chore: ignore generated output")
	expectedHEAD := readHEAD(repo)

	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })
	writeFile(t, filepath.Join(wt, "generated-export.zip"), "only copy\n")

	err := cleanupRecoveredWorktree(repo, wt, "", expectedHEAD, testWorktreeAuthority())
	if err == nil || !strings.Contains(err.Error(), "ignored non-disposable output") {
		t.Fatalf("cleanup error=%v, want ignored-output refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(wt, "generated-export.zip")); statErr != nil {
		t.Fatalf("ignored output was removed: %v", statErr)
	}
}

func TestCleanupRecoveredWorktreeDiscardsKnownDependencyCache(t *testing.T) {
	assumeQuiescentProcessCensus(t)
	repo, _ := initBareishRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "node_modules/\n")
	mustRun(t, repo, "git", "add", ".gitignore")
	mustRun(t, repo, "git", "commit", "-m", "chore: ignore dependencies")
	expectedHEAD := readHEAD(repo)

	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })
	if err := os.MkdirAll(filepath.Join(wt, "node_modules", "example"), 0o755); err != nil {
		t.Fatalf("create dependency cache: %v", err)
	}
	writeFile(t, filepath.Join(wt, "node_modules", "example", "index.js"), "module.exports = true\n")

	if err := cleanupRecoveredWorktree(repo, wt, "", expectedHEAD, testWorktreeAuthority()); err != nil {
		t.Fatalf("cleanup with disposable dependency cache: %v", err)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Fatalf("worktree still exists after disposable-cache cleanup: %v", statErr)
	}
}

func TestCleanupRecoveredWorktreeQuarantinesLateIgnoredWriter(t *testing.T) {
	repo, _ := initBareishRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "generated/\n")
	mustRun(t, repo, "git", "add", ".gitignore")
	mustRun(t, repo, "git", "commit", "-m", "chore: ignore generated output")
	expectedHEAD := readHEAD(repo)

	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")

	// Start a real background process before cleanup. Its cwd is the worktree
	// inode; after the atomic rename, a relative write must land in the
	// recovery copy rather than being deleted or recreating the old path.
	writer := exec.Command(os.Args[0], "-test.run=^TestWorktreeCleanupLateWriterHelper$")
	writer.Env = append(os.Environ(), "GO_WANT_WORKTREE_LATE_WRITER=1")
	writer.Dir = wt
	writerInput, err := writer.StdinPipe()
	if err != nil {
		t.Fatalf("late writer stdin: %v", err)
	}
	if err := writer.Start(); err != nil {
		t.Fatalf("start late writer: %v", err)
	}
	writerWaited := false
	t.Cleanup(func() {
		if writerWaited {
			return
		}
		_ = writer.Process.Kill()
		_ = writer.Wait()
	})

	var writerErr error
	hooks := &worktreeCleanupTestHooks{
		afterRename: func(WorktreeCleanupResult) {
			if _, err := writerInput.Write([]byte{'x'}); err != nil {
				writerErr = err
				return
			}
			if err := writerInput.Close(); err != nil {
				writerErr = err
				return
			}
			writerErr = writer.Wait()
			writerWaited = true
		},
	}
	result, err := cleanupRecoveredWorktreeForRun(
		"run-late-ignored-writer",
		repo,
		wt,
		"",
		expectedHEAD,
		testWorktreeAuthority(),
		hooks,
	)
	if err != nil {
		t.Fatalf("quarantine finalized worktree: %v", err)
	}
	if writerErr != nil {
		t.Fatalf("late writer: %v", writerErr)
	}
	if result.RecoveryPath == "" || result.RecoveryMarker == "" {
		t.Fatalf("cleanup did not expose its recovery copy: %+v", result)
	}
	if !strings.Contains(result.LateWrite, "ignored non-disposable output") {
		t.Fatalf("late-write diagnostic=%q, want ignored-output warning", result.LateWrite)
	}
	if _, err := os.Lstat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical worktree path still exists after retirement: %v", err)
	}
	lateOutput := filepath.Join(result.RecoveryPath, "generated", "late-report.json")
	if got, err := os.ReadFile(lateOutput); err != nil {
		t.Fatalf("late ignored output was not preserved in recovery copy: %v", err)
	} else if string(got) != `{"late":true}` {
		t.Fatalf("late ignored output=%q", got)
	}
	if registered, err := registeredWorktree(repo, result.RecoveryPath); err != nil {
		t.Fatalf("verify recovery registration: %v", err)
	} else if !registered {
		t.Fatalf("recovery copy is not a registered Git worktree: %s", result.RecoveryPath)
	}

	rawMarker, err := os.ReadFile(result.RecoveryMarker)
	if err != nil {
		t.Fatalf("read recovery marker: %v", err)
	}
	var marker worktreeRecoveryManifest
	if err := json.Unmarshal(rawMarker, &marker); err != nil {
		t.Fatalf("decode recovery marker: %v", err)
	}
	if marker.RunID != "run-late-ignored-writer" ||
		marker.OriginalPath != wt ||
		marker.RecoveryPath != result.RecoveryPath ||
		marker.ExpectedHEAD != expectedHEAD {
		t.Fatalf("recovery marker does not identify the retired worktree: %+v", marker)
	}
}

func TestCleanupRecoveredWorktreeRetainsLiveWriterBeforeItWrites(t *testing.T) {
	repo, _ := initBareishRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "generated/\n")
	mustRun(t, repo, "git", "add", ".gitignore")
	mustRun(t, repo, "git", "commit", "-m", "chore: ignore generated output")
	expectedHEAD := readHEAD(repo)

	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")

	// The writer is alive with cwd inside the checkout but has not changed any
	// file when cleanup runs. Cleanliness alone would authorize deletion; the
	// process-reference proof must retain the renamed checkout.
	writer := exec.Command(os.Args[0], "-test.run=^TestWorktreeCleanupLateWriterHelper$")
	writer.Env = append(os.Environ(), "GO_WANT_WORKTREE_LATE_WRITER=1")
	writer.Dir = wt
	writerInput, err := writer.StdinPipe()
	if err != nil {
		t.Fatalf("late writer stdin: %v", err)
	}
	if err := writer.Start(); err != nil {
		t.Fatalf("start late writer: %v", err)
	}
	writerWaited := false
	t.Cleanup(func() {
		if writerWaited {
			return
		}
		_ = writer.Process.Kill()
		_ = writer.Wait()
	})

	result, err := cleanupRecoveredWorktreeForRun(
		"run-live-writer",
		repo,
		wt,
		"",
		expectedHEAD,
		testWorktreeAuthority(),
		nil,
	)
	if err != nil {
		t.Fatalf("quarantine with live writer: %v", err)
	}
	if result.RecoveryPath == "" {
		t.Fatalf("live writer did not retain a recovery worktree: %+v", result)
	}
	if !strings.Contains(result.RetentionReason, "live process references") {
		t.Fatalf("retention reason=%q, want live-process proof", result.RetentionReason)
	}

	if _, err := writerInput.Write([]byte{'x'}); err != nil {
		t.Fatalf("release late writer: %v", err)
	}
	if err := writerInput.Close(); err != nil {
		t.Fatalf("close late writer input: %v", err)
	}
	if err := writer.Wait(); err != nil {
		t.Fatalf("late writer: %v", err)
	}
	writerWaited = true

	output := filepath.Join(result.RecoveryPath, "generated", "late-report.json")
	if got, err := os.ReadFile(output); err != nil {
		t.Fatalf("live writer output was not preserved: %v", err)
	} else if string(got) != `{"late":true}` {
		t.Fatalf("live writer output=%q", got)
	}
}

func TestCleanupRecoveredWorktreePreservesRecreatedAbsolutePath(t *testing.T) {
	repo, expectedHEAD := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")

	var hookErr error
	hooks := &worktreeCleanupTestHooks{
		afterRename: func(WorktreeCleanupResult) {
			if err := os.MkdirAll(wt, 0o755); err != nil {
				hookErr = err
				return
			}
			hookErr = os.WriteFile(filepath.Join(wt, "absolute-late-output.txt"), []byte("preserve me\n"), 0o644)
		},
	}
	result, err := cleanupRecoveredWorktreeForRun(
		"run-absolute-late-writer",
		repo,
		wt,
		"",
		expectedHEAD,
		testWorktreeAuthority(),
		hooks,
	)
	if hookErr != nil {
		t.Fatalf("absolute-path writer fixture: %v", hookErr)
	}
	if err == nil || !strings.Contains(err.Error(), "was recreated during quarantine") {
		t.Fatalf("cleanup error=%v, want recreated-path fail-closed diagnostic", err)
	}
	if result.RecoveryPath == "" {
		t.Fatalf("cleanup did not report the recovery copy after recreation: %+v", result)
	}
	if got, err := os.ReadFile(filepath.Join(wt, "absolute-late-output.txt")); err != nil {
		t.Fatalf("absolute-path output was deleted: %v", err)
	} else if string(got) != "preserve me\n" {
		t.Fatalf("absolute-path output=%q", got)
	}
	if got := readHEAD(result.RecoveryPath); got != expectedHEAD {
		t.Fatalf("recovery HEAD=%q, want %q", got, expectedHEAD)
	}
	if registered, err := registeredWorktree(repo, result.RecoveryPath); err != nil {
		t.Fatalf("verify recovery registration: %v", err)
	} else if !registered {
		t.Fatalf("recovery copy is not registered after absolute-path recreation: %s", result.RecoveryPath)
	}
}

func TestWorktreeCleanupLateWriterHelper(t *testing.T) {
	if os.Getenv("GO_WANT_WORKTREE_LATE_WRITER") != "1" {
		return
	}
	var release [1]byte
	if _, err := os.Stdin.Read(release[:]); err != nil {
		os.Exit(2)
	}
	if err := os.MkdirAll(filepath.Join("generated"), 0o755); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(
		filepath.Join("generated", "late-report.json"),
		[]byte(`{"late":true}`),
		0o644,
	); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestWorktreeCleanupLocksBlockLateCommit(t *testing.T) {
	repo, _ := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", "-b", "cleanup-lock-topic", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })
	before := readHEAD(wt)

	unlock, err := acquireWorktreeCleanupLocks(repo, wt)
	if err != nil {
		t.Fatalf("acquire cleanup locks: %v", err)
	}
	defer unlock()
	cmd := exec.Command("git", "-C", wt, "commit", "--allow-empty", "-m", "late commit")
	if out, commitErr := cmd.CombinedOutput(); commitErr == nil {
		t.Fatalf("late commit succeeded while cleanup locks were held: %s", out)
	}
	if got := readHEAD(wt); got != before {
		t.Fatalf("worktree HEAD moved under cleanup locks: got %q, want %q", got, before)
	}
}

func TestCleanupRecoveredWorktreeRemovesAttachedFinalBranch(t *testing.T) {
	assumeQuiescentProcessCensus(t)
	repo, _ := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	const branch = "nested/attached-cleanup"
	mustRun(t, repo, "git", "worktree", "add", "-b", branch, wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })
	finalSHA := addCommit(t, wt, "attached.go", "package attached\n", "feat: attached output")
	mustRun(t, repo, "git", "pack-refs", "--all", "--prune")
	looseRef := filepath.Join(repo, ".git", "refs", "heads", "nested", "attached-cleanup")
	if _, statErr := os.Stat(looseRef); !os.IsNotExist(statErr) {
		t.Fatalf("attached branch was not packed for the regression fixture: %v", statErr)
	}

	if err := cleanupRecoveredWorktree(repo, wt, branch, finalSHA, testWorktreeAuthority()); err != nil {
		t.Fatalf("cleanup attached finalized worktree: %v", err)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Fatalf("attached worktree still exists after cleanup: %v", statErr)
	}
	guards := strings.TrimSpace(string(mustOutput(t, repo, "git", "for-each-ref", "--format=%(refname)", "refs/iterion/cleanup-guards/")))
	if guards != "" {
		t.Fatalf("cleanup guard leaked after attached cleanup: %q", guards)
	}
}

func TestCleanupGuardRetainedWhenStorageBranchMoves(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	mustRun(t, repo, "git", "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", wt).Run() })
	finalSHA := addCommit(t, wt, "guarded.go", "package guarded\n", "feat: guarded")
	const branch = "iterion/run/guard-race"
	mustRun(t, repo, "git", "branch", branch, finalSHA)

	guardRef, err := createCleanupGuard(repo, finalSHA)
	if err != nil {
		t.Fatalf("create guard: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "update-ref", "-d", guardRef).Run() })
	mustRun(t, repo, "git", "branch", "-f", branch, originalTip)
	if err := releaseCleanupGuard(repo, guardRef, branch, finalSHA); err == nil {
		t.Fatal("cleanup guard was released after storage branch moved")
	}
	if got := readGitObject(repo, guardRef+"^{commit}"); got != finalSHA {
		t.Fatalf("retained cleanup guard=%q, want %q", got, finalSHA)
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

// TestRecoverFinalize_AlreadyFinalizedCleansLeftoverWorktree simulates a crash
// after final metadata was saved but before cleanup. Reconciliation must not
// create a suffixed branch; it only verifies the persisted branch/commit and
// removes the clean leftover worktree.
func TestRecoverFinalize_AlreadyFinalizedCleansLeftoverWorktree(t *testing.T) {
	assumeQuiescentProcessCensus(t)
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const runID = "run_already_finalized_with_worktree"
	wt := addOwnedWorktree(t, st, repo, runID)
	finalSHA := addCommit(t, wt, "done.go", "package done\n", "feat: finalized")
	const finalBranch = "iterion/run/already-finalized"
	mustRun(t, repo, "git", "branch", finalBranch, finalSHA)

	r := &store.Run{
		ID:                runID,
		Status:            store.RunStatusFinished,
		Worktree:          true,
		WorkDir:           wt,
		RepoRoot:          repo,
		BaseCommit:        originalTip,
		FinalCommit:       finalSHA,
		FinalBranch:       finalBranch,
		WorktreeCreatedAt: testWorktreeAuthority(),
	}
	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("cleanup already-finalized worktree: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("already-finalized worktree still exists: err=%v", err)
	}
	branches := strings.TrimSpace(string(mustOutput(t, repo, "git", "branch", "--list", finalBranch+"*")))
	if strings.Count(branches, finalBranch) != 1 {
		t.Fatalf("recovery created an unexpected suffixed branch: %q", branches)
	}
}

func TestRecoverFinalize_LegacyRunWithoutTrustedCreationTimeIsPreserved(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const runID = "run_legacy_without_creation_time"
	wt := addOwnedWorktree(t, st, repo, runID)
	finalSHA := addCommit(t, wt, "legacy.go", "package legacy\n", "feat: legacy output")

	r := &store.Run{
		ID:         runID,
		Name:       "legacy-without-authority",
		Status:     store.RunStatusFinished,
		Worktree:   true,
		WorkDir:    wt,
		RepoRoot:   repo,
		BaseCommit: originalTip,
		// WorktreeCreatedAt is deliberately empty: old persisted runs do not
		// have a trustworthy process-census boundary.
	}
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("recover legacy worktree: %v", err)
	}
	if r.FinalCommit != finalSHA || r.FinalBranch == "" {
		t.Fatalf("legacy finalization metadata mismatch: %+v", r)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("legacy worktree was removed without trusted creation time: %v", err)
	}
	if registered, err := registeredWorktree(repo, wt); err != nil {
		t.Fatalf("list legacy worktrees: %v", err)
	} else if !registered {
		t.Fatal("legacy worktree no longer registered")
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

// TestRecoverFinalize_SkipsFailedResumable — resumable runs keep their
// worktree and checkpoint for a future resume. Pre-finalizing there would
// create a stale storage branch before the resumed run reaches its real tip.
func TestRecoverFinalize_SkipsFailedResumable(t *testing.T) {
	st, _ := store.New(t.TempDir())
	r := &store.Run{
		ID:       "run_failed_resumable",
		Status:   store.RunStatusFailedResumable,
		Worktree: true,
		WorkDir:  "/tmp/wt",
		RepoRoot: "/tmp/repo",
	}
	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("failed_resumable path errored: %v", err)
	}
	if r.FinalCommit != "" || r.FinalBranch != "" {
		t.Errorf("failed_resumable path mutated state: %+v", r)
	}
}

// TestRecoverFinalize_CancelledRun — a run the operator cancelled with
// commits in the worktree MUST be finalized so the merge UI can act on
// the partial work. Without this, "Squash and merge" fails with "no
// storage branch" and the operator has to recover by hand.
func TestRecoverFinalize_CancelledRun(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const runID = "run_cancelled_partial"
	wt := addOwnedWorktree(t, st, repo, runID)
	finalSHA := addCommit(t, wt, "partial.go", "package main\n", "feat: partial work")
	r := &store.Run{
		ID:         runID,
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
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("cancelled run worktree should remain inspectable: %v", err)
	}
}

// TestRecoverFinalize_FailedRun keeps the implementation and regression suite
// aligned: hard-failed runs are terminal and their partial commits are promoted
// for inspection, while failed_resumable runs above remain untouched.
func TestRecoverFinalize_FailedRun(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store new: %v", err)
	}
	const runID = "run_failed_partial"
	wt := addOwnedWorktree(t, st, repo, runID)
	finalSHA := addCommit(t, wt, "failed.go", "package failed\n", "wip: failed run output")
	r := &store.Run{
		ID:         runID,
		Name:       "failed-partial",
		Status:     store.RunStatusFailed,
		Worktree:   true,
		WorkDir:    wt,
		RepoRoot:   repo,
		BaseCommit: originalTip,
	}
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	if err := RecoverFinalize(context.Background(), st, r, nil); err != nil {
		t.Fatalf("recover failed run: %v", err)
	}
	if r.FinalCommit != finalSHA || r.FinalBranch != "iterion/run/failed-partial" {
		t.Fatalf("failed run recovery mismatch: %+v", r)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("failed run worktree should remain inspectable: %v", err)
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
