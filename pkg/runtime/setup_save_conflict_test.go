package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// setupInterleaveStore lands a foreign write on the run document right
// before a chosen setup SaveRun, between the engine's last load and its save.
// On the Mongo store that is the runner's run-log flusher: every AppendRunLog
// fences the parent run with a version bump (withValueRecordWrite), every
// runLogFlushInterval, while setup is still writing full documents.
type setupInterleaveStore struct {
	store.RunStore
	// before selects the save the foreign write precedes.
	before func(*store.Run) bool
	// foreign is the concurrent writer; it must touch no field setup owns.
	foreign func(context.Context, store.RunStore, string) error
	// remaining is how many saves get a foreign write first; -1 is every one.
	remaining   int
	interleaved int
	conflicts   int
	foreignErr  error
}

func (s *setupInterleaveStore) Unwrap() store.RunStore { return s.RunStore }

func (s *setupInterleaveStore) SaveRun(ctx context.Context, r *store.Run) error {
	if s.remaining != 0 && s.before(r) {
		if s.remaining > 0 {
			s.remaining--
		}
		s.interleaved++
		if err := s.foreign(ctx, s.RunStore, r.ID); err != nil {
			s.foreignErr = err
			return err
		}
	}
	err := s.RunStore.SaveRun(ctx, r)
	if errors.Is(err, store.ErrRunConflict) {
		s.conflicts++
	}
	return err
}

// stampRunnerVersion is a foreign granular write: it advances the run's CAS
// version and touches only runner_version, a field no setup step owns.
func stampRunnerVersion(ctx context.Context, s store.RunStore, runID string) error {
	stamper, ok := s.(interface {
		SetRunnerVersion(context.Context, string, string) error
	})
	if !ok {
		return errors.New("fixture store cannot stamp a runner version")
	}
	return stamper.SetRunnerVersion(ctx, runID, "foreign-build")
}

// isWorkspaceSave is runPersistWorkspace's save: the first one carrying the
// work dir.
func isWorkspaceSave(r *store.Run) bool { return r.WorkDir != "" }

// isMetadataSave is runResolveDoc's stamp, the save before admission.
func isMetadataSave(r *store.Run) bool { return r.Admission == nil && r.WorkDir == "" }

func setupConflictEngine(t *testing.T, s store.RunStore, workDir string, calls *int) *Engine {
	t.Helper()
	wf := admissionWorkflow()
	wf.Worktree = "none"
	wf.Entry = "probe"
	wf.Nodes["probe"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "probe"}, Command: "unused"}
	wf.Edges = []*ir.Edge{{From: "probe", To: "done"}}
	ex := newStubExecutor()
	ex.on("probe", func(map[string]any) (map[string]any, error) { *calls++; return map[string]any{}, nil })
	return New(wf, s, ex, WithWorkDir(workDir), WithSandboxOverride("none"), WithRunName("setup-conflict"))
}

// TestSetupSaveSurvivesForeignWrite reproduces #3732: a writer of fields
// setup does not own lands between the engine's last load and a setup
// SaveRun. Setup must re-apply only its own fields on the fresh document and
// keep going, and the foreign write must survive.
func TestSetupSaveSurvivesForeignWrite(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before func(*store.Run) bool
	}{
		{"save work dir", isWorkspaceSave},
		{"save run metadata", isMetadataSave},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tmpStore(t)
			row, err := base.CreateRun(t.Context(), "setup-cas", "admission", nil)
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &setupInterleaveStore{RunStore: base, before: tc.before, foreign: stampRunnerVersion, remaining: 1}
			workDir := t.TempDir()
			calls := 0
			runErr := setupConflictEngine(t, wrapped, workDir, &calls).Run(t.Context(), row.ID, nil)
			if wrapped.foreignErr != nil {
				t.Fatalf("foreign write: %v", wrapped.foreignErr)
			}
			if wrapped.interleaved != 1 || wrapped.conflicts != 1 {
				t.Fatalf("fixture did not cross exactly one setup CAS conflict: interleaved=%d conflicts=%d", wrapped.interleaved, wrapped.conflicts)
			}
			if runErr != nil {
				t.Fatalf("run died on a foreign write to fields setup does not own: %v", runErr)
			}
			fresh, err := base.LoadRun(t.Context(), row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || fresh.Status != store.RunStatusFinished {
				t.Fatalf("run did not execute to the end: calls=%d status=%s", calls, fresh.Status)
			}
			if fresh.RunnerVersion != "foreign-build" {
				t.Fatalf("setup clobbered the foreign write: runner_version=%q", fresh.RunnerVersion)
			}
			if fresh.WorkDir != workDir || fresh.Name != "setup-conflict" {
				t.Fatalf("setup fields lost on the retried save: work_dir=%q name=%q", fresh.WorkDir, fresh.Name)
			}
		})
	}
}

