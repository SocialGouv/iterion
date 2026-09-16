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
		ok, err := edge.Expression.EvalBool(*exprCtx)
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
	var exprCtx *expr.Context
	// exhausted names the first loop edge declined at its cap or out of
	// fuel: when nothing else matches, that is what the run died of.
	var exhausted string

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
					} else if unconditional == nil {
						unconditional, unconditionalErr = edge, capErr
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
					// Said as an event, like the liveness stall and the budget
					// guard: a reader of the run — a dry run — tells a bounded
					// loop's cap from an unbounded loop's fuel by the reason.
					reason := "loop_cap"
					if loop.Unbounded {
						reason = "loop_out_of_fuel"
					}
					if err := e.emit(rs.ctx, rs.runID, store.EventBudgetWarning, fromNodeID, map[string]any{
						"loop": edge.LoopName, "reason": reason, "crossings": rs.loopCounters[edge.LoopName], "cap": maxIter,
					}); err != nil {
						e.logger.Warn("failed to emit %s warning: %v", reason, err)
					}
					if exhausted == "" {
						exhausted = fmt.Sprintf("loop %q %s (%d/%d)", edge.LoopName, kind, rs.loopCounters[edge.LoopName], maxIter)
					}
					continue
				}
				// Liveness monitor: an unbounded loop making no progress (its
				// source output unchanged across maxLoopStall crossings) is at a
				// fixpoint — skip the back-edge so the run falls through to the
				// exit path instead of burning the rest of its fuel.
				if loop.Unbounded && e.loopStalled(edge.LoopName, output, rs) {
					e.logger.Warn("%s: node %q: edge to %q skipped — loop %q made no progress for %d crossings (liveness stall), falling through",
						logPrefix, fromNodeID, edge.To, edge.LoopName, maxLoopStall)
					if err := e.emit(rs.ctx, rs.runID, store.EventBudgetWarning, fromNodeID, map[string]any{
						"loop": edge.LoopName, "reason": "liveness_stall", "crossings": maxLoopStall,
					}); err != nil {
						e.logger.Warn("failed to emit liveness_stall warning: %v", err)
					}
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
					if err := e.emit(rs.ctx, rs.runID, store.EventBudgetWarning, fromNodeID, v.eventData(edge.LoopName)); err != nil {
						e.logger.Warn("failed to emit loop_budget_guard warning: %v", err)
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
			}
		}

		// Expression form: parsed AST evaluated against the full context.
		if edge.Expression != nil {
			if exprCtx == nil {
				exprCtx = e.exprContext(rs, output)
			}
			ok, err := edge.Expression.EvalBool(exprCtx)
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
		}
	}
	return unconditional, unconditionalErr
}
