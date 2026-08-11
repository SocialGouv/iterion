package runtime

import (
	"os"
	"strings"
	"time"
)

// loopBudgetMark records what a run had consumed, per enforced budget
// dimension, at one point in a loop's life: the moment it was entered,
// or its last back-edge crossing. The distance to the next crossing is
// what one iteration of that loop cost.
type loopBudgetMark map[string]float64

// loopBudgetVerdict describes a loop iteration the budget cannot fund.
// spent is what the previous iteration consumed on the blocking axis;
// used/limit are the run's standing on it, carried so the ordinary
// budget_warning consumers (run report, alert manager) render the event
// like any other.
type loopBudgetVerdict struct {
	dimension string
	spent     float64
	remaining float64
	used      float64
	limit     float64
}

// display converts an axis's figures to operator units. Durations are
// tracked in nanoseconds; every other axis is already in its own unit.
func (v loopBudgetVerdict) display() (spent, remaining, used, limit float64, unit string) {
	if v.dimension == "duration" {
		s := float64(time.Second)
		return v.spent / s, v.remaining / s, v.used / s, v.limit / s, "seconds"
	}
	return v.spent, v.remaining, v.used, v.limit, ""
}

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
// fund another iteration of loopName, or nil while the loop is still
// affordable. It is a pure READ: marking is the business of the paths
// that know a loop was entered or its back-edge actually taken
// (markLoopBudget), so evaluating an edge that is not selected can
// neither move a loop's price nor arm a sibling edge with a zero one.
//
// One iteration is priced by what the previous one consumed: the
// distance between the run's consumption now and the loop's last mark.
// A loop with no mark has not been measured yet — it reports no
// shortfall rather than guessing, because the alternative (pricing from
// run start) charges a late-entered loop for everything that ran before
// it existed and declines its very first crossing.
//
// The engine already refuses to start a node past 90% of a budget — but
// it does so by FAILING the run, and a campaign bot's delivery tail
// (open the PR, publish the report) then never runs: the work it
// committed in stride is stranded on a clone that dies with the pod.
// Declining the back-edge instead routes the run out through its own
// exit path with what it has banked. The exceeded and hard-limit checks
// stay as the backstop for a single node that overruns on its own.
func (e *Engine) loopBudgetShortfall(loopName string, rs *runState) *loopBudgetVerdict {
	if !loopBudgetGuardEnabled() {
		return nil
	}
	axes := rs.budget.Axes()
	if len(axes) == 0 {
		return nil
	}
	previous, marked := rs.loopBudgetMarks[loopName]
	if !marked {
		return nil
	}

	for _, dim := range budgetDimensions {
		axis, enforced := axes[dim]
		if !enforced {
			continue
		}
		spent := axis.used - previous[dim]
		// A dimension the last iteration did not move cannot be the one
		// that starves the next; only a measured burn prices a pass.
		if spent <= 0 {
			continue
		}
		limit := axis.used + axis.remaining
		// Decline once another iteration would land at or past the
		// threshold where the engine refuses to START any node
		// (checkBudgetBeforeExec's 90% hard limit). Stopping merely
		// before the CAP would not be enough: the run would fall
		// through into an exit path the hard limit then refuses, which
		// strands the banked work just as surely as overrunning. This
		// also subsumes "the next iteration does not fit at all".
		if axis.used+spent >= budgetHardThreshold*limit {
			return &loopBudgetVerdict{
				dimension: dim,
				spent:     spent,
				remaining: axis.remaining,
				used:      axis.used,
				limit:     limit,
			}
		}
	}

	return nil
}

// markLoopBudget re-bases a loop's price on the run's consumption right
// now. Called when the loop is ENTERED from outside (so its first
// crossing prices its first iteration, not the whole run that preceded
// it) and each time its back-edge is actually taken.
func markLoopBudget(rs *runState, loopName string) {
	axes := rs.budget.Axes()
	if len(axes) == 0 {
		return
	}
	mark := make(loopBudgetMark, len(axes))
	for dim, axis := range axes {
		mark[dim] = axis.used
	}
	rs.loopBudgetMarks[loopName] = mark
}

// baselineUnpricedLoops bases every not-yet-priced loop at the run's
// current consumption. Called once before this session executes a node.
//
// It is the right baseline for a loop whose body contains the workflow
// entry: execution starts INSIDE it, no edge ever enters it, so nothing
// else would mark it — and at that moment the run has consumed nothing,
// which is exactly what its first iteration will be measured against.
// Loops entered later are re-marked at their entry edge, and a loop
// whose price survived on the checkpoint keeps it.
func (e *Engine) baselineUnpricedLoops(rs *runState) {
	for loopName := range e.workflow.Loops {
		if _, priced := rs.loopBudgetMarks[loopName]; priced {
			continue
		}
		markLoopBudget(rs, loopName)
	}
}

// restoreLoopBudgetMarks rehydrates the loop prices from a checkpoint so
// a resumed run keeps pricing iterations across the pause. Consumption
// is restored alongside (SharedBudget.Restore), so the marks stay
// comparable. A checkpoint carrying none — an older format, or a resume
// from the workflow entry — leaves every loop unmarked, which reports no
// shortfall until each has been measured once in this session.
func restoreLoopBudgetMarks(rs *runState, marks map[string]map[string]float64) {
	for loopName, mark := range marks {
		if len(mark) == 0 {
			continue
		}
		rs.loopBudgetMarks[loopName] = loopBudgetMark(mark)
	}
}

// snapshotLoopBudgetMarks projects the loop prices for persistence.
// Returns nil when nothing has been marked, so checkpoints stay compact.
func snapshotLoopBudgetMarks(rs *runState) map[string]map[string]float64 {
	if len(rs.loopBudgetMarks) == 0 {
		return nil
	}
	out := make(map[string]map[string]float64, len(rs.loopBudgetMarks))
	for loopName, mark := range rs.loopBudgetMarks {
		if len(mark) == 0 {
			continue
		}
		out[loopName] = cloneMap(map[string]float64(mark))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
