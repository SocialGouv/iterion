package runtime

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Edge evaluation
// ---------------------------------------------------------------------------

// unitSuffix renders a display unit for log interpolation (" seconds"),
// or nothing for the axes that carry their own.
func unitSuffix(unit string) string {
	if unit == "" {
		return ""
	}
	return " " + unit
}

// edgeConditionHolds reports whether a `when` clause admits this edge as
// a candidate. Unconditional and `else` edges always qualify — their
// fallback role is settled later, by the selection order.
//
// Loop and foreach back-edges consult this BEFORE their own bookkeeping:
// a conditional back-edge whose condition is false was never a candidate,
// and must not be priced against the budget or reported as a loop that
// could not be funded. The verdict matches what the selection code below
// would conclude for the same edge, so an early skip here is the same
// decision taken sooner.
func (e *Engine) edgeConditionHolds(edge *ir.Edge, fromNodeID, logPrefix string, output map[string]any, rs *runState, exprCtx **expr.Context) bool {
	if edge.Expression != nil {
		if *exprCtx == nil {
			*exprCtx = e.exprContext(rs, output)
		}
		ok, err := e.edgeWhenHolds(edge, fromNodeID, *exprCtx)
		if err != nil {
			// Selection reports the failure; staying quiet here avoids
			// logging the same broken expression twice per crossing.
			return false
		}
		return ok
	}
	if edge.Condition == "" {
		return true
	}
	val, ok := output[edge.Condition]
	if !ok {
		return false
	}
	boolVal, isBool := val.(bool)
	if !isBool {
		e.logger.Warn("%s: node %q: condition field %q is %T, expected bool — edge to %q skipped",
			logPrefix, fromNodeID, edge.Condition, val, edge.To)
		return false
	}
	if edge.Negated {
		boolVal = !boolVal
	}
	return boolVal
}

