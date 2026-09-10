package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestApplyBudgetOverrides pins the launch-override contract on the cloud
// runner: non-zero override wins, zero inherits the DSL budget, nil is a
// no-op, and a malformed duration fails the run instead of silently
// running without the requested cap.
func TestApplyBudgetOverrides(t *testing.T) {
	t.Run("override wins, zero inherits", func(t *testing.T) {
		wf := &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 60, MaxTokens: 5000}}
		err := applyBudgetOverrides(wf, &queue.BudgetOverrides{MaxCostUSD: 120, MaxDuration: "4h"}, iterlog.Nop())
		if err != nil {
			t.Fatalf("applyBudgetOverrides: %v", err)
		}
		if wf.Budget.MaxCostUSD != 120 {
			t.Errorf("MaxCostUSD = %v, want 120 (override wins)", wf.Budget.MaxCostUSD)
		}
		if wf.Budget.MaxDuration != "4h" {
			t.Errorf("MaxDuration = %q, want 4h", wf.Budget.MaxDuration)
		}
		if wf.Budget.MaxTokens != 5000 {
			t.Errorf("MaxTokens = %d, want 5000 (zero override inherits)", wf.Budget.MaxTokens)
		}
	})

	t.Run("nil override is a no-op", func(t *testing.T) {
		wf := &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 60}}
		if err := applyBudgetOverrides(wf, nil, iterlog.Nop()); err != nil {
			t.Fatalf("applyBudgetOverrides(nil): %v", err)
		}
		if wf.Budget.MaxCostUSD != 60 {
			t.Errorf("MaxCostUSD = %v, want untouched 60", wf.Budget.MaxCostUSD)
		}
	})

	t.Run("unbudgeted workflow gets the override", func(t *testing.T) {
		wf := &ir.Workflow{}
		if err := applyBudgetOverrides(wf, &queue.BudgetOverrides{MaxTokens: 9000}, iterlog.Nop()); err != nil {
			t.Fatalf("applyBudgetOverrides: %v", err)
		}
		if wf.Budget == nil || wf.Budget.MaxTokens != 9000 {
			t.Errorf("Budget = %+v, want MaxTokens 9000", wf.Budget)
		}
	})

	t.Run("malformed duration fails the run", func(t *testing.T) {
		wf := &ir.Workflow{}
		err := applyBudgetOverrides(wf, &queue.BudgetOverrides{MaxDuration: "4 hours"}, iterlog.Nop())
		if err == nil || !strings.Contains(err.Error(), "max_duration") {
			t.Errorf("err = %v, want max_duration validation error", err)
		}
	})

	t.Run("cloud ceiling still clamps an override", func(t *testing.T) {
		t.Setenv("ITERION_CLOUD_MAX_COST_USD", "100")
		wf := &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: 60}}
		if err := applyBudgetOverrides(wf, &queue.BudgetOverrides{MaxCostUSD: 500}, iterlog.Nop()); err != nil {
			t.Fatalf("applyBudgetOverrides: %v", err)
		}
		applyCloudBudgetCeiling(wf, iterlog.Nop())
		if wf.Budget.MaxCostUSD != 100 {
			t.Errorf("MaxCostUSD = %v, want 100 (ceiling clamps the tenant override)", wf.Budget.MaxCostUSD)
		}
	})
}

// #718, driven through the runner's OWN budget resolution and a real
// engine resume: the publisher stamps the doc from the merged ask
// ($120) because the platform ceiling lives in the runner pod's
// environment, not the server's. The pod then clamps to $50 and runs
// against that. Whatever the runner hands the engine is what the doc
// must say — otherwise the studio meter and `iterion remote runs get`
// advertise a cap the attempt cannot spend.
func TestExecuteRunBudget_ResumeReStampsTheDocAfterTheCloudCeiling(t *testing.T) {
	t.Setenv("ITERION_CLOUD_MAX_COST_USD", "50")
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const runID = "run-resume-ceiling"
	if _, err := st.CreateRun(ctx, runID, "ceiling_probe", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// What the publisher left behind: the merged ask, unaware of the pod's
	// ceiling.
	if err := st.SetRunBudgetSnapshot(ctx, runID, &store.RunBudget{MaxCostUSD: 120}); err != nil {
		t.Fatalf("SetRunBudgetSnapshot: %v", err)
	}
	if err := st.FailRunResumable(ctx, runID, &store.Checkpoint{NodeID: "done"}, "usage window", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.WorkDir = t.TempDir()
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	// The pod's own resolution, in the order executeRun performs it.
	wf := &ir.Workflow{
		Name:   "ceiling_probe",
		Entry:  "done",
		Nodes:  map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Budget: &ir.Budget{MaxCostUSD: 60},
	}
	if err := applyBudgetOverrides(wf, &queue.BudgetOverrides{MaxCostUSD: 120}, iterlog.Nop()); err != nil {
		t.Fatalf("applyBudgetOverrides: %v", err)
	}
	applyCloudBudgetCeiling(wf, iterlog.Nop())

	if err := runtime.New(wf, st, unusedExecutor{}).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after resume: %v", err)
	}
	if got.Budget == nil || got.Budget.MaxCostUSD != 50 {
		t.Fatalf("run.Budget = %+v after a resume under a $50 platform ceiling, want MaxCostUSD 50 — the doc over-reports by the clamp margin", got.Budget)
	}
}

// unusedExecutor satisfies NodeExecutor for a workflow whose only node is
// terminal: reaching Execute would mean the resume ran something.
type unusedExecutor struct{}

func (unusedExecutor) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, errors.New("no node of this workflow should execute")
}
