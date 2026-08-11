package runtime

import (
	"os"
	"strings"
)

// loopBudgetMark records what a run had consumed, per enforced budget
// dimension, at one loop back-edge decision. The distance between two
// consecutive marks is what that loop's body cost in between — the
// price of one more iteration.
type loopBudgetMark map[string]float64

// loopBudgetGuardEnabled reports whether the back-edge affordability
// guard is active. It is on by default and turned off with
// ITERION_LOOP_BUDGET_GUARD=off|0|false — the escape hatch for an
// operator who would rather a loop run until it hits the cap head-on.
func loopBudgetGuardEnabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("ITERION_LOOP_BUDGET_GUARD"))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// loopBudgetShortfall reports the budget dimension that can no longer
// fund another iteration of loopName — "" while the loop is still
// affordable. It also re-marks the loop for the next crossing, so it
// must be called exactly once per back-edge decision.
//
// One iteration is priced by what the PREVIOUS one consumed: the
// distance between this crossing's consumption and the last mark's (the
// session baseline for the first crossing, so a resumed run prices a
// pass by what IT ran, not by everything since the original launch).
//
// The engine already refuses to start a node past 90% of a budget — but
// it does so by FAILING the run, and a campaign bot's delivery tail
// (open the PR, publish the report) then never runs: the work it
// committed in stride is stranded on a clone that dies with the pod.
// Declining the back-edge instead routes the run out through its own
// exit path with what it has banked. The exceeded and hard-limit checks
// stay as the backstop for a single node that overruns on its own.
func (e *Engine) loopBudgetShortfall(loopName string, rs *runState) (dimension string, need, have float64) {
	if !loopBudgetGuardEnabled() {
		return "", 0, 0
	}
	axes := rs.budget.Axes()
	if len(axes) == 0 {
		return "", 0, 0
	}

	previous, marked := rs.loopBudgetMarks[loopName]
	if !marked {
		previous = rs.budgetSessionBase
	}

	mark := make(loopBudgetMark, len(axes))
	for dim, axis := range axes {
		mark[dim] = axis.used
	}
	rs.loopBudgetMarks[loopName] = mark

	for _, dim := range budgetDimensions {
		axis, enforced := axes[dim]
		if !enforced {
			continue
		}
		spent := axis.used - previous[dim]
		// A dimension the last iteration did not move cannot be the one
		// that starves the next; only a measured burn prices a pass.
		if spent > 0 && axis.remaining < spent {
			return dim, spent, axis.remaining
		}
	}

	return "", 0, 0
}

// captureBudgetSessionBase records what this execution session STARTED
// from, per enforced dimension. Zero for a fresh run (the nil map reads
// as 0 everywhere); the restored consumption for a resumed one, so the
// first back-edge crossing after a resume prices its pass by that pass
// alone. Called once, after the checkpoint has seeded the budget and
// before any node runs.
func captureBudgetSessionBase(rs *runState) {
	axes := rs.budget.Axes()
	if len(axes) == 0 {
		return
	}
	base := make(loopBudgetMark, len(axes))
	for dim, axis := range axes {
		base[dim] = axis.used
	}
	rs.budgetSessionBase = base
}