// evaluateEdgesWithLoopsRS is the rs-aware variant: it evaluates edge `when`
// expressions against the full runState (vars, outputs, artifacts, loop, run)
// while still falling back to the simple boolean-field check when the edge
// has no parsed Expression. The expression evaluation context is built lazily
// at most once per call (only if at least one outgoing edge uses an
// expression).
func (e *Engine) evaluateEdgesWithLoopsRS(fromNodeID, logPrefix string, output map[string]any, rs *runState) (*ir.Edge, error) {
	var unconditional, elseEdge *ir.Edge
	var unconditionalErr, elseErr error
	// loopFallback is the first unconditional iteration edge — a loop
	// back-edge with budget, a foreach back-edge with an element left — and
	// the fallback that wins over the node's other fallbacks (its exhaustion
	// exit, an `else`) whatever order they are written in: an iteration is
	// left once it is spent, never because its exit was read first. Like
	// the other slots it carries a deferred cap error, so a loop edge whose
	// cap cannot resolve fails the run in either order instead of losing
	// the error to an exit written above it.
	var loopFallback *ir.Edge
	var loopFallbackErr error
	var exprCtx *expr.Context
	// exhausted names the first loop edge declined at its cap or out of
	// fuel: when nothing else matches, that is what the run died of.
	var exhausted string
	// declined is the first loop edge declined for any reason, capDecline
	// the first at its cap or fuel: a death with no edge left carries the
	// decline it follows, for a reader of the error to tell a ceiling the
	// shapes imposed from the program's own dead end.
	var declined, capDecline *LoopDeclined
	// mixed says the node's loop edges were declined for reasons that do not
	// read alike — a bounded cap beside an unbounded loop's fuel — in which
	// case the death carries no decline: a disagreement is no cause, never a
	// guess, whatever order the edges were written in.
	mixed := false
	noteDecline := func(d *LoopDeclined) {
		if declined == nil {
			declined = d
		} else if d.Ceiling() != declined.Ceiling() {
			mixed = true
		}
	}

	for _, edge := range e.workflow.Edges {
		if edge.From != fromNodeID {
			continue
		}

		if edge.LoopName != "" {
			loop, ok := e.workflow.Loops[edge.LoopName]
			if ok && e.edgeConditionHolds(edge, fromNodeID, logPrefix, output, rs, &exprCtx) {
				maxIter, err := e.resolveLoopMaxChecked(loop, rs)
				if err != nil {
					capErr := &RuntimeError{Code: ErrCodeExpressionFailed, NodeID: fromNodeID, Message: err.Error(), Hint: "the loop cap must resolve to a non-negative integer at this crossing"}
					if edge.Condition != "" || edge.Expression != nil {
						return nil, capErr
					}
					// A fallback only becomes a crossing after all conditional
					// alternatives have been considered. Defer its cap failure.
					if edge.IsElse {
						if elseEdge == nil {
							elseEdge, elseErr = edge, capErr
						}
					} else if loopFallback == nil {
						loopFallback, loopFallbackErr = edge, capErr
					}
					continue
				}
				if rs.loopCounters[edge.LoopName] >= maxIter {
					kind := "exhausted"
					if loop.Unbounded {
						kind = "out of fuel"
					}
					e.logger.Warn("%s: node %q: edge to %q skipped — loop %q %s (%d/%d)",
						logPrefix, fromNodeID, edge.To, edge.LoopName, kind, rs.loopCounters[edge.LoopName], maxIter)
					// A bounded loop's cap is the program's design — C145's exit
					// is written for it — so reaching it warns no one: the
					// decline is carried by the death when nothing else matches,
					// and said nowhere else. An unbounded loop out of fuel did
					// not converge on its own, which an operator hears once,
					// like the liveness stall and the budget guard.
					reason := "loop_cap"
					d := &LoopDeclined{Loop: edge.LoopName, Reason: reason}
					if loop.Unbounded {
						reason = "loop_out_of_fuel"
						d = e.declineLoopEdge(rs, fromNodeID, edge.LoopName, reason, loopDeclineData(edge.LoopName, reason,
							fmt.Sprintf("loop %q %s (%d/%d): its edge is declined — the run goes on by its other edges, or dies of LOOP_EXHAUSTED when none matches", edge.LoopName, kind, rs.loopCounters[edge.LoopName], maxIter),
							map[string]any{"crossings": rs.loopCounters[edge.LoopName], "cap": maxIter}))
					}
					if exhausted == "" {
						exhausted = fmt.Sprintf("loop %q %s (%d/%d)", edge.LoopName, kind, rs.loopCounters[edge.LoopName], maxIter)
						capDecline = d
					}
					noteDecline(d)
					continue
				}
				// Liveness monitor: an unbounded loop making no progress (its
				// source output unchanged across maxLoopStall crossings) is at a
				// fixpoint — skip the back-edge so the run falls through to the
				// exit path instead of burning the rest of its fuel.
				if loop.Unbounded && e.loopStalled(edge.LoopName, output, rs) {
					e.logger.Warn("%s: node %q: edge to %q skipped — loop %q made no progress for %d crossings (liveness stall), falling through",
						logPrefix, fromNodeID, edge.To, edge.LoopName, maxLoopStall)
					d := e.declineLoopEdge(rs, fromNodeID, edge.LoopName, "liveness_stall", loopDeclineData(edge.LoopName, "liveness_stall",
						fmt.Sprintf("loop %q made no progress for %d crossings (liveness stall): its edge is declined — the run goes on by its other edges, or dies with no edge left when none matches", edge.LoopName, maxLoopStall),
						map[string]any{"crossings": maxLoopStall}))
					noteDecline(d)
					continue
				}
				// Affordability: another iteration priced by the last one
				// against what the budget has left. Skipping the back-edge
				// hands the run to its exit path with the work it banked,
				// where dying mid-iteration on the hard cap would strand it.
				if v := e.loopBudgetShortfall(edge.LoopName, rs); v != nil {
					spent, remaining, _, _, unit := v.display()
					e.logger.Warn("%s: node %q: edge to %q skipped — loop %q cannot fund another iteration (%s: %.2f%s left, last one took %.2f%s), falling through to the exit path",
						logPrefix, fromNodeID, edge.To, edge.LoopName, v.dimension, remaining, unitSuffix(unit), spent, unitSuffix(unit))
					d := e.declineLoopEdge(rs, fromNodeID, edge.LoopName, "loop_budget_guard", v.eventData(edge.LoopName))
					noteDecline(d)
					continue
				}
				// The loop edge holds and has budget. Without a `when` it is
				// a fallback, and the one fallback that wins: the bare exit
				// beside it is for the spent loop. A `when` on the loop edge
				// keeps the conditional rules below; an `else` loop edge keeps
				// the `else` slot.
				if edge.Condition == "" && edge.Expression == nil && !edge.IsElse {
					if loopFallback == nil {
						loopFallback = edge
					}
					continue
				}
			}
		}

		// Foreach back-edge: take it only while another element remains. The
		// body already ran for the current index; skip (fall through) when
		// index+1 has reached the collection length.
		if edge.ForeachName != "" {
			if fe, ok := e.workflow.Foreaches[edge.ForeachName]; ok {
				count := len(e.resolveForeachCollection(fe, rs.scope()))
				if idx := rs.loopCounters[foreachCounterKey(edge.ForeachName)]; idx+1 >= count {
					e.logger.Warn("%s: node %q: edge to %q skipped — foreach %q exhausted (%d/%d)",
						logPrefix, fromNodeID, edge.To, edge.ForeachName, idx+1, count)
					continue
				}
				// An element is left: the iteration wins over the node's
				// fallbacks, as a loop edge with budget does.
				if edge.Condition == "" && edge.Expression == nil && !edge.IsElse {
					if loopFallback == nil {
						loopFallback = edge
					}
					continue
				}
			}
		}

		// Expression form: parsed AST evaluated against the full context.
		if edge.Expression != nil {
			if exprCtx == nil {
				exprCtx = e.exprContext(rs, output)
			}
			ok, err := e.edgeWhenHolds(edge, fromNodeID, exprCtx)
			if err != nil {
				e.logger.Warn("%s: node %q: edge `when` expression %q failed: %v — edge to %q skipped",
					logPrefix, fromNodeID, edge.ExpressionSrc, err, edge.To)
				continue
			}
			if ok {
				return edge, nil
			}
			continue
		}

		if edge.Condition == "" {
			// Same fallback tie-break as evaluateEdges: the explicit
			// `else` form wins over a bare unconditional (validator
			// forbids coexistence; this is defence in depth).
			if edge.IsElse {
				if elseEdge == nil {
					elseEdge = edge
				}
			} else if unconditional == nil {
				unconditional = edge
			}
			continue
		}
		val, ok := output[edge.Condition]
		if !ok {
			continue
		}
		boolVal, isBool := val.(bool)
		if !isBool {
			e.logger.Warn("%s: node %q: condition field %q is %T, expected bool — edge to %q skipped",
				logPrefix, fromNodeID, edge.Condition, val, edge.To)
			continue
		}
		if edge.Negated {
			boolVal = !boolVal
		}
		if boolVal {
			return edge, nil
		}
	}

	if loopFallback != nil {
		return loopFallback, loopFallbackErr
	}
	if elseEdge != nil {
		return elseEdge, elseErr
	}
	if unconditional == nil && unconditionalErr == nil && exhausted != "" {
		// Nothing else matched and a loop edge was declined at its cap: the
		// run dies of the loop, named as such — LOOP_EXHAUSTED, the code the
		// docs and the retry policy always promised — not of a missing edge
		// in general.
		return nil, &RuntimeError{
			Code:    ErrCodeLoopExhausted,
			Message: fmt.Sprintf("node %q: %s, and no other edge matched", fromNodeID, exhausted),
			NodeID:  fromNodeID,
			Hint:    "add the loop-exhaustion exit: a bare edge from this node, taken once the loop is spent — to a typed `fail <name>:` when exhaustion is a refusal (C145 names the shape at validation)",
			Cause:   declineCause(capDecline, mixed),
		}
	}
	if unconditional == nil && unconditionalErr == nil && declined != nil {
		// Nothing else matched after a loop edge was declined — by the
		// liveness monitor or the budget guard: the death names the decline
		// it follows, on the trunk and in a branch alike.
		return nil, &RuntimeError{
			Code:    ErrCodeNoOutgoingEdge,
			Message: fmt.Sprintf("no outgoing edge from node %q: a loop edge was declined and no other edge matched", fromNodeID),
			NodeID:  fromNodeID,
			Hint:    "ensure the node's output matches at least one edge condition, or add an unconditional fallback edge",
			Cause:   declineCause(declined, mixed),
		}
	}
	return unconditional, unconditionalErr
}

