package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// snapshotBudgetForPersist projects the EFFECTIVE ir.Budget onto the
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
func snapshotBudgetForPersist(b *ir.Budget) *store.RunBudget {
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
