package cli

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// BudgetOverrides aliases ir.BudgetOverrides so existing CLI callers
// (RunOptions.Budget, ResumeOptions.Budget, cmd/iterion flag wiring) keep
// compiling unchanged. The canonical type + apply logic live in
// pkg/dsl/ir/budget_override.go, shared with the server launch path.
type BudgetOverrides = ir.BudgetOverrides

// applyBudgetOverrides delegates to ir.ApplyBudgetOverrides. See the ir
// package for the ordering contract (after recipe/preset resolution,
// before the executor snapshots Budget).
func applyBudgetOverrides(wf *ir.Workflow, o BudgetOverrides) {
	ir.ApplyBudgetOverrides(wf, o)
}
