package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// processConvergence aggregates branch results according to the convergence
// node's await strategy, merges outputs into the run state, builds the
// convergence node's input from multi-edge with-mappings, and returns
// the convergence node ID for the main loop to continue execution.
//
// seeds and floor are both computed by the caller, and for the same reason:
// a template replay knows its per-item edges and evidence, whereas fan_out_all
// branches have distinct targets. seeds names the region whose output view this
// invocation replaces; floor is the settled evidence it leaves behind.
func (e *Engine) processConvergence(rs *runState, convergenceNodeID string, results []*branchResult, seeds []string, floor []store.IncomingEdge) (string, error) {
	convNode, ok := e.workflow.Nodes[convergenceNodeID]
	if !ok {
		return "", &RuntimeError{Code: ErrCodeNodeNotFound, NodeID: convergenceNodeID, Message: fmt.Sprintf("convergence node %q not found", convergenceNodeID)}
	}

	// Determine await strategy: use node's explicit setting, default to wait_all.
	strategy := nodeAwaitMode(convNode)
	if strategy == ir.AwaitNone {
		strategy = ir.AwaitWaitAll
	}

	// Collect failed branches metadata.
	var failedBranches []map[string]any
	// budgetFailures/otherFailures classify why the branches died. A budget
	// refusal cancels its siblings (cancelOnFirstFailure), so a fan-out killed
	// by a spent budget yields one budget error plus N cancellations — the
	// cancellations carry no verdict of their own and must not mask it.
	budgetFailures, otherFailures := 0, 0
	var firstBudgetErr error
	for _, r := range results {
		if r.err != nil {
			failedBranches = append(failedBranches, map[string]any{
				"branch_id": r.branchID,
				"error":     r.err.Error(),
			})
			switch {
			case errors.Is(r.err, ErrBudgetExceeded):
				budgetFailures++
				if firstBudgetErr == nil {
					firstBudgetErr = r.err
				}
			case stoppedBranch(r.err):
			default:
				otherFailures++
			}
		}
	}

	// The branches named — the message quotes the first — in branch-id
	// order, not in the order their goroutines finished.
	sort.Slice(failedBranches, func(i, j int) bool {
		return failedBranches[i]["branch_id"].(string) < failedBranches[j]["branch_id"].(string)
	})

	// Apply await strategy.
	switch strategy {
	case ir.AwaitWaitAll:
		if len(failedBranches) > 0 {
			if budgetFailures > 0 && otherFailures == 0 {
				// A branch never gets the exit grace (withinBudgetGrace), so a
				// fan-out reached on a spent budget refuses every branch and
				// wait_all kills the run here. That death has to keep carrying
				// the sentinel: the cloud runner's terminal-ack carve-out
				// matches errors.Is(err, ErrBudgetExceeded), and a naked error
				// goes back to JetStream as retryable — a resume/refail loop
				// re-provisioning a sandbox to re-hit the same spent budget.
				// It is also what tells the operator to raise the cap and
				// resume rather than hunt a branch bug.
				return "", &RuntimeError{
					Code:    ErrCodeBudgetExceeded,
					Message: fmt.Sprintf("convergence at %s (wait_all): %d of %d branch(es) refused on a spent budget: %v", convergenceNodeID, budgetFailures, len(failedBranches), firstBudgetErr),
					NodeID:  convergenceNodeID,
					Hint:    "raise the exceeded budget dimension (--max-cost-usd / --max-tokens / --max-duration / --max-iterations) and resume; a parallel branch never receives the budget exit grace",
					Cause:   ErrBudgetExceeded,
				}
			}
			msg := fmt.Sprintf("convergence at %s (wait_all): %d branch(es) failed: %v",
				convergenceNodeID, len(failedBranches), failedBranches[0]["error"])
			// An UNDECIDED remote effect outranks the agreement rule below
			// and needs no agreement of its own: ONE branch whose mutation
			// may already have happened is enough to make the aggregate
			// unreplayable. commonBranchFailureCode structurally cannot
			// answer for it — it reads a *RuntimeError, and an executor's
			// typed failure is not one — so the aggregate fell through to
			// the untyped return below, which flattens the chain to a
			// string. The classifier then never sees the ambiguity, the run
			// fails EXECUTION_FAILED, and that code IS on the auto-resume
			// allow-list: the resume re-enters the router, re-runs every
			// branch, and re-sends the very call whose outcome was unknown.
			// One action node under a `fan_out_all`, or a `fan_out_each`
			// over N items, is the whole recipe.
			if amb := firstAmbiguousBranchErr(results); amb != nil {
				return "", &RuntimeError{
					Code:    ErrCodeAmbiguousEffect,
					Message: msg,
					NodeID:  convergenceNodeID,
					Hint:    "check the remote system before resuming: a branch's call may have taken effect, so re-running it would duplicate it",
					// WRAPPED, because the classification asks the CHAIN
					// (IsAmbiguousEffect) and not only the code.
					Cause: amb,
				}
			}
			// When every failed branch carries the SAME typed code, the
			// aggregate keeps it — a fan-out hitting one deterministic
			// wall (a ghost node after a source edit) must not launder
			// NODE_NOT_FOUND into the EXECUTION_FAILED catch-all — the
			// in-process auto-resume gate retries the latter and
			// refuses the former.
			cause := branchEndCause(results)
			if code := commonBranchFailureCode(results); code != "" {
				return "", &RuntimeError{Code: code, NodeID: convergenceNodeID, Message: msg, Cause: cause}
			}
			if cause != nil {
				// Codes that disagree stay the catch-all; what the branches'
				// ends agree on — two refusals at two fail nodes, two
				// ceilings — still travels, for the reader that asks the
				// chain. Typed, because the trunk keeps a RuntimeError as it
				// is and flattens a plain error to its text.
				return "", &RuntimeError{Code: ErrCodeExecutionFailed, NodeID: convergenceNodeID, Message: msg, Cause: cause}
			}
			return "", fmt.Errorf("%s", msg)
		}
	case ir.AwaitBestEffort:
		// Proceed even with failures — failed branch metadata is exposed.
	}

	// This settled invocation replaces its region's output view. Otherwise a
	// failed/skipped branch leaves a previous pass's output looking current,
	// both in the join's mappings and in direct outputs.* references later.
	e.clearConvergedOutputs(rs, convergenceNodeID, seeds)
	// Merge successful branch outputs into the run state.
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for nodeID, output := range r.outputs {
			rs.outputs[nodeID] = output
		}
		for name, output := range r.artifacts {
			// Last-write-wins, but make a silent clobber observable: two
			// parallel branches publishing the same artifact name would
			// otherwise overwrite each other with no trace.
			if prev, ok := rs.artifacts[name]; ok && !reflect.DeepEqual(prev, output) {
				e.logger.Warn("convergence at %s: artifact %q published by multiple branches with differing values — last write wins",
					convergenceNodeID, name)
			}
			rs.artifacts[name] = output
			if owner := r.artifactOwners[name]; owner != "" {
				rs.artifactOwners[name] = owner
			} else {
				delete(rs.artifactOwners, name)
			}
			if revision, ok := r.artifactRevisions[name]; ok {
				rs.artifactRevisions[name] = revision
				rs.artifactOwners[name] = revision.NodeID
			} else {
				delete(rs.artifactRevisions, name)
			}
		}
		for nodeID, version := range r.artifactVersions {
			// Max-merge (not last-write-wins): every branch copies the full
			// parent version map, so a branch that never touched this node
			// still carries its pre-fan-out version. A plain assignment would
			// let a stale branch ordered after the publishing one regress the
			// counter — freezing the persisted artifacts/<node>/<v>.json
			// history on a looped fan-out. Keep the highest version reached.
			if version > rs.artifactVersions[nodeID] {
				rs.artifactVersions[nodeID] = version
			}
		}
	}

	// Add failed branches metadata to outputs so it's available via with-mappings.
	if len(failedBranches) > 0 {
		// Expose as a special output on the convergence node.
		if rs.outputs[convergenceNodeID] == nil {
			rs.outputs[convergenceNodeID] = make(map[string]any)
		}
		rs.outputs[convergenceNodeID]["_failed_branches"] = failedBranches
	}

	// Emit convergence_ready event.
	convData := map[string]any{
		"strategy": strategy.String(),
	}
	if len(failedBranches) > 0 {
		convData["failed_branches"] = failedBranches
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventJoinReady, convergenceNodeID, convData); err != nil {
		e.logger.Warn("failed to emit convergence_ready: %v", err)
	}

	e.mergeJoinIncoming(rs, convergenceNodeID, results, floor)

	// Return the convergence node ID — the main loop will execute it normally.
	return convergenceNodeID, nil
}

