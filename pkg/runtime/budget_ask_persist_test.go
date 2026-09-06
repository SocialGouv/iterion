package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The imposed-cap marker alone persists as nothing, not as an empty
// object: it only ever travels next to the cap it marks.
func TestRunBudgetOverridesOf_CapImposedAloneIsNothing(t *testing.T) {
	if got := RunBudgetOverridesOf(&ir.BudgetOverrides{CapImposed: true}); got != nil {
		t.Fatalf("RunBudgetOverridesOf(CapImposed only) = %+v, want nil (it persisted as an empty budget_overrides: {} on the doc)", got)
	}
	if got := RunBudgetOverridesOf(&ir.BudgetOverrides{MaxCostUSD: 5, CapImposed: true}); got == nil || got.MaxCostUSD != 5 {
		t.Fatalf("RunBudgetOverridesOf(clamped cap) = %+v, want the cap persisted", got)
	}
}

// #718: the doc's EFFECTIVE-caps snapshot (Run.Budget — the studio
// meter's denominator and what `iterion remote runs get` prints) was
// stamped from the post-clamp workflow at LAUNCH only. A resumed attempt
// re-runs the whole resolution on a fresh workflow — the launch ask
// replayed from the doc, this resume's ask merged over it, then the
// PLATFORM ceiling (ITERION_CLOUD_MAX_*) clamped on the pod, which no
// publisher can see — and nothing re-stamped: a run resumed with
// --max-cost-usd 120 under a $50 platform ceiling dies at $50 while its
// doc still reads 120.
func TestResume_StampsThePostClampBudgetOnTheDoc(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-resume-budget-restamp"
	if _, err := s.CreateRun(ctx, runID, "resume_budget", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// The doc as the publisher left it: the merged ask, stamped before
	// any pod could apply the ceiling that lives in the runner's env.
	if err := s.SetRunBudgetSnapshot(ctx, runID, &store.RunBudget{MaxCostUSD: 120, MaxTokens: 9000}); err != nil {
		t.Fatalf("SetRunBudgetSnapshot: %v", err)
	}
	cp := &store.Checkpoint{NodeID: "done"}
	if err := s.FailRunResumable(ctx, runID, cp, "usage window", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// Keep the resume's skill/plugin mirroring inside a scratch dir.
	r.WorkDir = t.TempDir()
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	// The workflow the pod resumes on: the ask, then the platform
	// ceiling — exactly what applyCloudBudgetCeiling leaves behind.
	wf := &ir.Workflow{
		Name:   "resume_budget",
		Entry:  "done",
		Nodes:  map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Budget: &ir.Budget{MaxCostUSD: 120, MaxTokens: 9000},
	}
	wf.Budget.ClampToCeiling(&ir.Budget{MaxCostUSD: 50})
	if !wf.Budget.CapImposed {
		t.Fatal("the clamp did not mark the budget imposed — the premise of this test no longer holds")
	}

	if err := New(wf, s, newStubExecutor()).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after resume: %v", err)
	}
	if got.Budget == nil {
		t.Fatal("run.Budget = nil after resume — the launch snapshot was erased instead of refreshed")
	}
	if got.Budget.MaxCostUSD != 50 {
		t.Fatalf("run.Budget.MaxCostUSD = %v after a resume clamped to a $50 platform ceiling, want 50 — the doc advertises a cap the pod will not honour", got.Budget.MaxCostUSD)
	}
	if got.Budget.MaxTokens != 9000 {
		t.Fatalf("run.Budget.MaxTokens = %d, want 9000 (an axis the ceiling does not bound must survive the re-stamp)", got.Budget.MaxTokens)
	}
}

// A resume whose workflow declares no budget at all (a --force resume of
// an edited .bot that dropped the block) must PRESERVE the prior
// snapshot rather than erase it — the same non-nil guard the launch
// stamp carries in runResolveDoc.
func TestResume_BudgetlessWorkflowKeepsThePriorSnapshot(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-resume-budget-none"
	if _, err := s.CreateRun(ctx, runID, "resume_budget", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.SetRunBudgetSnapshot(ctx, runID, &store.RunBudget{MaxCostUSD: 42}); err != nil {
		t.Fatalf("SetRunBudgetSnapshot: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &store.Checkpoint{NodeID: "done"}, "boom", ""); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.WorkDir = t.TempDir()
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	wf := &ir.Workflow{
		Name:  "resume_budget",
		Entry: "done",
		Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
	}
	if err := New(wf, s, newStubExecutor()).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after resume: %v", err)
	}
	if got.Budget == nil || got.Budget.MaxCostUSD != 42 {
		t.Fatalf("run.Budget = %+v after a budgetless resume, want the prior $42 snapshot preserved", got.Budget)
	}
}
