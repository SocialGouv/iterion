package runtime

// Cloud-shape worktree coverage.
//
// The cloud runner executes a repo run inside a per-run clone that is
// DELETED at the end of every queue delivery and re-cloned fresh on the
// next one (pkg/runner/loop.go executeRun: prepareRepoWorkspace +
// deferred os.RemoveAll). Its store is the Mongo store, whose Root() is
// "". Those two facts together are the precondition the engine's
// worktree machinery must respect: a git worktree anchored outside the
// clone has its gitdir INSIDE the clone's .git, so the recycle severs
// the linkage and every git command in the workspace fails with
// "fatal: not a git repository: <clone>/.git/worktrees/<run-id>".
//
// These tests pin the contract from the engine boundary with the exact
// cloud wiring shape (rootless store + delegated per-run clone +
// clone recycling between deliveries): git must work in the workspace
// every node receives, on the launch delivery AND after resume.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// rootlessStore mirrors the cloud Mongo store's shape: a full RunStore
// with no filesystem root (pkg/store/mongo Store.Root() returns "").
type rootlessStore struct{ store.RunStore }

func (rootlessStore) Root() string { return "" }

// workDirProbeExecutor is a stubExecutor that also records the workDir
// the engine pushes via workDirSetter — the same seam real backends and
// tool nodes receive their cwd through — so node handlers can probe git
// exactly where a real node would run.
type workDirProbeExecutor struct {
	*stubExecutor
	workDir string
}

func (w *workDirProbeExecutor) SetWorkDir(dir string) { w.workDir = dir }

// gitToplevel runs `git rev-parse --show-toplevel` in dir, returning the
// resolved repo top or the full git error (which carries the
// "not a git repository: <gitdir>" detail on a severed worktree).
func gitToplevel(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %v: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// TestCloudRun_WorktreeAuto_GitWorksAcrossDeliveries reproduces the
// cloud delivery lifecycle for a `worktree: auto` workflow:
//
//	delivery 1: fresh per-run clone → engine.Run → pause at a human node
//	(recycle)   the runner deletes the clone and re-clones it fresh
//	delivery 2: fresh engine → engine.Resume → downstream nodes execute
//
// and asserts the precondition every downstream consumer relies on: git
// is functional in the workspace each node actually receives, and that
// workspace IS the live per-run clone. It also asserts the engine never
// fabricates a cwd-anchored worktree from the rootless store.
func TestCloudRun_WorktreeAuto_GitWorksAcrossDeliveries(t *testing.T) {
	seed, _ := initBareishRepo(t)

	// Pod filesystem analog: the runner's per-run clone dir and a
	// distinct process cwd (the pod's $HOME), which is where a
	// store.Root()=="" worktree would be anchored.
	pod := t.TempDir()
	runID := "run-cloud-wt"
	clone := filepath.Join(pod, "repos", runID)
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatalf("mkdir repos: %v", err)
	}
	cloneFresh := func() {
		t.Helper()
		if err := os.RemoveAll(clone); err != nil {
			t.Fatalf("remove clone: %v", err)
		}
		mustRun(t, pod, "git", "clone", "--quiet", seed, clone)
	}
	cloneFresh()

	home := filepath.Join(pod, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Chdir(home)

	st := rootlessStore{RunStore: tmpStore(t)}

	wf := &ir.Workflow{
		Name:  "cloud-wt",
		Entry: "probe1",
		Nodes: map[string]ir.Node{
			"probe1": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "probe1"}},
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
			},
			"probe2": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "probe2"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "probe1", To: "gate"},
			{From: "gate", To: "probe2"},
			{From: "probe2", To: "done"},
		},
		Schemas:  map[string]*ir.Schema{},
		Prompts:  map[string]*ir.Prompt{},
		Vars:     map[string]*ir.Var{},
		Loops:    map[string]*ir.Loop{},
		Worktree: "auto",
	}

	// Delivery 1: launch, probe git from the node's workspace, pause.
	exec1 := &workDirProbeExecutor{stubExecutor: newStubExecutor()}
	var probe1Top string
	var probe1Err error
	exec1.on("probe1", func(map[string]any) (map[string]any, error) {
		probe1Top, probe1Err = gitToplevel(exec1.workDir)
		return map[string]any{"ok": true}, nil
	})
	eng1 := New(wf, st, exec1,
		WithWorkDir(clone),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)
	if err := eng1.Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("engine.Run: want ErrRunPaused at the gate, got: %v", err)
	}
	if probe1Err != nil {
		t.Fatalf("git broken in the launch delivery's workspace: %v", probe1Err)
	}

	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Worktree {
		t.Errorf("run.Worktree = true: a rootless store cannot host a durable worktree — the run must execute in place in the per-run clone")
	}
	if r.WorkDir != clone {
		t.Errorf("run.WorkDir = %q, want the per-run clone %q", r.WorkDir, clone)
	}
	if _, statErr := os.Stat(filepath.Join(home, "worktrees")); !os.IsNotExist(statErr) {
		t.Errorf("cwd-anchored worktree dir fabricated at %s (stat err=%v) — store.Root()==\"\" must not become a worktree home", filepath.Join(home, "worktrees"), statErr)
	}

	// Delivery boundary: the runner recycles the per-run clone
	// (deferred os.RemoveAll at end of delivery, fresh clone on pickup).
	cloneFresh()

	// Delivery 2: a fresh engine resumes the run; the downstream node
	// must receive a workspace where git works.
	exec2 := &workDirProbeExecutor{stubExecutor: newStubExecutor()}
	var probe2Top string
	var probe2Err error
	exec2.on("probe2", func(map[string]any) (map[string]any, error) {
		probe2Top, probe2Err = gitToplevel(exec2.workDir)
		return map[string]any{"ok": true}, nil
	})
	eng2 := New(wf, st, exec2,
		WithWorkDir(clone),
		WithLogger(log.New(log.LevelWarn, os.Stderr)),
	)
	if err := eng2.Resume(context.Background(), runID, map[string]any{"note": "ok"}); err != nil {
		t.Fatalf("engine.Resume: %v", err)
	}
	if probe2Err != nil {
		t.Fatalf("git broken in the resumed delivery's workspace %q: %v", exec2.workDir, probe2Err)
	}

	// The workspace both nodes probed must BE the live per-run clone.
	wantTop, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatalf("resolve clone path: %v", err)
	}
	for probe, got := range map[string]string{"probe1": probe1Top, "probe2": probe2Top} {
		resolved, rerr := filepath.EvalSymlinks(got)
		if rerr != nil {
			t.Fatalf("%s toplevel %q: %v", probe, got, rerr)
		}
		if resolved != wantTop {
			t.Errorf("%s ran in repo %q, want the per-run clone %q", probe, resolved, wantTop)
		}
	}

	final, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run after resume: %v", err)
	}
	if final.Status != store.RunStatusFinished {
		t.Errorf("run status = %q, want %q", final.Status, store.RunStatusFinished)
	}
}