// clearConvergedOutputs invalidates only the forward region owned by this
// invocation, stopping before its collector and never crossing a bounded
// back-edge. Seeds come from the launched branches, not all declared router
// targets (an LLM multi-select can launch only a subset). Include untaken
// routes in that region: they produced no current value either.
//
// Run this after the branches settle, before merging their fresh results.
// Their immutable input snapshots can still supply deliberate feedback from
// the preceding pass. Published artifacts retain their separate history.
func (e *Engine) clearConvergedOutputs(rs *runState, joinNodeID string, seeds []string) {
	seen := make(map[string]bool)
	frontier := append([]string(nil), seeds...)
	for len(frontier) > 0 {
		node := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		if node == "" || node == joinNodeID || seen[node] {
			continue
		}
		seen[node] = true
		delete(rs.outputs, node)
		for _, edge := range e.workflow.Edges {
			if edge != nil && edge.From == node && !edge.IsBoundedIteration() {
				frontier = append(frontier, edge.To)
			}
		}
	}
}

// processConvergenceTerminal handles an all-done topology (every branch ran
// to its own *ir.DoneNode and no branch failed), including an implicit
// wait_all fan whose bounded local cycle leaves no structural collector.
// Merges branch outputs/artifacts into the run state and hands back one
// of the terminal node IDs so the engine's main loop emits run_finished.
func (e *Engine) processConvergenceTerminal(rs *runState, results []*branchResult, strategy ir.AwaitMode) (string, error) {
	for _, r := range results {
		for nodeID, output := range r.outputs {
			rs.outputs[nodeID] = output
		}
		for name, output := range r.artifacts {
			rs.artifacts[name] = output
			if owner := r.artifactOwners[name]; owner != "" {
				rs.artifactOwners[name] = owner
			} else {
				delete(rs.artifactOwners, name)
			}
			if revision, ok := r.artifactRevisions[name]; ok {
				rs.artifactRevisions[name] = revision
				rs.artifactOwners[name] = revision.NodeID
			} else {
				delete(rs.artifactRevisions, name)
			}
		}
		for nodeID, version := range r.artifactVersions {
			// Max-merge (not last-write-wins): every branch copies the full
			// parent version map, so a branch that never touched this node
			// still carries its pre-fan-out version. A plain assignment would
			// let a stale branch ordered after the publishing one regress the
			// counter — freezing the persisted artifacts/<node>/<v>.json
			// history on a looped fan-out. Keep the highest version reached.
			if version > rs.artifactVersions[nodeID] {
				rs.artifactVersions[nodeID] = version
			}
		}
	}
	// Use the first branch's terminal node — the engine treats any Done
	// node as run_finished, so picking one is unambiguous.
	terminal := results[0].terminalNodeID
	if err := e.emit(rs.ctx, rs.runID, store.EventJoinReady, terminal, map[string]any{
		"strategy":       strategy.String(),
		"terminal_join":  true,
		"branches_total": len(results),
	}); err != nil {
		e.logger.Warn("failed to emit terminal convergence join_ready: %v", err)
	}
	return terminal, nil
}

