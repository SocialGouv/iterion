package runview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tgit runs one git command in dir and fails the test on error.
func tgit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@test.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@test.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedForge builds a bare "forge" repo with a trunk branch (one base
// commit) and a run storage branch carrying one commit on top. Returns
// the bare URL, the storage branch name and its tip sha.
func seedForge(t *testing.T) (bareURL, storageBranch, storageSHA string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "forge.git")
	tgit(t, root, "init", "--bare", "--initial-branch=trunk", bare)
	work := filepath.Join(root, "seed")
	tgit(t, root, "clone", bare, work)
	tgit(t, work, "checkout", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(work, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, work, "add", "base.txt")
	tgit(t, work, "commit", "-m", "base")
	tgit(t, work, "push", "origin", "trunk")

	storageBranch = "iterion/run-test"
	tgit(t, work, "checkout", "-b", storageBranch)
	if err := os.WriteFile(filepath.Join(work, "work.txt"), []byte("run work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgit(t, work, "add", "work.txt")
	tgit(t, work, "commit", "-m", "run work")
	tgit(t, work, "push", "origin", storageBranch)
	storageSHA = tgit(t, work, "rev-parse", "HEAD")
	return "file://" + bare, storageBranch, storageSHA
}

// seedRepoTargetedRun persists a finished repo-targeted run pointing at
// the seeded forge, the way the cloud publisher + runner banking leave it.
func seedRepoTargetedRun(t *testing.T, dir, repoURL, storageBranch, storageSHA string) (*Service, string) {
	t.Helper()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	runID := "run-remote-merge"
	if _, err := st.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	r.Status = store.RunStatusFinished
	r.RepoURL = repoURL
	r.RepoSHA = "trunk"
	r.FinalBranch = storageBranch
	r.FinalCommit = storageSHA
	if err := st.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, runID
}

// TestPerformMerge_RepoTargeted_PushesToForge is the happy path: a
// finished repo-targeted run (no local workspace at all) merges into its
// launch ref and the FORGE trunk advances — the merge only counts once
// it is on the remote.
func TestPerformMerge_RepoTargeted_PushesToForge(t *testing.T) {
	bareURL, storageBranch, storageSHA := seedForge(t)
	dir := t.TempDir()
	svc, runID := seedRepoTargetedRun(t, dir, bareURL, storageBranch, storageSHA)

	res, err := svc.PerformMergeCtx(context.Background(), runID, MergeRequest{Strategy: store.MergeStrategyMerge})
	if err != nil {
		t.Fatalf("PerformMergeCtx: %v", err)
	}
	if res.MergeStatus != store.MergeStatusMerged {
		t.Fatalf("merge status = %q, want merged", res.MergeStatus)
	}
	if res.MergedInto != "trunk" {
		t.Fatalf("merged into %q, want trunk (the launch ref)", res.MergedInto)
	}

	// The forge's trunk must now contain the storage commit.
	bare := strings.TrimPrefix(bareURL, "file://")
	if out := tgit(t, bare, "merge-base", "--is-ancestor", storageSHA, "trunk"); out != "" {
		t.Fatalf("unexpected merge-base output: %s", out)
	}
	// The disposable server-side clone is gone after success.
	if svc.hasRepoTargetedMergeRoot(runID) {
		t.Fatal("merge clone survived a successful merge — it must be removed")
	}
}

// TestPerformMerge_RepoTargeted_PushRefusedIsLoud proves the failure
// direction: when the forge refuses the push, the merge is NOT reported
// as merged, the error names the push, and the run records the failure.
func TestPerformMerge_RepoTargeted_PushRefusedIsLoud(t *testing.T) {
	bareURL, storageBranch, storageSHA := seedForge(t)
	bare := strings.TrimPrefix(bareURL, "file://")
	hook := filepath.Join(bare, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho refused-by-hook >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A developer machine may point core.hooksPath elsewhere globally
	// (e.g. lefthook), which would silently skip this bare's hooks and
	// let the push through — pin the bare to its own hooks dir.
	tgit(t, bare, "config", "core.hooksPath", "hooks")
	dir := t.TempDir()
	svc, runID := seedRepoTargetedRun(t, dir, bareURL, storageBranch, storageSHA)

	_, err := svc.PerformMergeCtx(context.Background(), runID, MergeRequest{Strategy: store.MergeStrategyMerge})
	if err == nil {
		t.Fatal("merge reported success while the forge refused the push")
	}
	if !strings.Contains(err.Error(), "push failed") {
		t.Fatalf("error does not name the push: %v", err)
	}
	r, lerr := svc.store.LoadRun(context.Background(), runID)
	if lerr != nil {
		t.Fatalf("reload run: %v", lerr)
	}
	if r.MergeStatus != store.MergeStatusFailed {
		t.Fatalf("merge status = %q, want failed", r.MergeStatus)
	}
}

// TestPerformMerge_RepoTargeted_NoRefNoTarget refuses loudly when the
// run records no launch ref and the caller names no target — guessing a
// branch to merge into is how work lands in the wrong place.
func TestPerformMerge_RepoTargeted_NoRefNoTarget(t *testing.T) {
	bareURL, storageBranch, storageSHA := seedForge(t)
	dir := t.TempDir()
	svc, runID := seedRepoTargetedRun(t, dir, bareURL, storageBranch, storageSHA)
	r, err := svc.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	r.RepoSHA = ""
	if err := svc.store.SaveRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}

	_, err = svc.PerformMergeCtx(context.Background(), runID, MergeRequest{Strategy: store.MergeStrategyMerge})
	if err == nil {
		t.Fatal("merge picked a target on its own")
	}
	if !strings.Contains(err.Error(), "--into") {
		t.Fatalf("error does not point at the explicit-target escape hatch: %v", err)
	}
}