// TestResume_RefusesSeveredWorktreeLinkage pins the loud-failure guard
// for runs persisted with a worktree workspace whose registering
// repository is gone: Resume must return an explicit error naming the
// dead gitdir instead of claiming the run and executing nodes in a
// workspace where git silently answers "no repo".
func TestResume_RefusesSeveredWorktreeLinkage(t *testing.T) {
	st := tmpStore(t)

	ws := t.TempDir()
	goneGitDir := filepath.Join(t.TempDir(), "gone-clone", ".git", "worktrees", "run-severed")
	writeFile(t, filepath.Join(ws, ".git"), "gitdir: "+goneGitDir+"\n")

	wf := &ir.Workflow{
		Name:  "severed-wt",
		Entry: "step1",
		Nodes: map[string]ir.Node{
			"step1": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step1"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "step1", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	ctx := context.Background()
	r, err := st.CreateRun(ctx, "run-severed", wf.Name, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	r.Status = store.RunStatusFailedResumable
	r.Worktree = true
	r.WorkDir = ws
	r.Checkpoint = &store.Checkpoint{NodeID: "step1"}
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}

	exec := newStubExecutor()
	executed := false
	exec.on("step1", func(map[string]any) (map[string]any, error) {
		executed = true
		return map[string]any{}, nil
	})
	eng := New(wf, st, exec, WithLogger(log.New(log.LevelWarn, os.Stderr)))

	resumeErr := eng.Resume(ctx, "run-severed", nil)
	if resumeErr == nil {
		t.Fatalf("Resume succeeded on a severed worktree workspace — want an explicit refusal")
	}
	if !strings.Contains(resumeErr.Error(), goneGitDir) {
		t.Errorf("resume error %q does not name the dead gitdir %q", resumeErr, goneGitDir)
	}
	if executed {
		t.Errorf("node executed despite the severed workspace — the guard must refuse before running anything")
	}
	// The run keeps its resumable status: no claim happened.
	after, err := st.LoadRun(ctx, "run-severed")
	if err != nil {
		t.Fatalf("load run after refused resume: %v", err)
	}
	if after.Status != store.RunStatusFailedResumable {
		t.Errorf("run status = %q after refused resume, want %q (untouched)", after.Status, store.RunStatusFailedResumable)
	}
}

// TestCheckWorktreeLinkage covers the predicate's non-firing shapes: it
// must flag ONLY the severed pointer, leaving every other workspace
// shape to its existing behaviour.
func TestCheckWorktreeLinkage(t *testing.T) {
	repo, _ := initBareishRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	mustRun(t, repo, "git", "worktree", "add", "--detach", linked, "HEAD")
	t.Cleanup(func() { _ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", linked).Run() })

	severed := t.TempDir()
	writeFile(t, filepath.Join(severed, ".git"), "gitdir: "+filepath.Join(t.TempDir(), "nope", ".git", "worktrees", "x"))

	nonPointer := t.TempDir()
	writeFile(t, filepath.Join(nonPointer, ".git"), "not a pointer\n")

	cases := []struct {
		name    string
		workDir string
		wantErr bool
	}{
		{"empty workDir", "", false},
		{"missing workspace", filepath.Join(t.TempDir(), "absent"), false},
		{"main checkout (.git directory)", repo, false},
		{"intact linked worktree", linked, false},
		{"non-pointer .git file", nonPointer, false},
		{"severed pointer", severed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkWorktreeLinkage(tc.workDir)
			if tc.wantErr && err == nil {
				t.Fatalf("checkWorktreeLinkage(%q) = nil, want error", tc.workDir)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkWorktreeLinkage(%q) = %v, want nil", tc.workDir, err)
			}
		})
	}
}
