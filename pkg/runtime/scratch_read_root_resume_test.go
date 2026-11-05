package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
)

// TestResume_NonWorktreeSubdirKeepsLaunchScratchDir is the #2867 witness:
// a run launched with `worktree: none` from a SUBDIRECTORY of a git repo
// keys ${PROJECT_SCRATCH_DIR} on the repo root at launch
// (EngineRepoRoot over the workDir), while its record keeps RepoRoot EMPTY
// by design (pinned by TestEngineRun_ExplicitNoneSkipsWorktreeOnGitRepo).
// A resume must re-resolve the same root from the restored workspace — not
// fall back to the workDir key, which would move the scratch (a
// crash-resume silently loses its working state; a fan-in through scratch
// reads an empty dir) and the memory tree, the very drift restoreRunEnv's
// own comment says it exists to prevent.
func TestResume_NonWorktreeSubdirKeepsLaunchScratchDir(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())

	wf := &ir.Workflow{
		Name:  "scratch-resume-subdir",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"gate":  &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{{From: "start", To: "gate"}, {From: "gate", To: "done"}},
		// The bot's own view of the scratch: a var expanded by the engine,
		// exactly as a `run_dir: ${PROJECT_SCRATCH_DIR}/...` declaration is.
		Vars: map[string]*ir.Var{
			"run_dir": {Name: "run_dir", Type: ir.VarString, HasDefault: true, Default: "${PROJECT_SCRATCH_DIR}"},
		},
		Worktree: "none",
	}

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	subdir := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	const runID = "run-scratch-resume-subdir"
	ctx := context.Background()

	launchSpy := &scratchPushSpy{}
	eng := New(wf, s, launchSpy, WithWorkDir(subdir), WithLogger(log.New(log.LevelWarn, os.Stderr)))
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("launch = %v, want ErrRunPaused at the gate", err)
	}

	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	// Fixture invariant: this must be the non-worktree arm — the record
	// keeps RepoRoot empty (the divergence's whole premise) and WorkDir is
	// the subdirectory, so the repo-root key and the workDir key genuinely
	// differ; otherwise the test proves nothing.
	if r.Worktree || r.RepoRoot != "" {
		t.Fatalf("fixture invariant broken: Worktree=%v RepoRoot=%q, want a non-worktree run with an empty baseline", r.Worktree, r.RepoRoot)
	}
	if r.WorkDir != subdir {
		t.Fatalf("fixture invariant broken: WorkDir=%q, want the launch subdirectory %q", r.WorkDir, subdir)
	}
	if memory.WorkspaceScratchDir(repo) == memory.WorkspaceScratchDir(subdir) {
		t.Fatalf("fixture invariant broken: repo root and subdir encode the same scratch key (%q)", memory.WorkspaceScratchDir(repo))
	}

	launchVar, _ := launchSpy.vars[len(launchSpy.vars)-1]["run_dir"].(string)
	if want := memory.WorkspaceScratchDir(repo); launchVar != want {
		t.Fatalf("launch ${PROJECT_SCRATCH_DIR} = %q, want the repo-root-keyed %q", launchVar, want)
	}
	launchRun, launchShared := launchSpy.lastScratch(t)

	// A fresh engine with NO WithWorkDir, as a runner restart sees the
	// resume: the environment must be rebuilt from the record alone.
	resumeSpy := &scratchPushSpy{}
	eng = New(wf, s, resumeSpy, WithLogger(log.New(log.LevelWarn, os.Stderr)))
	if err := eng.Resume(ctx, runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	if len(resumeSpy.vars) == 0 {
		t.Fatal("the resume never pushed the resolved vars")
	}
	resumedVar, _ := resumeSpy.vars[len(resumeSpy.vars)-1]["run_dir"].(string)
	if resumedVar != launchVar {
		t.Errorf("resume moved ${PROJECT_SCRATCH_DIR}: launch %q, resume %q — a crash-resume loses the scratch it filled", launchVar, resumedVar)
	}
	// The scratch read roots pushed to claw travel with the var (#2650,
	// #2869): same subtree, same shared root, on both sides of the pause.
	resumedRun, resumedShared := resumeSpy.lastScratch(t)
	if resumedRun != launchRun {
		t.Errorf("resume pushed run scratch root %q, want the launch's %q", resumedRun, launchRun)
	}
	if resumedShared != launchShared {
		t.Errorf("resume pushed shared scratch root %q, want the launch's %q", resumedShared, launchShared)
	}
}

// TestResume_RunScratchDirKeepsTheLaunchSubtree is the resume half of
// ${RUN_SCRATCH_DIR} (#2869): a fresh engine resuming a paused run must
// expand the var to the SAME subtree the launch gave the nodes. The owner
// the subtree keys on is settled by restoreRunEnv before the resume arms
// re-resolve the vars; resolved before it, the var silently falls back to
// the shared project scratch while the push still attaches runs/<id> — the
// resumed nodes would read and write a directory the state saved before
// the pause is not in, with no error anywhere.
func TestResume_RunScratchDirKeepsTheLaunchSubtree(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())

	wf := &ir.Workflow{
		Name:  "run-scratch-resume",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"gate":  &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{{From: "start", To: "gate"}, {From: "gate", To: "done"}},
		Vars: map[string]*ir.Var{
			"own_dir": {Name: "own_dir", Type: ir.VarString, HasDefault: true, Default: "${RUN_SCRATCH_DIR}"},
		},
		Worktree: "none",
	}

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	const runID = "run-scratch-resume-own"
	ctx := context.Background()

	launchSpy := &scratchPushSpy{}
	eng := New(wf, s, launchSpy, WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)))
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("launch = %v, want ErrRunPaused at the gate", err)
	}
	want := filepath.Join(memory.WorkspaceScratchDir(repo), memory.RunScratchDirName, runID)
	if got, _ := launchSpy.vars[len(launchSpy.vars)-1]["own_dir"].(string); got != want {
		t.Fatalf("launch ${RUN_SCRATCH_DIR} = %q, want the run's subtree %q", got, want)
	}

	resumeSpy := &scratchPushSpy{}
	eng = New(wf, s, resumeSpy, WithLogger(log.New(log.LevelWarn, os.Stderr)))
	if err := eng.Resume(ctx, runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(resumeSpy.vars) == 0 {
		t.Fatal("the resume never pushed the resolved vars")
	}
	if got, _ := resumeSpy.vars[len(resumeSpy.vars)-1]["own_dir"].(string); got != want {
		t.Errorf("resumed ${RUN_SCRATCH_DIR} = %q, want the launch's subtree %q — the resumed nodes lose the state saved before the pause", got, want)
	}
	if run, _ := resumeSpy.lastScratch(t); run != want {
		t.Errorf("resume pushed run read root %q, want %q", run, want)
	}
}
