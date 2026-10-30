package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// loopBudgetMark records what a run had consumed, per enforced budget
// dimension, at one point in a loop's life: the moment it was entered,
// or its last back-edge crossing. The distance to the next crossing is
// what one iteration of that loop cost.
type loopBudgetMark map[string]float64

// loopBudgetVerdict describes a loop iteration the budget cannot fund.
// spent is what the previous iteration consumed on the blocking axis —
// size-scaled when the loop declares its work signal (sized_by, #2677)
// and both ends of the scale read; used/limit are the run's standing on
// it, carried so the ordinary budget_warning consumers (run report,
// alert manager) render the event like any other.
type loopBudgetVerdict struct {
	dimension string
	spent     float64
	remaining float64
	used      float64
	limit     float64
	// The proration the verdict carries when the loop's sized_by signal
	// read at both ends: the signal's source, its size when the previous
	// iteration was priced, and its size now. prorated=false leaves the
	// trio unset and the decline's prose on the unscaled wording.
	prorated bool
	sizedBy  string
	sizeFrom float64
	sizeTo   float64
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
// guard is active for the run as a whole — the chain without a loop in
// scope: CLI/launch override → workflow block → ITERION_LOOP_BUDGET_GUARD
// → on.
//
// It defaults ON because the behaviour it replaces loses work. Turning it
// off is the escape hatch for an operator who would rather a loop run at
// its cap head-on and take the hard failure.
func (e *Engine) loopBudgetGuardEnabled() bool {
	return e.loopBudgetGuardEnabledFor("")
}

// loopBudgetGuardEnabledFor resolves the chain for ONE loop: the launch
// override (the operator's explicit word) decides first, then the loop's
// own clause (`as name(3) guard: off` — the bot author's word for THAT
// loop, the most specific layer), then the workflow block, the env, and
// the default. A loop that is not declared in the workflow resolves the
// bare chain.
func (e *Engine) loopBudgetGuardEnabledFor(loopName string) bool {
	if v, ok := parseLoopBudgetGuard(e.loopBudgetGuardOverride); ok {
		return v
	}
	if e.workflow != nil {
		if loop := e.workflow.Loops[loopName]; loop != nil {
			if v, ok := parseLoopBudgetGuard(loop.Guard); ok {
				return v
			}
		}
		if v, ok := parseLoopBudgetGuard(e.workflow.LoopBudgetGuard); ok {
			return v
		}
	}
	if v, ok := parseLoopBudgetGuard(os.Getenv("ITERION_LOOP_BUDGET_GUARD")); ok {
		return v
	}
	return true
}

// loopWorkSize reads the loop's declared work signal (`as name(3)
// sized_by "outputs.probe.pending"`, #2677) against the run's live
// state. ok=false — the loop declares no signal, the expression fails,
// or the reading is not a non-negative number — means "no reading": the
// caller prices unscaled rather than inventing a size.
func (e *Engine) loopWorkSize(loop *ir.Loop, rs *runState) (size float64, ok bool) {
	if loop == nil || loop.SizedByAST == nil {
		return 0, false
	}
	v, err := loop.SizedByAST.Eval(e.exprContext(rs, nil))
	if err != nil {
		e.logger.Warn("loop %q sized_by %q: %v — pricing this crossing unscaled",
			loop.Name, loop.SizedByExpr, err)
		return 0, false
	}
	if v == nil {
		// Not yet: the signal's producer has not run (the first crossing
		// of a loop whose signal appears inside its own body). Unscaled,
		// and quiet about it — this is the normal case, not a defect.
		return 0, false
	}
	size, ok = loopSizeNumber(v)
	if !ok {
		e.logger.Warn("loop %q sized_by %q read %v (%T), not a non-negative number — pricing this crossing unscaled",
			loop.Name, loop.SizedByExpr, v, v)
	}
	return size, ok
}

// loopSizeNumber coerces one reading of a loop's work signal to a
// non-negative float. Outputs are dynamically typed, so a numeric string
// reads — the same tolerance the loop cap extends (loopCapInteger).
func loopSizeNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), n >= 0
	case int64:
		return float64(n), n >= 0
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0
	case float32:
		return loopSizeNumber(float64(n))
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return loopSizeNumber(f)
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return loopSizeNumber(f)
		}
	}
	return 0, false
}

// parseLoopBudgetGuard reads one layer of the chain. ok=false means the
// layer is unset (or unreadable) and the next one decides. The DSL
// validator (C133) and ValidateLoopBudgetGuardMode reject anything but
// on|off at their own boundaries; the extra spellings are accepted from
// the env, where operators reach for 0/1.
func parseLoopBudgetGuard(v string) (enabled, ok bool) { return parseOnOff(v) }

