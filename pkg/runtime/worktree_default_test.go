package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

type noGitWorktreeStore struct {
	store.RunStore
}

func (s noGitWorktreeStore) Root() string { return "" }

func (s noGitWorktreeStore) Capabilities() store.Capabilities {
	caps := s.RunStore.Capabilities()
	caps.GitWorktree = false
	return caps
}

// TestWorkspaceIsGitRepo_TempDirIsNotARepo locks the predicate the engine
// uses to gate worktree setup: a freshly-created temp directory must NOT
// be reported as a git repo. Without this guard the new IR default of
// `worktree: auto` would hard-fail every CLI run against a scratch
// folder + every e2e test that runs out of t.TempDir().
func TestWorkspaceIsGitRepo_TempDirIsNotARepo(t *testing.T) {
	dir := t.TempDir()
	if workspaceIsGitRepo(dir) {
		t.Fatalf("workspaceIsGitRepo(%q) = true, want false (no .git anywhere)", dir)
	}
}

// TestWorkspaceIsGitRepo_InitializedRepoIsARepo confirms the predicate
// actually detects real repos so the engine still enters the worktree
// branch when isolation is feasible.
func TestWorkspaceIsGitRepo_InitializedRepoIsARepo(t *testing.T) {
	repo, _ := initBareishRepo(t)
	if !workspaceIsGitRepo(repo) {
		t.Fatalf("workspaceIsGitRepo(%q) = false, want true (just `git init`-ed)", repo)
	}
	// And a subdirectory inside the repo still reports true (parent walk).
	sub := filepath.Join(repo, "nested", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if !workspaceIsGitRepo(sub) {
		t.Fatalf("workspaceIsGitRepo(%q) = false, want true (parent has .git)", sub)
	}
}

func TestEngineRun_PersistsLauncherWorktreeAuthorityBoundary(t *testing.T) {
	wf := &ir.Workflow{
		Name:     "wt-launch-authority",
		Entry:    "fail",
		Nodes:    map[string]ir.Node{"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}}},
		Worktree: "auto",
	}
	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	authoritySince := time.Now().Add(-time.Second).UTC()

	const runID = "run-launch-authority"
	eng := New(
		wf,
		s,
		newStubExecutor(),
		WithWorkDir(repo),
		WithWorktreeAuthoritySince(authoritySince),
	)
	if err := eng.Run(context.Background(), runID, nil); err == nil {
		t.Fatal("Engine.Run reached fail node without an error")
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if !r.WorktreeCreatedAt.Equal(authoritySince) {
		t.Fatalf(
			"WorktreeCreatedAt=%s, want launcher boundary %s",
			r.WorktreeCreatedAt.Format(time.RFC3339Nano),
			authoritySince.Format(time.RFC3339Nano),
		)
	}
	if _, err := os.Stat(r.WorkDir); err != nil {
		t.Fatalf("failed run's worktree was not preserved: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", r.WorkDir).Run()
	})
}

// TestEngineRun_AutoWorktreeDegradesOnNonGit drives the full engine path
// for a workflow that defaults to `worktree: auto` against a non-git
// workspace and asserts:
//   - the run succeeds (no "not a git repository" error),
//   - the run's WorkDir stays at the operator-supplied path (no
//     phantom worktree dir under storeRoot/worktrees/),
//   - run.Worktree is false (the run record honestly reflects in-place),
//   - no `worktrees/<runID>/` directory was created under the store root.
//
// This is the contract the IR default of `worktree: auto` relies on:
// degradation, never failure, when isolation is impossible.
func TestEngineRun_AutoWorktreeDegradesOnNonGit(t *testing.T) {
	// Workflow with no DSL declarations: the runtime takes the
	// "auto" branch since e.workflow.Worktree == "auto" matches.
	// We construct directly so we don't have to round-trip via the
	// parser — the IR default is exercised separately in pkg/dsl/ir.
	wf := &ir.Workflow{
		Name:  "wt-default-noop",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "start"},
				Command:  "true",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "start", To: "done"},
		},
		Worktree: "auto",
	}

	s := tmpStore(t)
	// A temp dir that is intentionally NOT a git repo — the operator's
	// "I just want to run this thing" scenario.
	workDir := t.TempDir()

	eng := New(wf, s, newStubExecutor(),
		WithWorkDir(workDir),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)

	runID := "run-wt-degrade"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run on non-git workspace returned error: %v (must degrade gracefully)", err)
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("run status = %q, want %q", r.Status, store.RunStatusFinished)
	}
	if r.Worktree {
		t.Errorf("run.Worktree = true, want false (degraded to in-place)")
	}
	if r.WorkDir != workDir {
		t.Errorf("run.WorkDir = %q, want operator-supplied %q (no phantom worktree dir)", r.WorkDir, workDir)
	}

	// And no worktree directory should have been created on disk.
	wtDir := filepath.Join(s.Root(), "worktrees", runID)
	if info, err := os.Stat(wtDir); err == nil {
		t.Errorf("unexpected worktree directory created at %s (mode %s) — engine should have skipped setup", wtDir, info.Mode())
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v (want IsNotExist)", wtDir, err)
	}
}

