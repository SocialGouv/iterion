package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// snapshotBudgetForPersist projects the EFFECTIVE ir.Budget onto the
// display-only store.RunBudget persisted on the run. The effective budget
// is what the engine holds by the time the run doc is stamped: the .bot's
// budget: block after recipe/preset/CLI overrides (applyBudgetOverrides
// mutates wf.Budget in place) and, in cloud, the platform ceiling clamp
// (applyCloudBudgetCeiling clamps wf.Budget before the engine is built).
// So a single snapshot at runResolveDoc captures the caps SharedBudget
// actually enforces on both the local and cloud paths.
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
		MaxIterations:       b.MaxIterations,
		MaxDuration:         ir.ExpandEnvWithDefault(b.MaxDuration),
		MaxParallelBranches: b.MaxParallelBranches,
	}
}
