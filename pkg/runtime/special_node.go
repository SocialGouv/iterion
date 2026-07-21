package runtime

import (
	"context"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// special_node.go centralises the executor-less node kinds — compute, subbot,
// emit, wait. They share two things this file owns:
//
//   - execSpecialNode: the main-loop lifecycle envelope (node_started → body →
//     output → validate → [post-validate] → node_finished → checkpoint → edge
//     select), previously copy-pasted across execEmit/execWait/execCompute/
//     execSubbot (the ADR-051 "lifecycle-wrapper" follow-on).
//   - the node-specific *body* helpers (computeOutput, runSubbotNode; emitEvent
//     and awaitEvent live in events.go), each taking an explicit resolveScope so
//     the SAME body runs on the main loop (trunk scope) and inside a fan-out
//     branch (merged parent+branch scope) — the ADR-051 "unify branch/main
//     special-node dispatch" follow-on. The branch path (executeNodeForBranch)
//     calls these bodies directly; its own loop owns the branch lifecycle.

// specialNodeBody computes a node's output. It returns an error for a genuine
// execution failure (the envelope/branch aborts the node).
type specialNodeBody func() (map[string]any, error)

// execSpecialNode runs the shared started→body→validate→finished→checkpoint→edge
// envelope for an executor-less node on the main loop. kind is the node-kind
// string used in the node_started event and log messages; extraStarted adds
// node-kind-specific node_started fields (emit/wait `event`, subbot `source`).
// postValidate, when non-nil, runs AFTER schema validation and before
// node_finished — the seam compute uses to persist its published artifact only
// once the output is known valid (validate-before-persist ordering preserved).
func (e *Engine) execSpecialNode(
	rs *runState,
	nodeID, kind string,
	node ir.Node,
	extraStarted map[string]any,
	body specialNodeBody,
	postValidate func(output map[string]any) error,
) (string, error) {
	startedPayload := map[string]any{
		"kind":      kind,
		"iteration": e.currentLoopIteration(nodeID, rs.loopCounters),
	}
	if p := e.currentLoopIterationPath(nodeID, rs.loopCounters); p != "" {
		startedPayload["iteration_path"] = p
	}
	for k, v := range extraStarted {
		startedPayload[k] = v
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, startedPayload); err != nil {
		return "", err
	}

	output, err := body()
	if err != nil {
		return "", err
	}

	rs.outputs[nodeID] = output

	if err := e.validateNodeOutput(nodeID, node, output); err != nil {
		return "", err
	}
	if postValidate != nil {
		if err := postValidate(output); err != nil {
			return "", err
		}
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, nodeID, buildNodeFinishedData(e.sanitizeOutputForEvent(node, output))); err != nil {
		return "", err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(rs.runID, nodeID, output)
	}
	if err := e.store.SaveCheckpoint(rs.ctx, rs.runID, buildCheckpoint(rs, nodeID)); err != nil {
		e.logger.Error("failed to save checkpoint after %s %q: %v", kind, nodeID, err)
	}

	nextNodeID, err := e.selectEdgeRS(rs, nodeID, output)
	if err != nil {
		return "", err
	}
	delete(rs.nodeAttempts, nodeID)
	return nextNodeID, nil
}

// executeSpecialNodeForBranch dispatches the executor-less special node kinds
// (compute / subbot / emit / wait) inside a fan-out branch, reusing the same
// body helpers as the main loop against the branch's merged scope. It returns
// handled=false for any other node kind so the caller falls through to the
// resource-lease + executor path. When handled, done=true means the branch must
// stop with result.err set; the surrounding execBranch loop owns the
// node_started/node_finished pair and artifact publish, so this only produces,
// stores, and validates the output. emit carries no schema, so (matching the
// main loop) it is not validated.
func (e *Engine) executeSpecialNodeForBranch(ctx context.Context, rs *runState, branchID, nodeID string, node ir.Node, sc resolveScope, result *branchResult, slot *branchSlot) (output map[string]any, done, handled bool) {
	// store records a successful body output on the branch and validates it,
	// returning the (output,done) the caller propagates. validate=false skips
	// the schema check (emit has no output schema).
	store := func(out map[string]any, validate bool) (map[string]any, bool, bool) {
		result.outputs[nodeID] = out
		if validate {
			if err := e.validateNodeOutput(nodeID, node, out); err != nil {
				result.err = fmt.Errorf("node %q in branch %s: %w", nodeID, branchID, err)
				return nil, true, true
			}
		}
		return out, false, true
	}
	fail := func(err error) (map[string]any, bool, bool) {
		result.err = fmt.Errorf("node %q in branch %s: %w", nodeID, branchID, err)
		return nil, true, true
	}

	switch n := node.(type) {
	case *ir.EmitNode:
		return store(e.emitEvent(rs, n, sc), false)
	case *ir.WaitNode:
		// Release this branch's semaphore slot while parked so an emitting
		// sibling can acquire one and fire the event (ADR-051 slot-on-park);
		// reacquire on wake before continuing. On timeout/cancel the branch
		// returns still-released — launchBranches' defer release is a no-op.
		slot.release()
		out, err := e.awaitEvent(ctx, rs, nodeID, n)
		if err != nil {
			return fail(err)
		}
		if rerr := slot.acquire(ctx); rerr != nil {
			return fail(rerr)
		}
		return store(out, true)
	case *ir.AwaitAnswersNode:
		// Same slot-on-park discipline as WaitNode: release while parked
		// so sibling branches keep the semaphore, reacquire on wake.
		slot.release()
		out, err := e.awaitAsyncAnswers(ctx, rs, nodeID, n)
		if err != nil {
			return fail(err)
		}
		if rerr := slot.acquire(ctx); rerr != nil {
			return fail(rerr)
		}
		return store(out, true)
	case *ir.ComputeNode:
		out, err := e.computeOutput(rs, nodeID, n, sc)
		if err != nil {
			return fail(err)
		}
		return store(out, true)
	case *ir.SubbotNode:
		out, err := e.runSubbotNode(ctx, rs, nodeID, n, sc)
		if err != nil {
			return fail(err)
		}
		return store(out, true)
	}
	return nil, false, false
}

// computeOutput evaluates a ComputeNode's expressions deterministically against
// the given scope and returns the derived output map. Scope is explicit so the
// same evaluation runs on the main loop (trunk scope) and inside a fan-out
// branch (merged parent+branch scope). No LLM, no shell, no side effects —
// artifact persistence is the caller's concern (compute's postValidate hook).
func (e *Engine) computeOutput(rs *runState, nodeID string, cn *ir.ComputeNode, sc resolveScope) (map[string]any, error) {
	nodeInput := e.buildNodeInputRS(nodeID, sc)
	output := make(map[string]any, len(cn.Exprs))
	exprCtx := e.exprContextScoped(rs, sc, nodeInput)
	for _, ce := range cn.Exprs {
		v, err := evalComputeExpr(ce.AST, exprCtx)
		if err != nil {
			return nil, &RuntimeError{
				Code:    ErrCodeExecutionFailed,
				Message: fmt.Sprintf("compute %q: field %q expression %q: %v", nodeID, ce.Key, ce.Raw, err),
				NodeID:  nodeID,
				Hint:    "check the compute node's expressions for type mismatches or unknown references",
			}
		}
		output[ce.Key] = v
	}
	return output, nil
}

// runSubbotNode resolves a SubbotNode's `with:` mappings against the given
// scope, acquires its `needs:` resource leases (surfacing the leased instance id
// to the child as `_lease_<resource>`), invokes the host-supplied SubbotRunner,
// and returns the child's terminal output. Leases are released when this returns
// (the resource isn't used by the downstream validate/checkpoint steps). Scope
// is explicit so a subbot resolves its inputs correctly on the main loop and
// inside a fan-out branch alike.
func (e *Engine) runSubbotNode(ctx context.Context, rs *runState, nodeID string, sn *ir.SubbotNode, sc resolveScope) (map[string]any, error) {
	if e.subbotRunner == nil {
		return nil, &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("subbot %q: no SubbotRunner is wired", nodeID),
			NodeID:  nodeID,
			Hint:    "subbot nodes need the CLI/studio runtime that can compile + run a child .bot; the bare engine can't (import cycle with runview)",
		}
	}

	// Resolve the child's input vars from the `with:` mappings.
	vars := make(map[string]any, len(sn.With))
	for _, dm := range sn.With {
		vars[dm.Key] = e.resolveMapping(dm, sc)
	}

	// Acquire resource leases for the duration of the child run; surface the
	// leased instance id so the child can pick e.g. its worktree index.
	release, leases, lerr := e.acquireResources(ctx, rs, sn.Needs)
	if lerr != nil {
		return nil, lerr
	}
	defer release()
	for res, id := range leases {
		vars["_lease_"+res] = id
	}

	output, err := e.subbotRunner(ctx, SubbotRequest{
		Source:      sn.Source,
		Vars:        vars,
		ParentRunID: rs.runID,
		NodeID:      nodeID,
	})
	if err != nil {
		return nil, &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("subbot %q (source %q): %v", nodeID, sn.Source, err),
			NodeID:  nodeID,
			Cause:   err,
		}
	}
	if output == nil {
		output = map[string]any{}
	}
	return output, nil
}