// ValidateLoopBudgetGuardMode rejects a --loop-budget-guard value that is
// neither empty (inherit) nor on|off. A typo would otherwise read as
// "inherit" and silently keep the guard an operator asked to lift.
func ValidateLoopBudgetGuardMode(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "on", "off":
		return nil
	}
	return fmt.Errorf("invalid loop-budget-guard mode %q: expected on or off", v)
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
	if !e.loopBudgetGuardEnabledFor(loopName) || rs.branchLocal {
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

	// The loop's declared work signal (#2677): when both ends read, the
	// previous iteration's cost scales by size_now /
	// size_at_previous_crossing, so a shrinking iteration — a re-ask of
	// the still-pending subset — is priced by the traversal, not by the
	// first, largest pass. No reading (no clause, an unreadable signal,
	// or a mark taken before the signal existed — the first crossing of
	// a loop whose signal appears inside its own body) prices unscaled:
	// the default keeps today's behavior word for word.
	var sizeFrom, sizeTo float64
	prorated := false
	var loop *ir.Loop
	if e.workflow != nil {
		loop = e.workflow.Loops[loopName]
	}
	if loop != nil && loop.SizedByAST != nil {
		if now, ok := e.loopWorkSize(loop, rs); ok {
			if from, sampled := rs.loopBudgetSizes[loopName]; sampled && from > 0 {
				sizeFrom, sizeTo, prorated = from, now, true
			}
		}
	}

	for _, dim := range budgetDimensions {
		axis, enforced := axes[dim]
		if !enforced {
			continue
		}
		spent := axis.used - previous[dim]
		if prorated {
			spent *= sizeTo / sizeFrom
		}
		// A dimension the last iteration did not move cannot be the one
		// that starves the next; only a measured burn prices a pass. A
		// prorated zero spends nothing: the iteration carries no work.
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
				prorated:  prorated,
				sizedBy:   loop.SizedByExpr,
				sizeFrom:  sizeFrom,
				sizeTo:    sizeTo,
			}
		}
	}

	return nil
}

// eventData is the budget_warning payload of a declined back-edge, every
// figure in the axis's operator units (seconds on the duration axis, the
// axis's own unit elsewhere) — used/limit as every other budget_warning
// carries them, and the rule itself: the next iteration would reach
// would_reach, at or past threshold (the hard-limit share of the limit),
// which is where the engine refuses to start any node. A reader with more
// remaining than needed is not left guessing why the loop stopped.
//
// The detail is the decline's prose for the surfaces that render a warning
// without its figures (the operator toast, the run report, the alert
// Reason): without it those read the ratio phrasing — "duration budget at
// 59%" — the opposite of what happened. It names the pricing basis (the
// LAST iteration's cost, so a shrinking loop reads priced by its first,
// largest pass) and the escape hatches, so an operator reading the decline
// knows the way through.
func (v loopBudgetVerdict) eventData(loopName string) map[string]any {
	spent, remaining, used, limit, unit := v.display()
	data := map[string]any{
		"loop": loopName, "reason": "loop_budget_guard",
		"dimension": v.dimension, "remaining": remaining, "needed": spent,
		"used": used, "limit": limit,
		"threshold": budgetHardThreshold * limit, "would_reach": used + spent,
		"detail": v.detailText(loopName),
	}
	if unit != "" {
		data["unit"] = unit
	}
	if v.prorated {
		// The sizes the price was scaled with, so a reader of the
		// decline sees the traversal the guard followed, not just its
		// verdict.
		data["sized_by"] = v.sizedBy
		data["size_previous"] = v.sizeFrom
		data["size_now"] = v.sizeTo
	}
	return data
}

// detailText renders the decline in one sentence of prose plus the way
// through: the per-loop clause first (the shrinking-iteration case it
// exists for), then the run-wide chain.
func (v loopBudgetVerdict) detailText(loopName string) string {
	spent, _, used, limit, unit := v.display()
	u := unitSuffix(unit)
	pricing := fmt.Sprintf("priced at the last one's cost (%.2f%s)", spent, u)
	if v.prorated {
		pricing = fmt.Sprintf("priced at the last one's cost scaled by its sized_by signal (%g now / %g at the previous crossing = %.2f%s)",
			v.sizeTo, v.sizeFrom, spent, u)
	}
	return fmt.Sprintf("loop %q declined its back-edge: another iteration, %s, would reach %.2f of the %.2f%s %s limit — at or past the %.2f%s threshold where the engine stops starting nodes; the run leaves by its other edges with what it banked. Without a readable sized_by signal the price is the last iteration's, so a loop whose iterations shrink (a re-ask of the still-pending subset) is priced by its first, largest pass: declare the loop's work signal with `as %s(...) sized_by \"outputs…\"`, lift the guard for this loop with `as %s(...) guard: off`, or run-wide with `loop_budget_guard: off` / `--loop-budget-guard off`.",
		loopName, pricing, used+spent, limit, u, v.dimension, budgetHardThreshold*limit, u, loopName, loopName)
}

