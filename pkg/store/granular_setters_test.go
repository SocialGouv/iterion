package store

import (
	"context"
	"testing"
)

// TestStaleSaveRunRevertsATransition pins the hazard the granular
// setters exist to avoid, so the reason for their shape is executable
// rather than only written down. SaveRun replaces the whole document
// from the caller's copy; when a status transition lands between the
// load and the save, that copy is stale and the transition is undone.
//
// This is the documented, deliberate SaveRun read-modify-write hazard
// (a version CAS is the real fix — follow-up), NOT a defect of this
// test's subject: it is the baseline that makes
// TestSetRunBudgetOverridesDoesNotReplaceTheDocument meaningful.
func TestStaleSaveRunRevertsATransition(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()

	if _, err := s.CreateRun(ctx, "r", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, "r", RunStatusQueued, ""); err != nil {
		t.Fatalf("to queued: %v", err)
	}

	stale, err := s.LoadRun(ctx, "r")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}

	// The operator cancels while the caller holds its copy.
	ok, err := s.UpdateRunStatusIf(ctx, "r", RunStatusCancelled, "operator cancel", []RunStatus{RunStatusQueued})
	if err != nil || !ok {
		t.Fatalf("cancel CAS: ok=%v err=%v", ok, err)
	}

	stale.BudgetOverrides = &RunBudgetOverrides{MaxCostUSD: 120}
	if err := s.SaveRun(ctx, stale); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	got, err := s.LoadRun(ctx, "r")
	if err != nil {
		t.Fatalf("LoadRun after: %v", err)
	}
	if got.Status != RunStatusCancelled {
		t.Logf("whole-doc SaveRun from a stale copy reverted %s -> %s (error %q lost)",
			RunStatusCancelled, got.Status, "operator cancel")
	} else {
		t.Fatal("SaveRun no longer replaces the whole document from the caller's copy — " +
			"the granular setters' rationale changed; revisit them and this test")
	}
}

// TestSetRunBudgetOverridesDoesNotReplaceTheDocument asserts the
// granular contract iface.go declares for the budget-ask setter: it
// touches budget_overrides + updated_at and nothing else.
//
// What this test actually catches, stated plainly so it is not read as
// proving more than it does: the load-modify-SaveRun form fails the
// updated_at half outright (SaveRun stamps no timestamp of its own, so
// the field-scoped write left the doc's mtime at its previous value
// while the Mongo twin's $set advances it). The peer-field assertions
// below are defence in depth — SaveRun's own bookkeeping happens to
// shield most of them on a SERIAL call, so they would only fire if that
// bookkeeping regressed. The half no in-process test can schedule
// reliably is the timed one: a transition landing inside the
// load-save window, whose consequence is pinned by
// TestStaleSaveRunRevertsATransition above.
func TestSetRunBudgetOverridesDoesNotReplaceTheDocument(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()

	if _, err := s.CreateRun(ctx, "r", "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// A parked run carries a failure code and a checkpoint; the resume
	// raises the cap on exactly this shape.
	if err := s.UpdateRunStatusCoded(ctx, "r", RunStatusFailedResumable, "budget exceeded", FailureBudgetExceeded); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := s.SaveCheckpoint(ctx, "r", &Checkpoint{NodeID: "implement"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	before, err := s.LoadRun(ctx, "r")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}

	if err := s.SetRunBudgetOverrides(ctx, "r", &RunBudgetOverrides{MaxCostUSD: 120, MaxDuration: "4h"}); err != nil {
		t.Fatalf("SetRunBudgetOverrides: %v", err)
	}

	got, err := s.LoadRun(ctx, "r")
	if err != nil {
		t.Fatalf("LoadRun after: %v", err)
	}
	if got.BudgetOverrides == nil || got.BudgetOverrides.MaxCostUSD != 120 || got.BudgetOverrides.MaxDuration != "4h" {
		t.Fatalf("BudgetOverrides = %+v, want the raised ask", got.BudgetOverrides)
	}
	// Every peer field the whole-document path would have re-derived.
	if got.Status != before.Status {
		t.Errorf("Status = %s, want %s untouched", got.Status, before.Status)
	}
	if got.FailureCode != before.FailureCode {
		t.Errorf("FailureCode = %q, want %q untouched", got.FailureCode, before.FailureCode)
	}
	if got.Error != before.Error {
		t.Errorf("Error = %q, want %q untouched", got.Error, before.Error)
	}
	if got.OutcomeSeq != before.OutcomeSeq {
		t.Errorf("OutcomeSeq = %d, want %d untouched (a whole-doc write re-runs the outcome bookkeeping)", got.OutcomeSeq, before.OutcomeSeq)
	}
	if got.Checkpoint == nil || before.Checkpoint == nil || got.Checkpoint.NodeID != before.Checkpoint.NodeID {
		t.Errorf("Checkpoint = %+v, want %+v untouched", got.Checkpoint, before.Checkpoint)
	}
	if !got.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("UpdatedAt = %s, want it advanced past %s", got.UpdatedAt, before.UpdatedAt)
	}
}