// loopDeclineData is the budget_warning payload of a loop edge the engine
// declined for a reason that is no budget axis (its cap, its fuel, a
// liveness stall). The alert manager and the report render a warning
// without used/limit from `dimension` and `detail` — without them it reads
// as a budget at 0% of 0 — so both ride beside the loop, the reason a
// reader of the run keys on, and the figures.
func loopDeclineData(loop, reason, detail string, figures map[string]any) map[string]any {
	data := make(map[string]any, len(figures)+4)
	for k, v := range figures {
		data[k] = v
	}
	data["loop"], data["reason"], data["dimension"], data["detail"] = loop, reason, "loop", detail
	return data
}

// declineLoopEdge says why the engine declined a loop edge — a
// budget_warning, attributed to the branch whose edge it is — and returns
// the decline for the death that may follow to carry.
func (e *Engine) declineLoopEdge(rs *runState, fromNodeID, loop, reason string, data map[string]any) *LoopDeclined {
	if err := e.emitBranch(rs.ctx, rs.runID, rs.correctionScope, store.EventBudgetWarning, fromNodeID, data); err != nil {
		e.logger.Warn("failed to emit %s warning: %v", reason, err)
	}
	return &LoopDeclined{Loop: loop, Reason: reason}
}

// declineCause is the cause a death carries for a decline: none when the
// node's declines disagreed (mixed), and a nil interface — never a nil
// pointer — when there was none.
func declineCause(d *LoopDeclined, mixed bool) error {
	if d == nil || mixed {
		return nil
	}
	return d
}