// markLoopBudget re-bases a loop's price on the run's consumption right
// now. Called when the loop is ENTERED from outside (so its first
// crossing prices its first iteration, not the whole run that preceded
// it) and each time its back-edge is actually taken.
//
// A loop with a sized_by clause (#2677) also samples its work signal
// here: the reading is the denominator of the scale the NEXT crossing's
// pricing applies. A signal that does not read at mark time — the first
// entry of a loop whose signal appears inside its own body — drops any
// stale sample, so that crossing prices unscaled rather than against a
// previous instance's size.
func (e *Engine) markLoopBudget(rs *runState, loopName string) {
	axes := rs.budget.Axes()
	if len(axes) == 0 {
		return
	}
	mark := make(loopBudgetMark, len(axes))
	for dim, axis := range axes {
		mark[dim] = axis.used
	}
	rs.loopBudgetMarks[loopName] = mark
	var loop *ir.Loop
	if e.workflow != nil {
		loop = e.workflow.Loops[loopName]
	}
	if loop == nil || loop.SizedByAST == nil {
		return
	}
	if size, ok := e.loopWorkSize(loop, rs); ok {
		if rs.loopBudgetSizes == nil {
			rs.loopBudgetSizes = make(map[string]float64)
		}
		rs.loopBudgetSizes[loopName] = size
	} else {
		delete(rs.loopBudgetSizes, loopName)
	}
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
		e.markLoopBudget(rs, loopName)
	}
}

// loopBudgetMarksVersion is the format stamped on checkpoints: version 2
// marks are measured (a loop's entry or its back-edge). Checkpoints below
// it were written by an engine that also kept the run-start baseline on
// loops never entered at their head — see restoreCheckpointBudget.
const loopBudgetMarksVersion = 2

// restoreCheckpointBudget restores consumption and loop prices from a
// checkpoint, then drops what a legacy checkpoint could only have guessed:
// a mark still at the run-start baseline on a loop whose body does not hold
// the workflow entry. Such a loop was entered off its head, on an engine
// that priced entries at the head only, and never measured; restored, the
// zero would price its first crossing after the resume at everything the
// run has spent and decline it. Dropped, the loop reports no shortfall
// until measured and the session baseline prices it from the resume point.
// Provenance decides, never the mark's shape: a version-2 checkpoint keeps
// a zero that a loop legitimately entered at zero consumption.
func (e *Engine) restoreCheckpointBudget(rs *runState, cp *store.Checkpoint) {
	restoreBudgetAccounting(rs, cp)
	if cp == nil || cp.LoopBudgetMarksV >= loopBudgetMarksVersion {
		return
	}
	for loopName, mark := range rs.loopBudgetMarks {
		if isRunStartBaseline(mark) && !loopHoldsEntry(e.workflow.Loops[loopName], e.workflow.Entry) {
			delete(rs.loopBudgetMarks, loopName)
		}
	}
}

// isRunStartBaseline reports a mark taken before the run consumed anything:
// no cost, no tokens, and less than a second of wall time.
func isRunStartBaseline(mark loopBudgetMark) bool {
	for dim, v := range mark {
		if dim == "duration" {
			if v >= float64(time.Second) {
				return false
			}
			continue
		}
		if v != 0 {
			return false
		}
	}
	return true
}

// loopHoldsEntry reports whether the workflow entry sits in the loop's
// body — the one case where a run-start mark is the loop's true price.
func loopHoldsEntry(loop *ir.Loop, entry string) bool {
	return loop != nil && loop.Body[entry]
}

// restoreLoopBudgetSizes rehydrates the sized_by samples beside the
// marks (restoreLoopBudgetMarks). A checkpoint carrying none — an older
// format, or no sized loop — leaves every loop unsampled, which prices
// unscaled until the next mark.
func restoreLoopBudgetSizes(rs *runState, sizes map[string]float64) {
	for loopName, size := range sizes {
		if rs.loopBudgetSizes == nil {
			rs.loopBudgetSizes = make(map[string]float64)
		}
		rs.loopBudgetSizes[loopName] = size
	}
}

// snapshotLoopBudgetSizes projects the sized_by samples for persistence.
// Returns nil when nothing is sampled, so checkpoints stay compact.
func snapshotLoopBudgetSizes(rs *runState) map[string]float64 {
	if len(rs.loopBudgetSizes) == 0 {
		return nil
	}
	return cloneMap(rs.loopBudgetSizes)
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
