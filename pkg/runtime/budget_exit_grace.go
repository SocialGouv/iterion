package runtime

// budgetExitGraceRatio is the fraction of the declared cap a run may
// spend BEYOND it, once the cap is gone, to finish the path it is on.
// 10% mirrors the margin the loop guard already reserves for the same
// purpose: it declines a back-edge whose next iteration would land past
// budgetHardThreshold, precisely so the tail still fits.
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
//     run can only walk forward to a terminal node.
//
// An earlier draft restricted grace to nodes that cannot reach a loop
// back-edge. It was dropped after measuring it against a real workflow:
// the deterministic gates that decide convergence AND route to the tail
// (scope_check → page_lint → gate → mr_gate) all sit upstream of a
// back-edge, so the restriction excluded exactly the nodes the grace
// exists to let through.
const budgetExitGraceRatio = 0.1

// withinBudgetGrace reports whether a node may run on a spent budget:
// the run must still be inside the bounded allowance. It returns the
// dimension being graced so the caller can name it in the event — an
// operator has to be able to see in the events that a run deliberately
// spent past what it declared, not discover it on the invoice.
func (e *Engine) withinBudgetGrace(rs *runState) (dimension string, ok bool) {
	if rs == nil || rs.budget == nil {
		return "", false
	}
	return rs.budget.exitGraceRoom(budgetExitGraceRatio)
}
