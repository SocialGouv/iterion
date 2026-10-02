package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// throwAwayWorkDirs lists what defaultWorkDir left directly under dir.
func throwAwayWorkDirs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), throwAwayWorkDirPrefix) {
			left = append(left, e.Name())
		}
	}
	return left
}

// An engine built without WithWorkDir, under `go test` in the package
// directory, works in a throw-away directory (#1803) that must not outlive
// the call that needed it. A Run that pauses and the Resume that finishes it
// — on the same engine, and on a fresh engine that only has the run record —
// leave nothing in $TMPDIR. The resume used to remove its directory when a
// helper returned, then mirror its skills into the removed path, re-creating
// it with no owner (#2016: 10 970 left behind in three days).
func TestThrowAwayWorkDirDoesNotOutliveTheCallThatNeededIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		freshEngine bool
	}{
		{name: "same engine resumes", freshEngine: false},
		{name: "fresh engine resumes from the run record", freshEngine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			skill := &Contributions{Plugin: []ContributionFile{{Kind: "skills", Name: "witness.md", Content: []byte("# witness\n")}}}

			// The mirror must really write into the throw-away directory while
			// each call runs, or there is nothing to leak and the test proves
			// nothing: every node records whether it saw the mirrored skill.
			var eng, resumer *Engine
			mirrored := map[string]bool{}
			sawSkill := func(node string, e **Engine) func(map[string]any) (map[string]any, error) {
				return func(map[string]any) (map[string]any, error) {
					wd := (*e).workDir
					_, err := os.Stat(filepath.Join(wd, ".claude", "skills", "witness.md"))
					mirrored[node] = err == nil && strings.HasPrefix(filepath.Base(wd), throwAwayWorkDirPrefix)
					return map[string]any{"summary": "needs review", "result": "integrated"}, nil
				}
			}
			exec := newStubExecutor()
			exec.on("analyze", sawSkill("analyze", &eng))
			exec.on("integrate", sawSkill("integrate", &resumer))

			s := tmpStore(t)
			eng = New(humanWorkflow(), s, exec, WithContributions(skill))
			if err := eng.Run(context.Background(), "run-throwaway", nil); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("Run: want ErrRunPaused, got %v", err)
			}
			if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
				t.Fatalf("after the paused Run, throw-away workdirs are left in $TMPDIR: %v", left)
			}

			resumer = eng
			if tc.freshEngine {
				resumer = New(humanWorkflow(), s, exec, WithContributions(skill))
			}
			if err := resumer.Resume(context.Background(), "run-throwaway", map[string]any{"approve": true}); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
				t.Fatalf("after the Resume, throw-away workdirs are left in $TMPDIR: %v", left)
			}
			for _, node := range []string{"analyze", "integrate"} {
				if !mirrored[node] {
					t.Fatalf("node %s did not run in a throw-away workdir holding the mirrored skill — the scenario no longer exercises the leak (mirrored=%v)", node, mirrored)
				}
			}
		})
	}
}

