package runview

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A resume that raises a cap loads the run doc at its top and only knows
// the caps after the compile — a window in which another writer (an
// operator cancel, the run finishing under a still-live pod, the sweeper)
// can move the doc. The snapshot write must land on the CURRENT doc:
// a whole-document SaveRun from the copy loaded at the top reverts the
// concurrent transition, and on the cloud path the reverted doc then
// passes SubmitResume's status CAS as if nothing had happened.

// seedResumableRun persists a paused_operator run whose hash matches
// fillerBotSrc, with a checkpoint so a resume has somewhere to restart.
func seedResumableRun(t *testing.T, st store.RunStore, runID string) {
	t.Helper()
	_, hash, err := compileForLaunch("", fillerBotSrc, "")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := st.SaveRun(context.Background(), &store.Run{
		FormatVersion: store.RunFormatVersion,
		ID:            runID,
		WorkflowName:  "main",
		WorkflowHash:  hash,
		FilePath:      "/opt/iterion/bots/stored-bot/main.bot",
		Status:        store.RunStatusPausedOperator,
		Checkpoint:    &store.Checkpoint{NodeID: "work"},
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// Cloud path: an operator cancel that lands between the resume's load and
// its budget write must survive it, typed cause included.
func TestResume_CloudPathBudgetWriteDoesNotRevertAConcurrentCancel(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const runID = "run-cancel-under-resume"
	seedResumableRun(t, st, runID)

	svc, err := NewService(dir,
		WithLogger(iterlog.Nop()),
		WithStore(st),
		WithLaunchPublisher(&stubLaunchPublisher{}),
		WithResumeSourceFiller(func(_ context.Context, run *store.Run, spec *ResumeSpec) (func(), error) {
			spec.Source = fillerBotSrc
			// The cancel lands after the resume loaded its copy of the doc.
			if err := st.UpdateRunStatusCoded(ctx, run.ID, store.RunStatusCancelled, "cancelled by the operator", store.FailureCancelled); err != nil {
				t.Fatalf("inject cancel: %v", err)
			}
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.Resume(ctx, ResumeSpec{
		RunID:    runID,
		FilePath: "/opt/iterion/bots/stored-bot/main.bot",
		Budget:   &ir.BudgetOverrides{MaxCostUSD: 120},
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	got, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Status != store.RunStatusCancelled || got.FailureCode != store.FailureCancelled {
		t.Fatalf("doc after the resume = %s/%q, want cancelled/CANCELLED — the budget write reverted the operator's cancel from a stale copy (and on cloud that reverted doc passes SubmitResume's CAS)", got.Status, got.FailureCode)
	}
}

// In-process path: a run that FINISHED between the resume's load and its
// budget write must not be resurrected into a second execution.
func TestResume_InProcessBudgetWriteDoesNotResurrectAFinishedRun(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir, store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const runID = "run-finished-under-resume"
	seedResumableRun(t, st, runID)

	t.Setenv(envDetached, "0")
	svc, err := NewService(dir,
		WithLogger(iterlog.Nop()),
		WithStore(st),
		WithResumeSourceFiller(func(_ context.Context, run *store.Run, spec *ResumeSpec) (func(), error) {
			spec.Source = fillerBotSrc
			if err := st.UpdateRunStatus(ctx, run.ID, store.RunStatusFinished, ""); err != nil {
				t.Fatalf("inject finish: %v", err)
			}
			return nil, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Resume(ctx, ResumeSpec{
		RunID:    runID,
		FilePath: "/opt/iterion/bots/stored-bot/main.bot",
		Budget:   &ir.BudgetOverrides{MaxCostUSD: 120},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case <-res.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("resume did not settle")
	}

	events, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, e := range events {
		if e.Type == store.EventRunResumed {
			t.Fatalf("a finished run was resumed (run_resumed on the timeline) — the budget write from the stale copy put it back to paused_operator and the under-lock re-validation accepted the resurrected doc")
		}
	}
	got, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Status != store.RunStatusFinished {
		t.Fatalf("doc after the refused resume = %s, want finished untouched", got.Status)
	}
}
