package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
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
