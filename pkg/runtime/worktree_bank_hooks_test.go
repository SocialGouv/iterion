package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestFinalizeWorktree_WipBankRunsNoRepositoryHook — the wip bank commits a
// run's own output in the run's tree, and the hooks directory is one the run
// can write: a hook there is the run's code, executed inside the landing
// gesture after every verdict the run's bots took. The bank runs none — not
// one that rewrites what is banked, not one that refuses the bank, not one
// that commits again on top of it.
func TestFinalizeWorktree_WipBankRunsNoRepositoryHook(t *testing.T) {
	for _, tc := range []struct {
		name, hook, body string
	}{
		{"a pre-commit hook rewriting what is banked", "pre-commit",
			"#!/bin/sh\nprintf 'rewritten by a hook\\n' > owner.txt\ngit add owner.txt\n"},
		{"a pre-commit hook refusing the bank", "pre-commit",
			"#!/bin/sh\nexit 1\n"},
		{"a commit-msg hook refusing the bank", "commit-msg",
			"#!/bin/sh\nexit 1\n"},
		{"a post-commit hook committing again on top of the bank", "post-commit",
			"#!/bin/sh\nprintf 'rewritten after the bank\\n' > owner.txt\ngit commit -qam 'after the bank'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, originalTip := initBareishRepo(t)
			wt := filepath.Join(t.TempDir(), "wt")
			gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
			t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

			runCommit := addCommit(t, wt, "owner.txt", "the owner's\n", "the run's own commit")
			hooks := gittest.Run(t, wt, "rev-parse", "--git-path", "hooks")
			if !filepath.IsAbs(hooks) {
				hooks = filepath.Join(wt, hooks)
			}
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hooks, tc.hook), []byte(tc.body), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(wt, "product.txt"), "the run's uncommitted output\n")

			res := finalizeWorktree(worktreeContext{
				repoRoot:       repo,
				wtPath:         wt,
				originalBranch: "main",
				originalTip:    originalTip,
			}, finalizeOptions{runID: "run_hooks", autoMerge: false, mergeStrategy: "merge"}, nil)

			if !res.WipBanked || res.PreserveWorktree || res.FinalCommit == "" {
				t.Fatalf("the bank did not go through: wip_banked=%v preserve=%v final=%q", res.WipBanked, res.PreserveWorktree, res.FinalCommit)
			}
			if subject := gittest.Run(t, repo, "log", "-1", "--format=%s", res.FinalCommit); !strings.HasPrefix(subject, "wip(iterion): auto-banked") {
				t.Fatalf("the banked head is %q, not the bank's own commit: a hook moved it", subject)
			}
			if parent := gittest.Run(t, repo, "rev-parse", res.FinalCommit+"^"); parent != runCommit {
				t.Fatalf("the bank's parent is %s, not the run's commit %s", parent, runCommit)
			}
			if owner := gittest.Run(t, repo, "show", res.FinalCommit+":owner.txt"); owner != "the owner's" {
				t.Fatalf("a hook rewrote what the bank carries: owner.txt = %q", owner)
			}
			if product := gittest.Run(t, repo, "show", res.FinalCommit+":product.txt"); product != "the run's uncommitted output" {
				t.Fatalf("the bank did not carry the run's output: product.txt = %q", product)
			}
		})
	}
}

// TestCommitUncommittedAndFinalize_RunsNoRepositoryHook — the salvage commit
// of a run's uncommitted output is the same gesture as the bank, in the same
// tree: no hook of the run's runs inside it.
func TestCommitUncommittedAndFinalize_RunsNoRepositoryHook(t *testing.T) {
	for _, tc := range []struct{ name, hook, body string }{
		{"a pre-commit hook rewriting what is committed", "pre-commit",
			"#!/bin/sh\nprintf 'rewritten by a hook\\n' > owner.txt\ngit add owner.txt\n"},
		{"a pre-commit hook refusing the commit", "pre-commit", "#!/bin/sh\nexit 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, originalTip := initBareishRepo(t)
			wt := filepath.Join(t.TempDir(), "wt")
			gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
			t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })
			addCommit(t, wt, "owner.txt", "the owner's\n", "the run's own commit")
			hooks := gittest.Run(t, wt, "rev-parse", "--git-path", "hooks")
			if !filepath.IsAbs(hooks) {
				hooks = filepath.Join(wt, hooks)
			}
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hooks, tc.hook), []byte(tc.body), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(wt, "product.txt"), "the run's uncommitted output\n")

			ctx := context.Background()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			r, err := st.CreateRun(ctx, "run_salvage", "wf", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Worktree, r.WorkDir, r.RepoRoot, r.BaseCommit, r.Status = true, wt, repo, originalTip, store.RunStatusFinished
			if err := st.SaveRun(ctx, r); err != nil {
				t.Fatal(err)
			}
			if err := CommitUncommittedAndFinalize(ctx, st, r, "salvage the run's output", nil); err != nil {
				t.Fatalf("the salvage commit failed: %v", err)
			}
			if r.FinalCommit == "" {
				t.Fatalf("no final commit recorded: %+v", r)
			}
			if owner := gittest.Run(t, repo, "show", r.FinalCommit+":owner.txt"); owner != "the owner's" {
				t.Fatalf("a hook rewrote what the salvage commit carries: owner.txt = %q", owner)
			}
		})
	}
}
