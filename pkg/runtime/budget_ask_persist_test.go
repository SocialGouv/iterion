package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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
