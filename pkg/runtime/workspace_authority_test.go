package runtime

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gitOut runs a git command in dir, failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// TestRunPersistWorkspace_WorkspaceAuthority pins the managed-worktree
// promotion gate: a linked-worktree workspace is adopted as a managed
// baseline (Worktree=true → Git finalization authority: iterion/run/*
// branch + best-effort FF; teardown stays with the launcher) ONLY when the workspace
// was delegated to the engine via WithWorkDir (dispatcher-seeded
// per-issue worktrees, studio-bound dirs). A defaulted-CWD run from
// inside a FOREIGN linked worktree — a Claude Code session worktree,
// an operator's manual `git worktree add` — is the operator's own
// place: the engine must not claim lifecycle authority over it
// (observed live: a `worktree: none` bot launched from a Claude
// worktree got stamped Worktree=true, queueing a close-time FF of the
// operator's checked-out branch onto the session worktree's HEAD).
func TestRunPersistWorkspace_WorkspaceAuthority(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	gitOut(t, root, "init", "-q", "-b", "main", main)
	gitOut(t, main, "commit", "--allow-empty", "-q", "-m", "seed")
	linked := filepath.Join(root, "linked-wt")
	gitOut(t, main, "worktree", "add", "-q", linked)

	wf := &ir.Workflow{Name: "authority", Nodes: map[string]ir.Node{}}

	cases := []struct {
		name          string
		delegated     bool
		wantWorktree  bool
		wantOwnership store.WorktreeOwnership
	}{
		{"foreign linked worktree (defaulted CWD) is NOT promoted", false, false, ""},
		{"delegated linked worktree (WithWorkDir) IS promoted", true, true, store.WorktreeOwnershipDelegated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			runID := "authority-" + map[bool]string{true: "delegated", false: "foreign"}[tc.delegated]
			run, err := s.CreateRun(context.Background(), runID, wf.Name, nil)
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}

			var eng *Engine
			if tc.delegated {
				eng = New(wf, s, nil, WithWorkDir(linked))
			} else {
				eng = New(wf, s, nil)
				eng.workDir = linked // simulate the defaulted-CWD path
			}
			if err := eng.runPersistWorkspace(context.Background(), runID, run, false, worktreeContext{}); err != nil {
				t.Fatalf("runPersistWorkspace: %v", err)
			}

			got, err := s.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if got.Worktree != tc.wantWorktree {
				t.Errorf("Worktree = %v, want %v (repo_root=%q base=%q)",
					got.Worktree, tc.wantWorktree, got.RepoRoot, got.BaseCommit)
			}
			if got.WorktreeOwnership != tc.wantOwnership {
				t.Errorf("WorktreeOwnership = %q, want %q", got.WorktreeOwnership, tc.wantOwnership)
			}
			if tc.wantWorktree && got.RepoRoot == "" {
				t.Error("promoted run must carry the main repo root as its baseline")
			}
			if tc.wantWorktree && got.WorktreeGitDir == "" {
				t.Error("promoted run must bind its private Git directory")
			}
		})
	}
}
