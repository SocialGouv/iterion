package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Data-driven fan-out (fan_out_each)
// ---------------------------------------------------------------------------
//
// A fan_out_each router resolves its `over:` template to a runtime array and
// re-executes the SINGLE outgoing template subgraph once per element. Unlike
// fan_out_all (one branch per statically-declared edge), the branch count is
// known only at runtime. Each branch carries its element bound onto the
// router's per-branch output (under the router's `as:` name, plus the
// canonical "item" / "index" / "count" keys), so the template subgraph can
// read {{outputs.<router>.item}} and a downstream condition router can pick a
// per-item agent type from boolean discriminators on the element.
//
// The template subgraph itself is statically declared and reachable — only
// its number of RUNTIME executions varies. This keeps the compile-time graph
// static (no synthetic nodes), so reachability/cycle validation is unchanged;
// it is the parallel sibling of the existing bounded loop, which already
// re-executes a static node N times.

// execFanOutEach handles a fan_out_each router node. It returns the next node
// ID to continue from (after the join), mirroring execFanOut.
func (e *Engine) execFanOutEach(ctx context.Context, rs *runState, routerNodeID string) (string, error) {
	node, ok := e.workflow.Nodes[routerNodeID]
	if !ok {
		return "", fmt.Errorf("fan_out_each router %q not found", routerNodeID)
	}
	rn, ok := node.(*ir.RouterNode)
	if !ok || rn.RouterMode != ir.RouterFanOutEach {
		return "", fmt.Errorf("node %q is not a fan_out_each router", routerNodeID)
	}

	// Emit router node_started.
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, routerNodeID, map[string]interface{}{
		"kind":      "router",
		"mode":      "fan_out_each",
		"iteration": e.currentLoopIteration(routerNodeID, rs.loopCounters),
	}); err != nil {
		return "", err
	}

	// Router is a pass-through: its base output = its input from incoming edges.
	routerInput := e.buildNodeInputRS(routerNodeID, rs.scope())
	rs.outputs[routerNodeID] = routerInput

	// Resolve `over` to a concrete array.
	items, err := e.resolveFanOutArray(rn, rs)
	if err != nil {
		return "", err
	}

	// Emit router node_finished with the resolved cardinality.
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, routerNodeID, map[string]interface{}{
		"count": len(items),
	}); err != nil {
		return "", err
	}

	// The single outgoing template edge (validated at compile time).
	var tmplEdge *ir.Edge
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID {
			tmplEdge = edge
			break
		}
	}
	if tmplEdge == nil {
		return "", fmt.Errorf("fan_out_each router %q has no outgoing template edge", routerNodeID)
	}

	// Where the parallel branches reconverge (a node marked await: wait_all,
	// or one with multiple incoming sources). Pre-computed so each branch
	// knows where to stop.
	convergence := e.findConvergencePoint(routerNodeID, []*ir.Edge{tmplEdge})

	// Empty array: nothing to fan out. Continue straight to the convergence
	// point (with an empty aggregate) rather than failing the run.
	if len(items) == 0 {
		rs.outputs[routerNodeID]["count"] = 0
		if convergence == "" {
			// No join downstream: the template head is the only continuation,
			// but with zero items there is nothing to run. Hand the template
			// head back so the main loop proceeds (it will run once with no
			// bound item); callers that need strict empty-skip should add a
			// wait_all join.
			return tmplEdge.To, nil
		}
		if e.logger != nil {
			e.logger.Warn("fan_out_each from %s: 'over' resolved to an empty array — skipping to convergence %s", routerNodeID, convergence)
		}
		return convergence, nil
	}

	// Concurrency limit from budget.
	maxParallel := len(items)
	if e.workflow.Budget != nil && e.workflow.Budget.MaxParallelBranches > 0 && e.workflow.Budget.MaxParallelBranches < maxParallel {
		maxParallel = e.workflow.Budget.MaxParallelBranches
	}

	// Sibling-cancellation policy (mirror execFanOut): cancel siblings on the
	// first failure unless the convergence node is best_effort.
	cancelOnFirstFailure := true
	if convergence != "" {
		if convNode, ok := e.workflow.Nodes[convergence]; ok {
			if mode := nodeAwaitMode(convNode); mode == ir.AwaitBestEffort {
				cancelOnFirstFailure = false
			}
		}
	}

	parentArtifacts := copyOutputs(rs.artifacts)

	branchCtx, cancelBranches := context.WithCancel(ctx)
	defer cancelBranches()

	sem := make(chan struct{}, maxParallel)
	resultsCh := make(chan *branchResult, len(items))

	for i, item := range items {
		branchID := fmt.Sprintf("branch_%s_%d", routerNodeID, i)

		// Per-branch parent outputs: deep-copy the shared state, then overlay
		// the bound element onto THIS router's output so the template subgraph
		// resolves {{outputs.<router>.<binding>}} / .item / .index / .count.
		perBranchOutputs := copyOutputs(rs.outputs)
		if perBranchOutputs[routerNodeID] == nil {
			perBranchOutputs[routerNodeID] = make(map[string]interface{})
		}
		perBranchOutputs[routerNodeID][rn.ItemBinding] = item
		perBranchOutputs[routerNodeID]["item"] = item
		perBranchOutputs[routerNodeID]["index"] = i
		perBranchOutputs[routerNodeID]["count"] = len(items)

		go func(i int, branchID string, parentOutputs map[string]map[string]interface{}) {
			defer func() {
				if r := recover(); r != nil {
					resultsCh <- &branchResult{
						branchID: branchID,
						outputs:  make(map[string]map[string]interface{}),
						err:      fmt.Errorf("panic in branch %s: %v", branchID, r),
					}
				}
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-branchCtx.Done():
				resultsCh <- &branchResult{
					branchID: branchID,
					outputs:  make(map[string]map[string]interface{}),
					err:      e.wrapContextErr(branchCtx.Err()),
				}
				return
			}

			result := e.execBranch(branchCtx, rs, branchID, tmplEdge, parentOutputs, parentArtifacts, convergence)
			if result != nil && result.err != nil {
				if errors.Is(result.err, ErrBudgetExceeded) || cancelOnFirstFailure {
					cancelBranches()
				}
			}
			resultsCh <- result
		}(i, branchID, perBranchOutputs)
	}

	// Collect results (ctx-aware drain, mirrors execFanOut).
	results := make([]*branchResult, 0, len(items))
	var ctxErr error
	doneCh := ctx.Done()
	var graceCh <-chan time.Time
	var graceTimer *time.Timer
	for collected := 0; collected < len(items); {
		select {
		case r := <-resultsCh:
			results = append(results, r)
			collected++
		case <-doneCh:
			ctxErr = ctx.Err()
			cancelBranches()
			doneCh = nil
			graceTimer = time.NewTimer(branchCancelGracePeriod)
			graceCh = graceTimer.C
		case <-graceCh:
			if abandoned := len(items) - collected; abandoned > 0 && e.logger != nil {
				e.logger.Warn("fan_out_each from %s: abandoning %d branch(es) still running %s after cancellation", routerNodeID, abandoned, branchCancelGracePeriod)
			}
			collected = len(items)
		}
	}
	if graceTimer != nil {
		graceTimer.Stop()
	}
	if ctxErr != nil {
		return "", e.wrapContextErr(ctxErr)
	}

	// Determine convergence (prefer branch-reported join; mirror execFanOut).
	convergenceNodeID := ""
	isBestEffort := !cancelOnFirstFailure
	for _, r := range results {
		if r.joinNodeID == "" {
			continue
		}
		if convergenceNodeID == "" {
			convergenceNodeID = r.joinNodeID
			continue
		}
		if convergenceNodeID != r.joinNodeID {
			if isBestEffort {
				if e.logger != nil {
					e.logger.Warn("fan_out_each from %s: branches converge to different nodes (%s vs %s in branch %s) — best_effort, keeping first",
						routerNodeID, convergenceNodeID, r.joinNodeID, r.branchID)
				}
				continue
			}
			return "", fmt.Errorf("branches converge to different nodes: %s vs %s", convergenceNodeID, r.joinNodeID)
		}
	}
	if convergenceNodeID == "" {
		if isBestEffort && allTerminatedAtDone(results) {
			return e.processConvergenceTerminal(rs, results)
		}
		convergenceNodeID = convergence
		if convergenceNodeID == "" {
			return "", fmt.Errorf("no convergence point found after fan_out_each from %s", routerNodeID)
		}
	}

	return e.processConvergence(rs, convergenceNodeID, results)
}

