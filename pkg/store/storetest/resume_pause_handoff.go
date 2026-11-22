package storetest

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type handoffDecorator struct{ store.RunStore }

func (s handoffDecorator) Unwrap() store.RunStore { return s.RunStore }

func testResumePauseHandoff(t *testing.T, s store.RunStore) {
	t.Helper()
	ctx := testCtx()
	const runID = "handoff"
	wrapped := handoffDecorator{s}
	handoff := store.AsResumePauseHandoffStore(wrapped)
	if handoff == nil {
		t.Fatal("wrapped store hides the handoff capability")
	}
	load := func() *store.Run {
		t.Helper()
		r, err := s.LoadRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	won := func(changed bool, err error) {
		t.Helper()
		if err != nil || !changed {
			t.Fatalf("expected successful handoff mutation, got %v, %v", changed, err)
		}
	}
	lost := func(changed bool, err error) {
		t.Helper()
		if err != nil || changed {
			t.Fatalf("expected unchanged handoff, got %v, %v", changed, err)
		}
	}
	assertHandoff := func(r *store.Run, status store.RunStatus, id, node string) {
		t.Helper()
		if r.Status != status || r.Checkpoint == nil || r.Checkpoint.NodeID != node || r.Checkpoint.InteractionID != "" || len(r.Checkpoint.InteractionQuestions) != 0 {
			t.Fatalf("status=%s checkpoint=%+v, want %s at %s without pause pointer", r.Status, r.Checkpoint, status, node)
		}
		if id == "" {
			if r.ResumePauseHandoff != nil {
				t.Fatalf("retired handoff persisted: %+v", r.ResumePauseHandoff)
			}
		} else if h := r.ResumePauseHandoff; h == nil || h.ID != id || h.NodeID != "gate" || h.InteractionID != "question" || h.InteractionQuestions["confirm"] != "Continue?" {
			t.Fatalf("handoff = %+v, want %s and original pause identity", h, id)
		}
		if r.CodemodeBoundsOverride == nil || r.CodemodeBoundsOverride.StoreEntries != 23 {
			t.Fatalf("quota not preserved: %+v", r.CodemodeBoundsOverride)
		}
	}
	claim := func(id string) {
		t.Helper()
		questions := map[string]any{"confirm": "Continue?"}
		must(s.PauseRun(ctx, runID, &store.Checkpoint{NodeID: "gate", InteractionID: "question", InteractionQuestions: questions}))
		r := load()
		claimer := store.AsCodemodeQuotaResumeClaimer(wrapped)
		if claimer == nil {
			t.Fatal("wrapped store hides the quota claim capability")
		}
		quota, err := claimer.ClaimResumeWithCodemodeQuota(ctx, runID, store.CodemodeQuotaResumeClaim{
			ExpectedStatus: r.Status, ExpectedVersion: r.CASVersion,
			PauseHandoff: &store.ResumePauseHandoff{ID: id, NodeID: "gate", InteractionID: "question", InteractionQuestions: questions},
		}, store.CodemodeScratchLimits{Entries: 23})
		must(err)
		if quota == nil {
			t.Fatal("lost initial pause claim")
		}
	}
	_, err := s.CreateRun(ctx, runID, "w", nil)
	must(err)
	claim("first")
	assertHandoff(load(), store.RunStatusRunning, "first", "gate")

	// Metadata saves carry no authority over the persisted handoff.
	copy := load()
	copy.ResumePauseHandoff = nil
	must(s.SaveRun(ctx, copy))
	assertHandoff(load(), store.RunStatusRunning, "first", "gate")

	cp := &store.Checkpoint{NodeID: "next", Outputs: map[string]map[string]any{"gate": {"result": "$literal-value"}}}
	lost(handoff.FinishResumePauseHandoff(ctx, runID, "wrong", cp))
	lost(handoff.ParkResumePauseHandoff(ctx, runID, "wrong", "wrong owner", nil))
	assertHandoff(load(), store.RunStatusRunning, "first", "gate")

	won(handoff.ParkResumePauseHandoff(ctx, runID, "first", "$operator retry", &store.ResumePauseBudget{BudgetCostUSD: 2, BudgetTokensUsed: 40}))
	parked := load()
	assertHandoff(parked, store.RunStatusFailedResumable, "first", "gate")
	if parked.Checkpoint.BudgetCostUSD != 2 || parked.Checkpoint.BudgetTokensUsed != 40 {
		t.Fatal("park lost booked spend")
	}
	if parked.FinishedAt == nil || parked.Error != "$operator retry" || parked.FailureCode != store.FailureInterrupted || parked.OutcomeSeq != 1 {
		t.Fatalf("unexpected park outcome: finished_at=%v error=%q outcome_seq=%d", parked.FinishedAt, parked.Error, parked.OutcomeSeq)
	}
	lost(handoff.FinishResumePauseHandoff(ctx, runID, "first", cp))
	lost(handoff.ParkResumePauseHandoff(ctx, runID, "first", "already parked", nil))
	lost(handoff.ReclaimResumePauseHandoff(ctx, runID, "first", "second", parked.Status, parked.CASVersion-1, time.Time{}))
	lost(handoff.ReclaimResumePauseHandoff(ctx, runID, "wrong", "second", parked.Status, parked.CASVersion, time.Time{}))
	won(handoff.ReclaimResumePauseHandoff(ctx, runID, "first", "second", parked.Status, parked.CASVersion, time.Time{}))
	assertHandoff(load(), store.RunStatusRunning, "second", "gate")
	lost(handoff.FinishResumePauseHandoff(ctx, runID, "first", cp))
	lost(handoff.ParkResumePauseHandoff(ctx, runID, "first", "stale owner", nil))

	won(handoff.ParkResumePauseHandoff(ctx, runID, "second", "queue retry", nil))
	must(s.UpdateRunStatus(ctx, runID, store.RunStatusQueued, ""))
	queued := load()
	if queued.QueuedAt == nil {
		t.Fatal("queued run has no attempt timestamp")
	}
	lost(handoff.ReclaimResumePauseHandoff(ctx, runID, "second", "third", queued.Status, queued.CASVersion, queued.QueuedAt.Add(time.Millisecond)))
	lost(handoff.ReclaimResumePauseHandoff(ctx, runID, "second", "third", queued.Status, queued.CASVersion-1, *queued.QueuedAt))
	const nextEpoch = "test-handoff-acquisition"
	wrongEpoch := store.WithResumeExecutionClaim(ctx, store.ResumeExecutionClaim{ExpectedEpoch: "wrong-epoch", NextEpoch: nextEpoch})
	lost(handoff.ReclaimResumePauseHandoff(wrongEpoch, runID, "second", "third", queued.Status, queued.CASVersion, *queued.QueuedAt))
	if untouched := load(); untouched.CASVersion != queued.CASVersion || untouched.ExecutionEpoch != queued.ExecutionEpoch {
		t.Fatal("wrong epoch changed the queued run")
	}
	claimCtx := store.WithResumeExecutionClaim(ctx, store.ResumeExecutionClaim{ExpectedEpoch: queued.ExecutionEpoch, NextEpoch: nextEpoch})
	won(handoff.ReclaimResumePauseHandoff(claimCtx, runID, "second", "third", queued.Status, queued.CASVersion, *queued.QueuedAt))
	stale := load()
	assertHandoff(stale, store.RunStatusRunning, "third", "gate")
	if stale.ExecutionEpoch != nextEpoch || stale.CASVersion != queued.CASVersion+1 {
		t.Fatal("reclaim did not commit the caller's execution epoch in its mutation")
	}
	won(handoff.FinishResumePauseHandoff(ctx, runID, "third", cp))
	finished := load()
	assertHandoff(finished, store.RunStatusRunning, "", "next")
	if finished.CASVersion != stale.CASVersion+1 || finished.Checkpoint.Outputs["gate"]["result"] != "$literal-value" {
		t.Fatalf("checkpoint not committed in retirement write: version=%d previous=%d checkpoint=%+v", finished.CASVersion, stale.CASVersion, finished.Checkpoint)
	}
	lost(handoff.FinishResumePauseHandoff(ctx, runID, "third", nil))
	finished.ResumePauseHandoff = stale.ResumePauseHandoff
	must(s.SaveRun(ctx, finished))
	assertHandoff(load(), store.RunStatusRunning, "", "next")

	claim("no-checkpoint-replacement")
	won(handoff.FinishResumePauseHandoff(ctx, runID, "no-checkpoint-replacement", nil))
	assertHandoff(load(), store.RunStatusRunning, "", "gate")

	claim("replace")
	replacement := &store.Checkpoint{NodeID: "gate", InteractionID: "new-question", InteractionQuestions: map[string]any{"confirm": "Continue again?"}}
	lost(handoff.ReplaceResumePauseHandoff(ctx, runID, "wrong", replacement))
	lost(handoff.ReplaceResumePauseHandoff(ctx, runID, "replace", &store.Checkpoint{NodeID: "gate", InteractionID: "question"}))
	beforeReplace := load()
	assertHandoff(beforeReplace, store.RunStatusRunning, "replace", "gate")
	won(handoff.ReplaceResumePauseHandoff(ctx, runID, "replace", replacement))
	replaced := load()
	if replaced.Status != store.RunStatusPausedWaitingHuman || replaced.ResumePauseHandoff != nil || replaced.Checkpoint == nil || replaced.Checkpoint.InteractionID != "new-question" || replaced.CASVersion != beforeReplace.CASVersion+1 {
		t.Fatal("replacing a handoff did not publish the new pause atomically")
	}
	lost(handoff.ReplaceResumePauseHandoff(ctx, runID, "replace", replacement))

	claim("cancel")
	must(s.UpdateRunStatus(ctx, runID, store.RunStatusCancelled, "cancelled"))
	assertHandoff(load(), store.RunStatusCancelled, "", "gate")
	lost(handoff.FinishResumePauseHandoff(ctx, runID, "cancel", cp))

	claim("rewind")
	won(handoff.ParkResumePauseHandoff(ctx, runID, "rewind", "park before rewind", nil))
	rewind := load()
	rewind.Status = store.RunStatusCancelled
	must(s.SaveRun(ctx, rewind))
	assertHandoff(load(), store.RunStatusCancelled, "", "gate")

	claim("delete")
	must(s.DeleteRun(ctx, runID))
	lost(handoff.FinishResumePauseHandoff(ctx, runID, "delete", cp))
	lost(handoff.ParkResumePauseHandoff(ctx, runID, "delete", "stale after deletion", nil))
}