// findConvergencePoint elects the downstream node where the router's branches
// reconverge — a node declaring `await:`, or one reached by several distinct
// predecessors INSIDE the fan-out (a predecessor outside it, such as a
// condition router that also reaches a fan-out target directly, is not a
// reconvergence). Terminal nodes (done/fail) can be convergence points when
// multiple branches target them directly. Computed before branches start so
// that each branch knows where to stop; the election is shared with the
// compiler (ir.ExecBranchConvergencePoint) so C243/C244 see the same node.
func (e *Engine) findConvergencePoint(routerNodeID string, fanEdges []*ir.Edge) string {
	return ir.ExecBranchConvergencePoint(e.workflow, routerNodeID, fanEdges)
}

// ---------------------------------------------------------------------------
// Output copy helpers
// ---------------------------------------------------------------------------

// mergeOutputs creates a merged view of parent and branch outputs.
// Branch outputs take precedence over parent outputs.
func mergeOutputs(parent, branch map[string]map[string]any) map[string]map[string]any {
	merged := make(map[string]map[string]any, len(parent)+len(branch))
	for k, v := range parent {
		merged[k] = v
	}
	for k, v := range branch {
		merged[k] = v
	}
	return merged
}

// copyOutputs creates a deep copy of the outputs map so that concurrent
// branches cannot mutate shared parent state. Naive two-level copying
// (the previous implementation) left nested maps and slices aliased
// between branches: a fan-out where two branches both received an
// upstream output containing a nested map would race on that map's
// internal hashtable.
func copyOutputs(src map[string]map[string]any) map[string]map[string]any {
	dst := make(map[string]map[string]any, len(src))
	for k, v := range src {
		inner := make(map[string]any, len(v))
		for ik, iv := range v {
			inner[ik] = deepCopyValue(iv)
		}
		dst[k] = inner
	}
	return dst
}

