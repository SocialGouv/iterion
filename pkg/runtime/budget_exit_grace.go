package runtime

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultBudgetExitGraceRatio is the fraction of the declared cap a run
// may spend BEYOND it, once the cap is gone, to finish the path it is
// on. 10% mirrors the margin the loop guard already reserves for the
// same purpose: it declines a back-edge whose next iteration would land
// past budgetHardThreshold, precisely so the tail still fits.
//
// That guard covers overruns caused by iteration COUNT. It cannot cover
// a single node that overshoots the cap on its own — and when that
// happens the run dies holding work it has already paid for, with no way
// to deliver it: a pull request that never opens, a report never
// written. The money is spent either way; refusing the last few nodes
// only decides whether anything comes of it.
//
// Two things bound this, and they are the whole safety argument:
//
//   - the ceiling is PROPORTIONAL — a bot with a small cap gets a small
//     grace, and a run far past it stops regardless;
//   - no further ITERATION can start: with the budget spent, the loop
//     guard declines every back-edge it is asked to fund, so a graced
//     run can only walk forward to a terminal node. Because that half of
//     the argument is the loop guard's, withinBudgetGrace refuses the
//     grace outright when the guard is disabled.
//
// An earlier draft restricted grace to nodes that cannot reach a loop
// back-edge. It was dropped after measuring it against a real workflow:
// the deterministic gates that decide convergence AND route to the tail
// (scope_check → page_lint → gate → mr_gate) all sit upstream of a
// back-edge, so the restriction excluded exactly the nodes the grace
// exists to let through.
const defaultBudgetExitGraceRatio = 0.1

// budgetExitGraceRatio resolves the grace allowance:
// ITERION_BUDGET_EXIT_GRACE → 0.1. `0` means the declared caps are
// absolute (no grace) — the escape hatch for deployments where a cap
// must be a hard invoice ceiling (shared instances, pooled credentials).
// Values outside [0,1] and unparsable values fall back to the default
// rather than silently inventing a policy.
func budgetExitGraceRatio() float64 {
	raw := strings.TrimSpace(os.Getenv("ITERION_BUDGET_EXIT_GRACE"))
	if raw == "" {
		return defaultBudgetExitGraceRatio
	}
	switch strings.ToLower(raw) {
	case "off", "no", "false", "none":
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1 {
		// Fail CLOSED on a spend control: an operator reaching for this
		// variable wants a TIGHTER policy ("off", "10" meaning percent…);
		// silently granting the permissive default instead would spend
		// past the cap they asked to be absolute.
		graceEnvWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "iterion: ITERION_BUDGET_EXIT_GRACE=%q is not a ratio in [0,1] — treating the caps as ABSOLUTE (grace 0)\n", raw)
		})
		return 0
	}
	return v
}

var graceEnvWarnOnce sync.Once

// withinBudgetGrace reports whether a node may run on a spent budget:
// the run must still be inside the bounded allowance. It returns the
// dimension being graced so the caller can name it in the event — an
// operator has to be able to see in the events that a run deliberately
// spent past what it declared, not discover it on the invoice.
func (e *Engine) withinBudgetGrace(rs *runState) (dimension string, ok bool) {
	if rs == nil || rs.budget == nil {
		return "", false
	}
	// The "no further ITERATION can start" half of the safety argument
	// is the loop guard's, not the grace's — and that guard is an
	// operator escape hatch (`loop_budget_guard: off`,
	// ITERION_LOOP_BUDGET_GUARD). With it lifted a graced run can take a
	// back-edge and keep looping on a spent budget, so the grace must
	// not be offered.
	if !e.loopBudgetGuardEnabled() {
		return "", false
	}
	// An externally-imposed cap (platform ceiling, pool donor allowance —
	// ir.Budget.CapImposed) is an absolute promise to a third party: no
	// grace, the declared figure IS the wall.
	if rs.budget.capIsImposed() {
		return "", false
	}
	ratio := budgetExitGraceRatio()
	if ratio <= 0 {
		return "", false
	}
	return rs.budget.exitGraceRoom(ratio)
}

// GracedRemainingDuration is the wall-clock room left before the GRACED
// duration ceiling (maxDuration × (1+ratio)). It bounds a node that runs
// under a duration grace: the plain RemainingDuration is already ≤ 0
// there, and running deadline-less would un-bound exactly the axis the
// grace promises to keep proportional.
func (b *SharedBudget) GracedRemainingDuration(ratio float64) (remaining time.Duration, bounded bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxDuration <= 0 {
		return 0, false
	}
	ceiling := time.Duration(float64(b.maxDuration) * (1 + ratio))
	return ceiling - time.Since(b.startedAt), true
}

// capIsImposed reports whether any of this budget's limits was clamped
// by an authority outside the run. Lock-free read of an immutable-after-
// construction field.
func (b *SharedBudget) capIsImposed() bool {
	return b != nil && b.capImposed
}
