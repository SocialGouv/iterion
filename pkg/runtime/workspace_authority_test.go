package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gitOut runs a git command in dir, failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gittest.Run(t, dir, args...)
}

// TestRunPersistWorkspace_WorkspaceAuthority pins the managed-worktree
// promotion gate: a linked-worktree workspace is adopted as a managed
// baseline (Worktree=true → finalization authority: iterion/run/*
// branch + best-effort FF + cleanup on close) ONLY when the workspace
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
		name         string
		delegated    bool
		wantWorktree bool
	}{
		{"foreign linked worktree (defaulted CWD) is NOT promoted", false, false},
		{"delegated linked worktree (WithWorkDir) IS promoted", true, true},
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
			run.ExecutionContext = &store.ExecutionContext{
				RunStore: store.ContextRef{ID: "run-store", Kind: "filesystem"},
				Workspace: store.WorkspaceContext{
					Mode:        store.WorkspaceShared,
					WorkspaceID: "caller-declared",
				},
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
			if tc.wantWorktree && got.RepoRoot == "" {
				t.Error("promoted run must carry the main repo root as its baseline")
			}
			if got.ExecutionContext == nil {
				t.Fatal("execution context was not persisted")
			}
			wantMode := store.WorkspaceInherited
			wantWorkspaceID := store.StableContextID("workspace", linked)
			if tc.wantWorktree {
				wantMode = store.WorkspaceIsolated
				wantWorkspaceID = runID
			}
			if got.ExecutionContext.Workspace.Mode != wantMode {
				t.Errorf("execution context workspace mode = %q, want %q", got.ExecutionContext.Workspace.Mode, wantMode)
			}
			if got.ExecutionContext.Workspace.WorkspaceID != wantWorkspaceID {
				t.Errorf("execution context workspace id = %q, want %q", got.ExecutionContext.Workspace.WorkspaceID, wantWorkspaceID)
			}
		})
	}
}