// deepCopyValue recursively copies a value tree of the shapes produced
// by JSON unmarshalling (map[string]interface{}, []interface{}, plus
// scalars). Other concrete types pass through unchanged — the runtime
// only stores JSON-shaped values in node outputs, so this covers the
// real cases without paying the cost of reflection-based cloning.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyValue(val)
		}
		return out
	default:
		return v
	}
}

// commonBranchFailureCode returns the typed failure code shared by
// EVERY failed branch result, or "" when they disagree (or none is
// firstAmbiguousBranchErr returns the first branch failure that declares its
// remote effect UNDECIDED, or nil.
//
// Unlike the agreement rule below it takes the FIRST match rather than a
// unanimous one: the question it answers is "may anything here already have
// happened", and one branch saying yes settles it.
func firstAmbiguousBranchErr(results []*branchResult) error {
	for _, r := range results {
		if r == nil || r.err == nil {
			continue
		}
		if IsAmbiguousEffect(r.err) {
			return r.err
		}
	}
	return nil
}

// stoppedBranch says a branch ended because the fan-out was stopped — a
// sibling's failure cancelling it, the run cancelled, a deadline — not by
// a failure of its own. The aggregate reads its code and its cause from
// the branches that failed by themselves: a stopped sibling neither
// launders a typed code into EXECUTION_FAILED nor decides, by finishing
// first or last, what the run's end carries.
func stoppedBranch(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRunCancelled)
}

// branchEndCause is what a reader of the run's end asks the chain for —
// the loop decline a death followed, or the sentinel of a deliberate fail
// — when every branch that failed by itself carries the same one: a
// disagreement (one branch's decline beside another's own dead end) is no
// cause, never a guess. Read in branch-id order, so the loop named is the
// same whatever finished first. Nothing else of a branch's chain is
// exposed: the aggregate stays its own classification.
func branchEndCause(results []*branchResult) error {
	var failed []*branchResult
	for _, r := range results {
		if r != nil && r.err != nil && !stoppedBranch(r.err) {
			failed = append(failed, r)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].branchID < failed[j].branchID })
	var cause error
	for _, r := range failed {
		var this error
		var d *LoopDeclined
		switch {
		case errors.As(r.err, &d) && d != nil:
			this = d
		case errors.Is(r.err, ErrDeliberateFailure):
			this = ErrDeliberateFailure
		default:
			return nil
		}
		if cause == nil {
			cause = this
		} else if !sameEndCause(cause, this) {
			return nil
		}
	}
	return cause
}

// sameEndCause says two branch ends read alike: two declines that are both
// a ceiling's or both the program's (CeilingReason), or two refusals.
func sameEndCause(a, b error) bool {
	da, aok := a.(*LoopDeclined)
	db, bok := b.(*LoopDeclined)
	if aok || bok {
		return aok && bok && da.Ceiling() == db.Ceiling()
	}
	return a == b
}

// typed) — partial agreement stays the catch-all, never a guess. A branch
// the fan-out stopped is not read: it failed by nothing of its own.
func commonBranchFailureCode(results []*branchResult) ErrorCode {
	var code ErrorCode
	for _, r := range results {
		if r == nil || r.err == nil || stoppedBranch(r.err) {
			continue
		}
		var rtErr *RuntimeError
		if !errors.As(r.err, &rtErr) || rtErr.Code == "" {
			return ""
		}
		if code == "" {
			code = rtErr.Code
		} else if code != rtErr.Code {
			return ""
		}
	}
	return code
}
