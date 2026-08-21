package runtime

import (
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Edge evaluation
// ---------------------------------------------------------------------------

// evaluateEdges walks the workflow edges originating from fromNodeID and returns
// the first conditional match (or the first unconditional fallback). It returns
// nil when no edge matches. The logPrefix is included in warning messages.
// This variant does NOT check loop counters — use evaluateEdgesWithLoops for
// loop-aware selection.
//
// Branches inside fan-out / llm-multi call this variant. The runState's
// loop counters are owned by the main execution loop and not propagated
// to branches (branches run concurrently with arbitrary topology; sharing
// the loop counter would be racy and the semantics — global vs per-branch
// — are not defined). To prevent runaway iteration when a workflow
// accidentally places a loop or foreach edge inside an execBranch body
// (which would otherwise be selected without the MaxIterations /
// collection-exhaustion guard — foreach with an empty Condition is an
// unconditional back-edge), we skip every IsBoundedIteration() edge.
// The compiler refuses those graphs (C243); this skip is defence in
// depth. The intent matches the existing comment on the Expression case:
// "branches don't iterate, so loop/run namespaces have no meaning."
func (e *Engine) evaluateEdges(fromNodeID, logPrefix string, output map[string]any) *ir.Edge {
	var unconditional, elseEdge *ir.Edge

	for _, edge := range e.workflow.Edges {
		if edge.From != fromNodeID {
			continue
		}
		if edge.IsBoundedIteration() {
			// Defensive: a loop/foreach edge inside a branch would otherwise
			// iterate without the main loop's MaxIterations / foreach
			// bookkeeping (evaluateEdgesWithLoopsRS). Skip with a warning
			// so a hand-built IR or a validator miss cannot run away.
			kind, name := "loop", edge.LoopName
			if edge.ForeachName != "" {
				kind, name = "foreach", edge.ForeachName
			}
			if e.logger != nil {
				e.logger.Warn("%s: node %q: edge to %q is a %s edge (%q) inside a parallel branch — skipped (loop semantics are undefined inside branches)",
					logPrefix, fromNodeID, edge.To, kind, name)
			}
			continue
		}
		if edge.Expression != nil {
			// Expression-form `when` is unsupported in branch-local edge
			// selection — branches don't iterate, so loop/run namespaces
			// have no meaning. Use a simple boolean field condition or
			// compute the predicate in a `compute` node upstream.
			e.logger.Debug("%s: node %q: edge to %q has an expression `when` but branch evaluator has no runState — edge skipped",
				logPrefix, fromNodeID, edge.To)
			continue
		}
		if edge.Condition == "" {
			// `else` edges and bare unconditional edges share the
			// fallback role; the validator forbids coexistence, and the
			// explicit form wins the tie-break defensively.
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
			return edge
		}
	}

	if elseEdge != nil {
		return elseEdge
	}
	return unconditional
}

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
func (e *Engine) evaluateEdgesWithLoopsRS(fromNodeID, logPrefix string, output map[string]any, rs *runState) *ir.Edge {
	var unconditional, elseEdge *ir.Edge
	var exprCtx *expr.Context

	for _, edge := range e.workflow.Edges {
		if edge.From != fromNodeID {
			continue
		}

		if edge.LoopName != "" {
			loop, ok := e.workflow.Loops[edge.LoopName]
			if ok && e.edgeConditionHolds(edge, fromNodeID, logPrefix, output, rs, &exprCtx) {
				maxIter := e.resolveLoopMax(loop, rs)
				if rs.loopCounters[edge.LoopName] >= maxIter {
					kind := "exhausted"
					if loop.Unbounded {
						kind = "out of fuel"
					}
					e.logger.Warn("%s: node %q: edge to %q skipped — loop %q %s (%d/%d)",
						logPrefix, fromNodeID, edge.To, edge.LoopName, kind, rs.loopCounters[edge.LoopName], maxIter)
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
					spent, remaining, used, limit, unit := v.display()
					e.logger.Warn("%s: node %q: edge to %q skipped — loop %q cannot fund another iteration (%s: %.2f%s left, last one took %.2f%s), falling through to the exit path",
						logPrefix, fromNodeID, edge.To, edge.LoopName, v.dimension, remaining, unitSuffix(unit), spent, unitSuffix(unit))
					data := map[string]any{
						"loop": edge.LoopName, "reason": "loop_budget_guard",
						"dimension": v.dimension, "remaining": remaining, "needed": spent,
						// used/limit are what every other budget_warning
						// carries — the run report and the alert manager
						// render the axis from them.
						"used": used, "limit": limit,
					}
					if unit != "" {
						data["unit"] = unit
					}
					if err := e.emit(rs.ctx, rs.runID, store.EventBudgetWarning, fromNodeID, data); err != nil {
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
				return edge
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
			return edge
		}
	}

	if elseEdge != nil {
		return elseEdge
	}
	return unconditional
}
