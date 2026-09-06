package runtime

import "time"

// RunNamespaceMembers is the exhaustive `run.<member>` vocabulary, in
// documentation order. Every consumer of the namespace — the expression
// evaluator (`compute` nodes, `when` conditions), prompt bodies, tool
// commands / scripts / postconditions, and the data-mapping resolver (a
// fail node's `message:`, an edge `with`, an `emit` payload, a subbot
// `with:`) — resolves exactly these names, so a member added here reaches
// all four at once.
var RunNamespaceMembers = []string{
	"id",
	"elapsed_seconds",
	"cost_usd",
	"tokens",
	"iterations",
	"max_duration_seconds",
	"max_cost_usd",
	"max_tokens",
	"max_iterations",
}

// runNamespace builds the `run.*` view for one resolution: the run's
// identity, what it has consumed, and the caps in force RIGHT NOW (after
// CLI/recipe overrides and any live raise_budget), so a phase-budget guard
// compares against the ceiling the run actually has instead of mirroring
// the `budget:` block through hand-maintained vars.
//
// Consumption and caps come from the run's SharedBudget, snapshotted under
// its own lock so a parallel branch recording usage mid-read cannot pair a
// used value with a cap from another instant.
//
// Two conventions the caller must know, both documented in docs/dsl.md:
//
//   - A `max_*` of 0 means UNBOUNDED on that axis — the workflow declared
//     no cap there — never "no allowance left".
//   - A workflow with NO `budget:` block has no tracker at all, so
//     cost/tokens/iterations read 0 (nothing meters them) and every cap
//     reads 0 (nothing bounds them). `elapsed_seconds` still advances: it
//     falls back to the run state's own clock, which is the one figure a
//     guard cannot reconstruct for itself.
func runNamespace(rs *runState) map[string]any {
	if rs == nil {
		return nil
	}
	st := rs.budget.Status()
	elapsed := st.Elapsed
	if rs.budget == nil {
		// A run state built without a clock (the nil-parent branch guard)
		// reports no elapsed rather than the age of the Unix epoch.
		if rs.startedAt.IsZero() {
			elapsed = 0
		} else {
			elapsed = time.Since(rs.startedAt)
		}
	}
	return map[string]any{
		"id":                   rs.runID,
		"elapsed_seconds":      elapsed.Seconds(),
		"cost_usd":             st.CostUSD,
		"tokens":               int64(st.Tokens),
		"iterations":           int64(st.Iterations),
		"max_duration_seconds": st.MaxDuration.Seconds(),
		"max_cost_usd":         st.MaxCostUSD,
		"max_tokens":           int64(st.MaxTokens),
		"max_iterations":       int64(st.MaxIterations),
	}
}

// resolveRunPath resolves one `run.<member>` lookup. An unknown member
// returns nil — the namespace's historical behaviour, and the same one
// `vars.<unknown>` has: unresolved, never an error and never a zero that a
// guard could mistake for a measurement.
func resolveRunPath(rs *runState, path []string) any {
	if len(path) != 1 {
		return nil
	}
	ns := runNamespace(rs)
	v, ok := ns[path[0]]
	if !ok {
		return nil
	}
	return v
}

// stampNodeDuration records a node's wall-clock execution time on its
// output as `_duration_ms`, the timing counterpart of the `_cost_usd` /
// `_tokens` keys the backends write. Stamped by the engine rather than by
// each backend so every executed node carries it, LLM or not — a tool node
// that shells out is exactly the one an operator wants timed.
//
// A `_duration_ms` already present is left alone: a backend that measured
// the call itself knows better than the engine, which also counts the
// round trip.
func stampNodeDuration(output map[string]any, started time.Time) {
	if output == nil {
		return
	}
	if _, exists := output["_duration_ms"]; exists {
		return
	}
	output["_duration_ms"] = time.Since(started).Milliseconds()
}
