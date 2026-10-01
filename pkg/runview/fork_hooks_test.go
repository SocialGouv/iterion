package runview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A fork checks the child's worktree out of the parent's repository — a
// run's, whose hooks directory the run can write. A post-checkout hook there
// would rewrite the child's starting tree as it is checked out.
func TestForkWorktreeRunsNoRepositoryHook(t *testing.T) {
	repo := t.TempDir()
	gittest.InitRepo(t, repo)
	const plan = "version: 1\nlots:\n  - id: L1\n    status: todo\n"
	if err := os.MkdirAll(filepath.Join(repo, ".modernize"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".modernize", "plan.yaml"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, repo, "add", "-A")
	gittest.Run(t, repo, "commit", "-qm", "the contract")
	hooks := filepath.Join(repo, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"),
		[]byte("#!/bin/sh\nprintf 'forged\\n' > .modernize/plan.yaml\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	parent := &store.Run{ID: "run_parent", RepoRoot: repo, Worktree: true}
	wt, err := forkWorktree(parent, ForkSpec{RunID: parent.ID, NodeID: "n"}, nil, t.TempDir(), "run_child")
	if err != nil {
		t.Fatalf("forkWorktree: %v", err)
	}
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })
	got, err := os.ReadFile(filepath.Join(wt, ".modernize", "plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != plan {
		t.Fatalf("a hook of the parent's repository rewrote the child's tree as it was checked out: %q", got)
	}
}
