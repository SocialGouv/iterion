package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
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

// RunBudgetOverridesOf is the inverse of BudgetOverridesFromRun: the ask
// in its persisted form. Nil (or all-zero) in, nil out, so an ask-less
// launch persists nothing and the doc stays byte-identical.
func RunBudgetOverridesOf(o *ir.BudgetOverrides) *store.RunBudgetOverrides {
	if o == nil || o.IsZero() {
		return nil
	}
	return &store.RunBudgetOverrides{
		MaxCostUSD:          o.MaxCostUSD,
		MaxTokens:           o.MaxTokens,
		MaxDuration:         o.MaxDuration,
		MaxIterations:       o.MaxIterations,
		MaxParallelBranches: o.MaxParallelBranches,
	}
}

// BudgetOverridesFromWire lifts the queue's wire mirror of a budget ask
// back into the override shape — the one converter in that direction,
// shared by the runner (which applies the wire to the workflow it runs)
// and the publisher (which stamps the doc from the very figure it put on
// the wire). Nil in, nil out.
func BudgetOverridesFromWire(b *queue.BudgetOverrides) *ir.BudgetOverrides {
	if b == nil {
		return nil
	}
	return &ir.BudgetOverrides{
		MaxCostUSD:          b.MaxCostUSD,
		MaxTokens:           b.MaxTokens,
		MaxDuration:         b.MaxDuration,
		MaxIterations:       b.MaxIterations,
		MaxParallelBranches: b.MaxParallelBranches,
		CapImposed:          b.CapImposed,
	}
}

// MergeResumeBudgetAsk MERGES a THIS-RESUME override (the CLI/API budget
// flags on the resume request) OVER the launch ask persisted on the run
// doc, per field — the "non-zero wins, zero inherits" rule
// ApplyBudgetOverrides enforces on the executor side, so `--max-duration
// 4h` alone raises the duration without erasing the launch's cost/token
// caps. The one merge, on every resume surface (in-process, detached,
// the CLI, the cloud publisher), so the caps a resumed run executes
// against cannot depend on which door it came through. Nil when neither
// side carries an ask.
func MergeResumeBudgetAsk(fromSpec *ir.BudgetOverrides, fromDoc *store.RunBudgetOverrides) *ir.BudgetOverrides {
	base := BudgetOverridesFromRun(fromDoc)
	if fromSpec == nil || fromSpec.IsZero() {
		return base
	}
	if base == nil {
		base = &ir.BudgetOverrides{}
	}
	if fromSpec.MaxCostUSD > 0 {
		base.MaxCostUSD = fromSpec.MaxCostUSD
	}
	if fromSpec.MaxTokens > 0 {
		base.MaxTokens = fromSpec.MaxTokens
	}
	if fromSpec.MaxDuration != "" {
		base.MaxDuration = fromSpec.MaxDuration
	}
	if fromSpec.MaxIterations > 0 {
		base.MaxIterations = fromSpec.MaxIterations
	}
	if fromSpec.MaxParallelBranches > 0 {
		base.MaxParallelBranches = fromSpec.MaxParallelBranches
	}
	if fromSpec.CapImposed {
		base.CapImposed = true
	}
	return base
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