// TestSetupSaveConflictStaysExplicit: a writer that never lets the document
// settle exhausts the bounded retry, and the run fails on the typed conflict
// instead of executing on an unsaved workspace.
func TestSetupSaveConflictStaysExplicit(t *testing.T) {
	base := tmpStore(t)
	row, err := base.CreateRun(t.Context(), "setup-cas-busy", "admission", nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &setupInterleaveStore{RunStore: base, before: isWorkspaceSave, foreign: stampRunnerVersion, remaining: -1}
	calls := 0
	runErr := setupConflictEngine(t, wrapped, t.TempDir(), &calls).Run(t.Context(), row.ID, nil)
	if !errors.Is(runErr, store.ErrRunConflict) {
		t.Fatalf("want the typed conflict, got %v", runErr)
	}
	if wrapped.conflicts != ownedSaveAttempts {
		t.Fatalf("want %d bounded attempts, got %d conflicts", ownedSaveAttempts, wrapped.conflicts)
	}
	if calls != 0 {
		t.Fatal("execution reached with an unsaved workspace")
	}
}

// TestSetupSaveDoesNotRetryOverMovedRun: a conflict whose fresh document is
// no longer this execution's (here a concurrent cancel) is not retried; the
// setup step never writes over the move.
func TestSetupSaveDoesNotRetryOverMovedRun(t *testing.T) {
	base := tmpStore(t)
	row, err := base.CreateRun(t.Context(), "setup-cas-cancel", "admission", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel := func(ctx context.Context, s store.RunStore, runID string) error {
		return s.UpdateRunStatus(ctx, runID, store.RunStatusCancelled, "operator cancel")
	}
	wrapped := &setupInterleaveStore{RunStore: base, before: isWorkspaceSave, foreign: cancel, remaining: 1}
	calls := 0
	runErr := setupConflictEngine(t, wrapped, t.TempDir(), &calls).Run(t.Context(), row.ID, nil)
	if !errors.Is(runErr, store.ErrRunConflict) {
		t.Fatalf("want the typed conflict, got %v", runErr)
	}
	if wrapped.conflicts != 1 {
		t.Fatalf("a moved run was retried: conflicts=%d", wrapped.conflicts)
	}
	fresh, err := base.LoadRun(t.Context(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(runErr, ErrAdmissionStopped) || calls != 0 || fresh.Status != store.RunStatusCancelled || fresh.WorkDir != "" {
		t.Fatalf("setup wrote over a cancelled run: err=%v calls=%d status=%s work_dir=%q", runErr, calls, fresh.Status, fresh.WorkDir)
	}
}

// TestGateMergeSaveSurvivesForeignWrite: the review gate's merge record is the
// same load-modify-save shape mid-run, under the same concurrent writers.
func TestGateMergeSaveSurvivesForeignWrite(t *testing.T) {
	base := tmpStore(t)
	row, err := base.CreateRun(t.Context(), "gate-merge-cas", "admission", nil)
	if err != nil {
		t.Fatal(err)
	}
	isMergeSave := func(r *store.Run) bool { return r.MergeStatus == store.MergeStatusMerged }
	wrapped := &setupInterleaveStore{RunStore: base, before: isMergeSave, foreign: stampRunnerVersion, remaining: 1}
	calls := 0
	eng := setupConflictEngine(t, wrapped, t.TempDir(), &calls)
	if err := eng.persistGateMerge(t.Context(), row.ID, "abc123", "iterion/run/x", "main", "def456", string(store.MergeStatusMerged), "squash"); err != nil {
		t.Fatalf("gate merge record lost to a foreign write: %v", err)
	}
	if wrapped.foreignErr != nil || wrapped.conflicts != 1 {
		t.Fatalf("fixture did not cross exactly one CAS conflict: foreign=%v conflicts=%d", wrapped.foreignErr, wrapped.conflicts)
	}
	fresh, err := base.LoadRun(t.Context(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.RunnerVersion != "foreign-build" || fresh.MergeStatus != store.MergeStatusMerged || fresh.MergedCommit != "def456" || fresh.FinalBranch != "iterion/run/x" {
		t.Fatalf("merge record or foreign write lost: runner_version=%q merge_status=%s merged_commit=%q final_branch=%q", fresh.RunnerVersion, fresh.MergeStatus, fresh.MergedCommit, fresh.FinalBranch)
	}
}