// A resume after a failure restores its workspace in a helper of its own
// (restoreResumeWorkspace): the resumed node runs in a live throw-away dir
// holding the mirrored skill, and nothing is left once Resume returns — on the
// same engine and on a fresh one.
func TestThrowAwayWorkDir_AFailureResumeReleasesIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		freshEngine bool
	}{
		{name: "same engine resumes", freshEngine: false},
		{name: "fresh engine resumes from the run record", freshEngine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			skill := &Contributions{Plugin: []ContributionFile{{Kind: "skills", Name: "witness.md", Content: []byte("# witness\n")}}}

			var eng, resumer *Engine
			failOnce, resumedWithSkill := true, false
			exec := newStubExecutor()
			exec.on("step_a", func(map[string]any) (map[string]any, error) { return map[string]any{"result": "a"}, nil })
			exec.on("step_b", func(map[string]any) (map[string]any, error) {
				if failOnce {
					failOnce = false
					return nil, errors.New("transient failure")
				}
				wd := resumer.workDir
				_, err := os.Stat(filepath.Join(wd, ".claude", "skills", "witness.md"))
				resumedWithSkill = err == nil && strings.HasPrefix(filepath.Base(wd), throwAwayWorkDirPrefix)
				return map[string]any{"result": "b"}, nil
			})

			s := tmpStore(t)
			eng = New(charResumeWF(), s, exec, WithContributions(skill))
			if err := eng.Run(context.Background(), "run-failure-resume", nil); err == nil {
				t.Fatal("Run: want the transient failure of step_b, got nil")
			}
			if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
				t.Fatalf("after the failed Run, throw-away workdirs are left in $TMPDIR: %v", left)
			}

			resumer = eng
			if tc.freshEngine {
				resumer = New(charResumeWF(), s, exec, WithContributions(skill))
			}
			if err := resumer.Resume(context.Background(), "run-failure-resume", nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
				t.Fatalf("after the failure Resume, throw-away workdirs are left in $TMPDIR: %v", left)
			}
			if !resumedWithSkill {
				t.Fatal("step_b did not resume in a live throw-away workdir holding the mirrored skill — the scenario no longer exercises the failure-resume lifecycle")
			}
		})
	}
}

// A parent built without WithWorkDir hands its throw-away dir to a child the
// way the host runners do (WithWorkDir(req.WorkDir)). The child never records
// that path on its run: resumed after the parent released it, it would adopt
// the removed path and its skill mirror would re-create it with no owner.
func TestThrowAwayWorkDir_AChildNeverRecordsItsParentsThrowAway(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	skill := &Contributions{Plugin: []ContributionFile{{Kind: "skills", Name: "witness.md", Content: []byte("# witness\n")}}}
	exec := newStubExecutor()
	exec.on("analyze", func(map[string]any) (map[string]any, error) { return map[string]any{"summary": "needs review"}, nil })
	exec.on("integrate", func(map[string]any) (map[string]any, error) { return map[string]any{"result": "integrated"}, nil })
	s := tmpStore(t)

	var handed string
	runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
		handed = req.WorkDir
		child := New(humanWorkflow(), s, exec, WithWorkDir(req.WorkDir), WithParentRunID(req.ParentRunID), WithContributions(skill))
		if err := child.Run(ctx, "child-run", nil); err != nil && !errors.Is(err, ErrRunPaused) {
			return nil, err
		}
		return map[string]any{}, nil
	}
	parent := New(&ir.Workflow{
		Name: "parent", Worktree: "none", Entry: "run_child",
		Nodes: map[string]ir.Node{
			"run_child": &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "run_child"}, Source: "child.bot"},
			"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "run_child", To: "done"}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}, s, exec, WithSubbotRunner(runner))
	if err := parent.Run(context.Background(), "parent-run", nil); err != nil {
		t.Fatalf("parent Run: %v", err)
	}
	if filepath.Dir(handed) != tmp || !strings.HasPrefix(filepath.Base(handed), throwAwayWorkDirPrefix) {
		t.Fatalf("the parent handed %q to its child, want its throw-away workdir under %s — the scenario no longer exercises a handed-down throw-away", handed, tmp)
	}
	child, err := s.LoadRun(context.Background(), "child-run")
	if err != nil {
		t.Fatal(err)
	}
	if child.WorkDir == handed {
		t.Fatalf("the child recorded its parent's throw-away workdir %s on its run", handed)
	}

	resumer := New(humanWorkflow(), s, exec, WithContributions(skill))
	if err := resumer.Resume(context.Background(), "child-run", map[string]any{"approve": true}); err != nil {
		t.Fatalf("resume the child: %v", err)
	}
	if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
		t.Fatalf("after the child's resume, throw-away workdirs are left in $TMPDIR: %v", left)
	}
}
