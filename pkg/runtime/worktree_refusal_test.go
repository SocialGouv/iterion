package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestEarlyRefusalReclaimsOnlyPristineWorktree(t *testing.T) {
	for _, shape := range []string{"entry", "compute", "resumable", "tool", "dirty", "ignored", "committed"} {
		t.Run(shape, func(t *testing.T) {
			ctx := context.Background()
			repo, _ := initBareishRepo(t)
			s := tmpStore(t)
			wf := resumableFailWorkflow(t, "guard")
			wf.Worktree = "auto"
			wf.Nodes["refuse"].(*ir.FailNode).Resumable = shape == "resumable"
			if shape == "entry" {
				wf.Entry = "refuse"
			}
			if shape == "tool" {
				wf.Nodes["guard"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "guard"}, Command: "true"}
				wf.Edges = []*ir.Edge{{From: "guard", To: "refuse"}}
			}
			runID := "refusal-" + shape
			wt := filepath.Join(s.Root(), "worktrees", runID)
			eng := New(wf, s, newStubExecutor(), WithWorkDir(repo), WithEventObserver(func(ev store.Event) {
				if ev.Type != store.EventRunStarted {
					return
				}
				switch shape {
				case "dirty":
					writeFile(t, filepath.Join(wt, "evidence.txt"), "keep this\n")
				case "ignored":
					gittest.Run(t, wt, "config", "core.excludesFile", filepath.Join(repo, "ignore-rules"))
					writeFile(t, filepath.Join(repo, "ignore-rules"), "evidence.log\n")
					writeFile(t, filepath.Join(wt, "evidence.log"), "keep this too\n")
				case "committed":
					addCommit(t, wt, "evidence.txt", "keep commit\n", "evidence")
				}
			}))
			if err := eng.Run(ctx, runID, nil); err == nil {
				t.Fatal("expected refusal")
			}
			run, err := s.LoadRun(ctx, runID)
			if err != nil || run.Checkpoint == nil {
				t.Fatalf("refusal checkpoint must survive: run=%+v err=%v", run, err)
			}
			_, statErr := os.Stat(wt)
			wantRemoved := shape == "entry" || shape == "compute"
			if gotRemoved := os.IsNotExist(statErr); gotRemoved != wantRemoved {
				t.Fatalf("worktree removed=%v, want %v (stat=%v)", gotRemoved, wantRemoved, statErr)
			}
		})
	}
}

func TestEarlyRefusalCleanupCannotRaceReconstruction(t *testing.T) {
	ctx := context.Background()
	repo, base := initBareishRepo(t)
	st := tmpStore(t)
	const runID = "refusal-race"
	run, err := st.CreateRun(ctx, runID, "refused", nil)
	if err != nil {
		t.Fatal(err)
	}
	wc, cleanup, err := setupWorktree(st.Root(), runID, repo, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Worktree, run.WorkDir = true, wc.wtPath
	run.RepoRoot, run.BaseCommit, run.Status = repo, base, store.RunStatusFailed
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	eng := New(resumableFailWorkflow(t, "refuse"), st, newStubExecutor(), WithWorkDir(wc.wtPath))
	if !eng.reclaimEarlyRefusalWorktree(ctx, runID, &wc, func() {
		// A rewind arrives AFTER the recovery marker is persisted but
		// BEFORE the checkout is deleted. It must not clear the marker,
		// start writing here, then have its files removed by this cleanup.
		claimed, err := st.LoadRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		claimed.Status = store.RunStatusCancelled
		if err := st.SaveRun(ctx, claimed); err != nil {
			t.Fatal(err)
		}
		if err := RestoreReclaimedWorktree(ctx, st, claimed); err == nil || !strings.Contains(err.Error(), "locked") {
			t.Fatalf("concurrent reconstruction should report lifecycle lock: %v", err)
		}
		cleanup()
	}) {
		t.Fatal("pristine checkout was not reclaimed")
	}
	current, err := st.LoadRun(ctx, runID)
	if err != nil || !current.WorktreeReclaimed || current.Status != store.RunStatusCancelled {
		t.Fatalf("interrupted rewind lost recovery state: %+v %v", current, err)
	}
	if err := RestoreReclaimedWorktree(ctx, st, current); err != nil {
		t.Fatal(err)
	}
	if got := readHEAD(current.WorkDir); got != base {
		t.Fatalf("reconstructed %s, want %s", got, base)
	}
}

func TestResumeRestoresReclaimedWorktreeAfterInterruptedRewind(t *testing.T) {
	for _, missingBase := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "missing_baseline"}[missingBase], func(t *testing.T) {
			ctx := context.Background()
			repo, base := initBareishRepo(t)
			st := tmpStore(t)
			wf := resumableFailWorkflow(t, "guard")
			wf.Worktree = "auto"
			wf.Nodes["refuse"].(*ir.FailNode).Resumable = false
			const runID = "refused-then-rewound"
			if err := New(wf, st, newStubExecutor(), WithWorkDir(repo)).Run(ctx, runID, nil); err == nil {
				t.Fatal("expected refusal")
			}
			run, err := st.LoadRun(ctx, runID)
			if err != nil || !run.WorktreeReclaimed {
				t.Fatalf("missing recovery marker: run=%+v err=%v", run, err)
			}
			// A rewind claimed the refusal but stopped before reconstructing.
			run.Status = store.RunStatusCancelled
			run.Checkpoint.NodeID = "guard"
			if err := st.SaveRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			newTip := addCommit(t, repo, "new-main.txt", "new unrelated main\n", "main moved")
			if missingBase {
				gittest.Run(t, repo, "update-ref", "-d", pristineWorktreeRef(runID))
			}
			stop, err := expr.Parse("false")
			if err != nil {
				t.Fatal(err)
			}
			wf.Nodes["guard"].(*ir.ComputeNode).Exprs[0].AST = stop
			started := false
			eng := New(wf, st, newStubExecutor(), WithWorkDir(repo), WithEventObserver(func(ev store.Event) {
				if ev.Type == store.EventNodeStarted {
					started = true
					if got := readHEAD(run.WorkDir); got != base {
						t.Errorf("resumed at %s, want original %s", got, base)
					}
				}
			}))
			resumeErr := eng.Resume(ctx, runID, nil)
			current, err := st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if missingBase {
				if resumeErr == nil || started || current.Status != store.RunStatusCancelled || !current.WorktreeReclaimed {
					t.Fatalf("missing baseline must park without dispatch: err=%v started=%v run=%+v", resumeErr, started, current)
				}
			} else if resumeErr != nil || !started || current.WorktreeReclaimed || current.Status != store.RunStatusFinished {
				t.Fatalf("resume failed: err=%v started=%v run=%+v", resumeErr, started, current)
			}
			if got := readHEAD(repo); got != newTip {
				t.Fatalf("operator checkout moved: %s, want %s", got, newTip)
			}
		})
	}
}
