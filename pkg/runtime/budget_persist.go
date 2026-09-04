package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// SnapshotBudgetForPersist projects the EFFECTIVE ir.Budget onto the
// display-only store.RunBudget persisted on the run. The effective budget
// is simply what `e.workflow.Budget` holds by the time the run doc is
// stamped at runResolveDoc: applyBudgetOverrides (CLI) and
// applyCloudBudgetCeiling (cloud) mutate wf.Budget in place before the
// engine is built, and recipe.Apply hands back a workflow copy whose
// Budget is the merged result — so whichever path the caller took, one
// snapshot here captures the caps SharedBudget actually enforces on both
// the local and cloud paths.
//
// Returns nil for a nil budget so a run without a budget: block persists
// no cap and the studio Overview degrades to bare stats. MaxDuration is
// resolved through ExpandEnvWithDefault so a "${DUR:-30m}" source persists
// as the concrete "30m" the frontend parses.
func SnapshotBudgetForPersist(b *ir.Budget) *store.RunBudget {
	if b == nil {
		return nil
	}
	return &store.RunBudget{
		MaxCostUSD:          b.MaxCostUSD,
		MaxTokens:           b.MaxTokens,
		WarnTokens:          b.WarnTokens,
		MaxIterations:       b.MaxIterations,
		MaxDuration:         ir.ExpandEnvWithDefault(b.MaxDuration),
		MaxParallelBranches: b.MaxParallelBranches,
	}
}

// BudgetOverridesFromRun lifts the budget ask persisted on a run doc
// back into the override shape ApplyBudgetOverrides consumes — the
// resume path's replay source, on every surface (in-process, detached,
// cloud publish). Nil in, nil out. The one converter, so the doc→wire
// and doc→engine readings of the ask cannot drift.
func BudgetOverridesFromRun(o *store.RunBudgetOverrides) *ir.BudgetOverrides {
	if o == nil {
		return nil
	}
	return &ir.BudgetOverrides{
		MaxCostUSD:          o.MaxCostUSD,
		MaxTokens:           o.MaxTokens,
		MaxDuration:         o.MaxDuration,
		MaxIterations:       o.MaxIterations,
		MaxParallelBranches: o.MaxParallelBranches,
	}
}

// EffectiveBudgetSnapshot projects what a run's caps become once ask is
// applied over the workflow's declared budget — the same "non-zero
// wins, zero inherits" merge ApplyBudgetOverrides performs on the
// executor side — WITHOUT mutating wf: a publisher stamping the doc from
// the merged ask must not edit the workflow it was handed. Nil when
// neither the workflow nor the ask carries a cap.
func EffectiveBudgetSnapshot(wf *ir.Workflow, ask *ir.BudgetOverrides) *store.RunBudget {
	var eff ir.Budget
	if wf != nil && wf.Budget != nil {
		eff = *wf.Budget
	}
	scratch := &ir.Workflow{Budget: &eff}
	if ask != nil && !ask.IsZero() {
		ir.ApplyBudgetOverrides(scratch, *ask)
	}
	if *scratch.Budget == (ir.Budget{}) {
		return nil
	}
	return SnapshotBudgetForPersist(scratch.Budget)
}