// resolveFanOutArray resolves a fan_out_each router's `over` template to a
// concrete slice of elements.
func (e *Engine) resolveFanOutArray(rn *ir.RouterNode, rs *runState) ([]interface{}, error) {
	if len(rn.OverRefs) == 0 {
		return nil, fmt.Errorf("fan_out_each router %q has no resolvable 'over' source", rn.ID)
	}
	val := e.resolveRef(rn.OverRefs[0], rs.scope())
	return coerceToArray(val, rn.ID, rn.Over)
}

// coerceToArray turns a resolved value into a []interface{}. It accepts a
// native JSON array, a typed slice (reflect fallback), and — because the DSL
// has no object-array type, so upstream agents emit ticket lists as a `json`
// field — a JSON-encoded string that contains an array.
func coerceToArray(val interface{}, routerID, over string) ([]interface{}, error) {
	switch v := val.(type) {
	case nil:
		return nil, fmt.Errorf("fan_out_each router %q: 'over' %q resolved to nil (did the source node run and produce this field?)", routerID, over)
	case []interface{}:
		return v, nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return []interface{}{}, nil
		}
		var arr []interface{}
		if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			return nil, fmt.Errorf("fan_out_each router %q: 'over' %q resolved to a string that is not a JSON array: %w", routerID, over, err)
		}
		return arr, nil
	default:
		rv := reflect.ValueOf(val)
		if rv.Kind() == reflect.Slice {
			out := make([]interface{}, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				out[i] = rv.Index(i).Interface()
			}
			return out, nil
		}
		return nil, fmt.Errorf("fan_out_each router %q: 'over' %q resolved to %T, expected an array", routerID, over, val)
	}
}
