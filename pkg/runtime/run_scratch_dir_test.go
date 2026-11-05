package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/memory"
)

// varsReferencePath is the narrowing's detection (#2869): the shared
// scratch root rides along exactly while a resolved var still names it,
// and a ${RUN_SCRATCH_DIR} value — which lives BENEATH the shared root —
// must not count as such a reference.
func TestVarsReferencePath(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "scratch")
	runSubtree := filepath.Join(shared, "runs", "run-1")

	for _, tc := range []struct {
		name string
		vars map[string]any
		want bool
	}{
		{"no vars at all", nil, false},
		{"an unrelated value", map[string]any{"x": "elsewhere"}, false},
		{"the shared root itself", map[string]any{"run_dir": shared}, true},
		{"a path under the shared root", map[string]any{"run_dir": filepath.Join(shared, "bot")}, true},
		{"only the run subtree", map[string]any{"own_dir": runSubtree}, false},
		{"a path under the run subtree", map[string]any{"own_dir": filepath.Join(runSubtree, "briefs")}, false},
		{"the run subtree beside a real shared reference", map[string]any{
			"own_dir": runSubtree, "cache": filepath.Join(shared, "cache.json"),
		}, true},
		{"nested in a list", map[string]any{"x": []any{filepath.Join(shared, "a")}}, true},
		{"nested in a map", map[string]any{"x": map[string]any{"y": shared}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := varsReferencePath(tc.vars, shared, runSubtree); got != tc.want {
				t.Errorf("varsReferencePath(%v) = %v, want %v", tc.vars, got, tc.want)
			}
		})
	}
}

// TestRunScratchOwnerFollowsTheFamilyRoot pins the fan-in half of the
// narrowing (#2869): a sub-bot child's ${RUN_SCRATCH_DIR} keys on the
// family's ROOT run, so the child writes where the parent reads — the
// shared project scratch used to be what carried that.
func TestRunScratchOwnerFollowsTheFamilyRoot(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	ctx := context.Background()

	wf := &ir.Workflow{
		Name:  "scratch-owner",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{{From: "start", To: "done"}},
	}

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	run := func(eng *Engine, runID string) {
		t.Helper()
		if err := eng.Run(ctx, runID, nil); err != nil {
			t.Fatalf("engine.Run(%s): %v", runID, err)
		}
	}

	// A standalone run keys its subtree on itself.
	solo := New(wf, s, &scratchPushSpy{}, WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)))
	run(solo, "run-solo")
	if got := solo.runScratchDirVarValue(); got != filepath.Join(solo.projectScratchDirVarValue(), "runs", "run-solo") {
		t.Errorf("solo run scratch = %q, want runs/run-solo under the project scratch", got)
	}

	// A child of a recorded parent keys on the PARENT; a grandchild keys
	// on the family root — one subtree per fan-in family.
	parent := New(wf, s, &scratchPushSpy{}, WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)))
	run(parent, "run-parent")
	child := New(wf, s, &scratchPushSpy{}, WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)), WithParentRunID("run-parent"))
	run(child, "run-child")
	if got, want := child.runScratchDirVarValue(), filepath.Join(child.projectScratchDirVarValue(), "runs", "run-parent"); got != want {
		t.Errorf("child run scratch = %q, want the parent's subtree %q", got, want)
	}
	grandchild := New(wf, s, &scratchPushSpy{}, WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)), WithParentRunID("run-child"))
	run(grandchild, "run-grandchild")
	if got, want := grandchild.runScratchDirVarValue(), filepath.Join(grandchild.projectScratchDirVarValue(), "runs", "run-parent"); got != want {
		t.Errorf("grandchild run scratch = %q, want the family root's subtree %q", got, want)
	}

	// A child whose parent was never recorded here keeps the last id the
	// walk reached rather than failing.
	orphan := New(wf, s, &scratchPushSpy{}, WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)), WithParentRunID("run-elsewhere"))
	run(orphan, "run-orphan")
	if got, want := orphan.runScratchDirVarValue(), filepath.Join(orphan.projectScratchDirVarValue(), "runs", "run-elsewhere"); got != want {
		t.Errorf("orphan child run scratch = %q, want the parent's id %q", got, want)
	}
}

// TestRunScratchOwnerRefusesATraversalShapedParent is the containment half
// of the owner walk (#2869): every id the walk adopts is a path component
// of ${RUN_SCRATCH_DIR} and of the claw read root attached on it, and a
// parent id arrives verbatim from a launch request. A '..'-shaped one must
// end the walk, not become the component — otherwise
// <project scratch>/runs/../../.. is the iterion home's projects dir and
// the run reads every other repository's scratch. Driven through
// Engine.Run, the path a POST /api/runs parent_run_id takes, and through a
// RECORDED parent whose own parent id is traversal-shaped (the resume walk
// reads r.ParentRunID from the record the same way).
func TestRunScratchOwnerRefusesATraversalShapedParent(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	ctx := context.Background()

	wf := &ir.Workflow{
		Name:  "scratch-owner-traversal",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{{From: "start", To: "done"}},
	}

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	launch := func(runID, parentID string) (*Engine, string) {
		t.Helper()
		spy := &scratchPushSpy{}
		eng := New(wf, s, spy, WithWorkDir(repo), WithLogger(log.New(log.LevelError, os.Stderr)), WithParentRunID(parentID))
		if err := eng.Run(ctx, runID, nil); err != nil {
			t.Fatalf("engine.Run(%s): %v", runID, err)
		}
		pushedRun, _ := spy.lastScratch(t)
		return eng, pushedRun
	}
	underProjectScratch := func(eng *Engine, pushed, wantOwner string) {
		t.Helper()
		shared := eng.projectScratchDirVarValue()
		if want := filepath.Join(shared, "runs", wantOwner); pushed != want {
			t.Fatalf("pushed run read root = %q, want %q — the subtree left the project scratch %q", pushed, want, shared)
		}
	}

	for i, parent := range []string{"../../..", "..", "a/../../b"} {
		runID := fmt.Sprintf("run-traversal-%d", i)
		eng, pushed := launch(runID, parent)
		underProjectScratch(eng, pushed, runID)
	}

	// A recorded parent whose own recorded parent id is traversal-shaped:
	// the walk adopts the recorded parent and stops there.
	launch("run-recorded-parent", "..")
	eng, pushed := launch("run-recorded-child", "run-recorded-parent")
	underProjectScratch(eng, pushed, "run-recorded-parent")
}

// TestEnsureRunScratchDirNeverCreatesThroughAPlantedLink is the host half
// of the subtree's creation (#2869): sandboxed runs write the project
// scratch through their bind, so runs/ can be a link one of them planted.
// The engine's push must not MkdirAll through it — that would create the
// next run's subtree, and let it write its state, wherever the link
// points. The run itself still starts.
func TestEnsureRunScratchDirNeverCreatesThroughAPlantedLink(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	ctx := context.Background()

	wf := &ir.Workflow{
		Name:  "scratch-planted-runs",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:    []*ir.Edge{{From: "start", To: "done"}},
		Worktree: "none",
	}
	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	scratch := memory.WorkspaceScratchDir(repo)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(scratch, memory.RunScratchDirName)); err != nil {
		t.Fatal(err)
	}

	const runID = "run-planted-runs"
	eng := New(wf, s, &scratchPushSpy{}, WithWorkDir(repo), WithLogger(log.New(log.LevelError, os.Stderr)))
	if err := eng.Run(ctx, runID, nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, runID)); !os.IsNotExist(err) {
		t.Fatalf("the run subtree was created through the planted runs/ link at %s (%v)", filepath.Join(outside, runID), err)
	}
}
