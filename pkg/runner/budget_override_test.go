package runner

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
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
