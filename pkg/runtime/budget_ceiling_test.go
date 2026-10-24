package runtime

import (
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// W1/W2: remove ceiling recording, wire conversion, or the grace minimum to
// make the at-ceiling and $9.50/$10 boundary rows fail respectively.
func TestBudgetGraceStopsAtAbsoluteCeiling(t *testing.T) {
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "0.1")
	t.Setenv("ITERION_LOOP_BUDGET_GUARD", "on")
	for _, source := range []string{"platform", "donor"} {
		for _, cap := range []float64{9.5, 10} {
			t.Run(fmt.Sprintf("%s/%.2f", source, cap), func(t *testing.T) {
				wf := &ir.Workflow{Budget: &ir.Budget{MaxCostUSD: cap}}
				if source == "donor" {
					ir.ApplyBudgetOverrides(wf, *BudgetOverridesFromWire(&queue.BudgetOverrides{MaxCostUSD: cap, CapImposed: true, DonorMaxCostUSD: 10}))
				} else {
					wf.Budget.ClampToCeiling(&ir.Budget{MaxCostUSD: 10})
				}
				if !wf.Budget.CapImposed {
					t.Fatal("existing ceiling is not marked")
				}
				b := newSharedBudget(wf.Budget, nil)
				e := &Engine{}
				b.costUsed = 9.99
				if _, ok := e.withinSharedBudgetGrace(b); ok != (cap < 10) {
					t.Fatalf("grace just below $10 = %v, cap %v", ok, cap)
				}
				b.costUsed = 10
				if _, ok := e.withinSharedBudgetGrace(b); ok {
					t.Fatal("grace admits spend at the donor/platform ceiling")
				}
			})
		}
	}
	// The wall-clock deadline uses the same minimum, even below the ceiling.
	for _, cap := range []time.Duration{2 * time.Hour, 115 * time.Minute} {
		spec := &ir.Budget{MaxDuration: cap.String()}
		spec.ClampToCeiling(&ir.Budget{MaxDuration: "2h"})
		b := newSharedBudget(spec, nil)
		now := time.Unix(0, 0)
		b.useClock(func() time.Time { return now })
		remaining, bounded := b.GracedRemainingDuration(.1)
		if !bounded || remaining != 2*time.Hour {
			t.Fatalf("graced duration = %s, bounded %v", remaining, bounded)
		}
		now = now.Add(2 * time.Hour)
		if _, ok := (&Engine{}).withinSharedBudgetGrace(b); ok {
			t.Fatal("2h ceiling grants 2h12")
		}
	}
}

func TestBudgetCeilingsComposeWithoutRemovingOtherGrace(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		spec := &ir.Budget{MaxCostUSD: 7.5, MaxDuration: "1h"}
		ceilings := []*ir.Budget{{MaxCostUSD: 10}, {MaxCostUSD: 8, MaxDuration: "2h"}}
		if reverse {
			ceilings[0], ceilings[1] = ceilings[1], ceilings[0]
		}
		for _, c := range ceilings {
			spec.ClampToCeiling(c)
		}
		b := newSharedBudget(spec, nil)
		b.costUsed = 7.99
		if _, ok := b.exitGraceRoom(.1); !ok {
			t.Fatal("grace below tighter ceiling refused")
		}
		b.costUsed = 8
		if _, ok := b.exitGraceRoom(.1); ok {
			t.Fatal("tighter ceiling lost")
		}
	}
	spec := &ir.Budget{MaxCostUSD: 10}
	spec.ClampToCeiling(&ir.Budget{MaxDuration: "2h"})
	b := newSharedBudget(spec, nil)
	b.costUsed = 10.5
	if _, ok := b.exitGraceRoom(.1); !ok {
		t.Fatal("duration ceiling removes unrelated cost grace")
	}
}

// W3: remove the RaiseCaps clamps one axis at a time. Also proves replayed
// raises cannot undo a newly tightened admission ceiling.
func TestRaiseCapsCannotRaiseExternalCeilings(t *testing.T) {
	spec := &ir.Budget{MaxCostUSD: 9.5, MaxTokens: 50, MaxIterations: 5, MaxDuration: "1h"}
	spec.ClampToCeiling(&ir.Budget{MaxCostUSD: 10, MaxTokens: 100, MaxIterations: 10, MaxDuration: "2h"})
	b := newSharedBudget(spec, nil)
	want := ir.BudgetOverrides{MaxCostUSD: 10, MaxTokens: 100, MaxIterations: 10, MaxDuration: "2h0m0s", CapImposed: true}
	got, raised := b.RaiseCaps(ir.BudgetOverrides{MaxCostUSD: 1000, MaxTokens: 1000, MaxIterations: 1000, MaxDuration: "20h"})
	if !raised || got != want {
		t.Fatalf("raise = %+v, %v; want %+v", got, raised, want)
	}
	if got, raised = b.RaiseCaps(ir.BudgetOverrides{MaxCostUSD: 1000, MaxDuration: "20h"}); raised || got != want {
		t.Fatalf("repeat raise = %+v, %v", got, raised)
	}
}

func TestLegacyDonorMarkerSurvivesPlatformCeiling(t *testing.T) {
	wf := &ir.Workflow{}
	ir.ApplyBudgetOverrides(wf, *BudgetOverridesFromWire(&queue.BudgetOverrides{MaxCostUSD: 5, CapImposed: true}))
	wf.Budget.ClampToCeiling(&ir.Budget{MaxCostUSD: 100, MaxDuration: "2h"})
	b := newSharedBudget(wf.Budget, nil)
	b.costUsed = 5
	if _, ok := b.exitGraceRoom(.1); ok {
		t.Fatal("platform ceiling erased legacy donor protection")
	}
	if got, _ := b.RaiseCaps(ir.BudgetOverrides{MaxCostUSD: 100}); got.MaxCostUSD != 5 {
		t.Fatalf("legacy donor raised: %+v", got)
	}
}

// NaN must not turn the new per-axis minimum into an unordered comparison
// that grants arbitrary spend beyond the external ceiling.
func TestAbsoluteCeilingRefusesNaNGrace(t *testing.T) {
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "NaN")
	t.Setenv("ITERION_LOOP_BUDGET_GUARD", "on")
	spec := &ir.Budget{MaxCostUSD: 9.5}
	spec.ClampToCeiling(&ir.Budget{MaxCostUSD: 10})
	b := newSharedBudget(spec, nil)
	b.costUsed = 100
	if _, ok := (&Engine{}).withinSharedBudgetGrace(b); ok {
		t.Fatal("NaN grace bypasses the absolute $10 ceiling")
	}
}