func TestEngineRun_AutoWorktreeRespectsStoreCapability(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "wt-cloud-capability",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "start"},
				Command:  "true",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:    []*ir.Edge{{From: "start", To: "done"}},
		Worktree: "auto",
	}
	base := tmpStore(t)
	cloudLike := noGitWorktreeStore{RunStore: base}
	repo, _ := initBareishRepo(t)
	eng := New(wf, cloudLike, newStubExecutor(), WithWorkDir(repo))

	const runID = "run-no-git-worktree-capability"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	r, err := base.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Worktree {
		t.Fatalf("run.Worktree=true for a store with GitWorktree=false")
	}
	if r.WorkDir != repo {
		t.Fatalf("run.WorkDir=%q, want launcher clone %q", r.WorkDir, repo)
	}
	if _, err := os.Stat(filepath.Join("worktrees", runID)); !os.IsNotExist(err) {
		t.Fatalf("relative nested worktree was created despite store capability: %v", err)
	}
}

// TestEngineRun_ExplicitNoneSkipsWorktreeOnGitRepo confirms `worktree: none`
// is honored even inside a real git repository: an operator who explicitly
// opts out gets in-place execution, no worktree, no finalize-merge guard
// chain. This is the documented escape hatch for tests/fixtures that
// genuinely need in-place behaviour (the campaign explicitly calls out
// this case for review_test fixtures running inside git temp repos).
func TestEngineRun_ExplicitNoneSkipsWorktreeOnGitRepo(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "wt-explicit-none",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "start"},
				Command:  "true",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:    []*ir.Edge{{From: "start", To: "done"}},
		Worktree: "none",
	}

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)

	eng := New(wf, s, newStubExecutor(),
		WithWorkDir(repo),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)

	runID := "run-wt-none"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run with explicit none returned error: %v", err)
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	// Explicit opt-out from the operator's main checkout must stay a plain
	// in-place run. In particular, do not stamp Worktree=true just because
	// git commands succeed here: resume/review-gate paths use that flag to
	// reconstruct finalization and would otherwise operate on the user's
	// checkout as if it were a runtime-managed worktree.
	if r.Worktree {
		t.Errorf("run.Worktree = true, want false for explicit worktree:none in main checkout")
	}
	if r.RepoRoot != "" {
		t.Errorf("run.RepoRoot = %q, want empty for non-isolated main checkout", r.RepoRoot)
	}
	if r.BaseCommit != "" {
		t.Errorf("run.BaseCommit = %q, want empty for non-isolated main checkout", r.BaseCommit)
	}
	if r.WorkDir != repo {
		t.Errorf("run.WorkDir = %q, want exactly %q (engine must not have remapped to a worktree path)", r.WorkDir, repo)
	}
	wtDir := filepath.Join(s.Root(), "worktrees", runID)
	if _, err := os.Stat(wtDir); err == nil {
		t.Errorf("unexpected worktree directory at %s — explicit none must skip setup", wtDir)
	}
}

func TestEngineRun_DelegatedLinkedWorktreeFinalizesAndPreservesForLauncher(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "wt-linked-dispatcher",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{
				BaseNode: ir.BaseNode{ID: "start"},
				Command:  "true",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:    []*ir.Edge{{From: "start", To: "done"}},
		Worktree: "none",
	}

	s := tmpStore(t)
	repo, originalTip := initBareishRepo(t)
	linked := filepath.Join(t.TempDir(), "linked-wt")
	mustRun(t, repo, "git", "worktree", "add", "--detach", linked, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", linked).Run() })

	executor := newStubExecutor()
	var producedSHA string
	executor.handlers["start"] = func(map[string]any) (map[string]any, error) {
		producedSHA = addCommit(t, linked, "delegated.go", "package delegated\n", "feat: delegated output")
		return map[string]any{}, nil
	}
	eng := New(wf, s, executor,
		WithWorkDir(linked),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)

	runID := "run-linked-wt-baseline"
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("engine.Run in unmanaged linked worktree returned error: %v", err)
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if !r.Worktree {
		t.Fatalf("run.Worktree = false, want true for an already-isolated linked worktree")
	}
	if r.WorktreeOwnership != store.WorktreeOwnershipDelegated {
		t.Errorf("run.WorktreeOwnership = %q, want delegated", r.WorktreeOwnership)
	}
	if r.WorktreeGitDir == "" {
		t.Error("run.WorktreeGitDir is empty for delegated worktree")
	}
	if r.WorkDir != linked {
		t.Errorf("run.WorkDir = %q, want linked worktree %q", r.WorkDir, linked)
	}
	if r.RepoRoot != repo {
		t.Errorf("run.RepoRoot = %q, want main repo %q", r.RepoRoot, repo)
	}
	if r.BaseCommit != originalTip {
		t.Errorf("run.BaseCommit = %q, want linked worktree HEAD %q", r.BaseCommit, originalTip)
	}
	if producedSHA == "" || r.FinalCommit != producedSHA {
		t.Errorf("run.FinalCommit = %q, want produced SHA %q", r.FinalCommit, producedSHA)
	}
	if r.FinalBranch == "" {
		t.Error("run.FinalBranch is empty after delegated finalization")
	} else if got, err := readBranchCommit(repo, r.FinalBranch); err != nil || got != producedSHA {
		t.Errorf("delegated storage branch = %q err=%v, want %q", got, err, producedSHA)
	}
	if _, err := os.Stat(linked); err != nil {
		t.Errorf("delegated linked worktree was removed before launcher hooks/policy: %v", err)
	}
	if err := RecoverFinalize(context.Background(), s, r, nil); err != nil {
		t.Fatalf("RecoverFinalize delegated finalized run: %v", err)
	}
	if _, err := os.Stat(linked); err != nil {
		t.Errorf("reconciliation removed launcher-owned delegated worktree: %v", err)
	}
}
