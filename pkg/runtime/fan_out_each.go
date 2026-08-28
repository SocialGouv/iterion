package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Data-driven fan-out (fan_out_each) — with optional dependency DAG scheduling
// ---------------------------------------------------------------------------
//
// A fan_out_each router resolves its `over:` template to a runtime array and
// re-executes the SINGLE outgoing template subgraph once per element. The
// branch count is known only at runtime. Each branch binds its element onto
// the router's per-branch output (under the router's `as:` name, plus the
// canonical "item" / "index" / "count" keys).
//
// PARALLELISATION + DEPENDENCIES in one primitive:
//   - Without `key:`/`depends_on:` every branch is independent → all run in
//     parallel (bounded by max_parallel_branches). This is plain fan-out.
//   - With `key:` (the item's id field) and `depends_on:` (the item's array
//     of ids it depends on), the engine schedules branches as a DAG: an item
//     runs only after all the items it depends on have finished; independent
//     items still run concurrently up to the cap. Empty deps ⇒ fully parallel;
//     a linear chain ⇒ fully sequential; anything between ⇒ topological with
//     maximal parallelism. Cycles are rejected up-front (Kahn). A failed item
//     skips its dependents (they cannot run) — surfaced as failed branches at
//     the convergence join.
//
// The template subgraph stays statically declared and reachable — only its
// number/ordering of RUNTIME executions varies — so reachability/cycle
// validation of the bot graph is unchanged. It reuses the existing parallel
// branch executor (execBranch / processConvergence / findConvergencePoint).

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

	dag := rn.KeyField != ""
	mode := "fan_out_each"
	if dag {
		mode = "fan_out_each_dag"
	}

	// Emit router node_started.
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, routerNodeID, map[string]any{
		"kind":      "router",
		"mode":      mode,
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

	// For DAG mode, build + validate the dependency graph (cycle / unknown
	// id / duplicate key are hard errors discovered before any branch runs).
	var depsIdx [][]int
	if dag {
		depsIdx, err = buildFanOutDAG(items, rn.KeyField, rn.DepsField)
		if err != nil {
			return "", fmt.Errorf("fan_out_each router %q: %w", routerNodeID, err)
		}
	}

	// Emit router node_finished with the resolved cardinality.
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, routerNodeID, map[string]any{
		"count": len(items),
		"dag":   dag,
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

	// Where the parallel branches reconverge.
	convergence := e.findConvergencePoint(routerNodeID, []*ir.Edge{tmplEdge})

	// Empty array: nothing to fan out. Continue straight to the convergence
	// point (with an empty aggregate) rather than failing the run.
	if len(items) == 0 {
		rs.outputs[routerNodeID]["count"] = 0
		if convergence == "" {
			// No reconvergence node: the template subgraph runs ONCE with
			// router.count=0 and no item/index binding. Warn so this isn't
			// silent — a template that dereferences {{item}} will see an
			// empty value. (Control flow is preserved to avoid regressing
			// bots whose fan_out_each legitimately feeds a terminal.)
			if e.logger != nil {
				e.logger.Warn("fan_out_each from %s: 'over' resolved to an empty array and the template has no convergence node — running the template once with no item binding (count=0)", routerNodeID)
			}
			rs.setIncoming(tmplEdge)
			return tmplEdge.To, nil
		}
		if e.logger != nil {
			e.logger.Warn("fan_out_each from %s: 'over' resolved to an empty array — skipping to convergence %s", routerNodeID, convergence)
		}
		// No branch recorded an incoming edge. Leave the join untracked
		// so a previous iteration's union cannot leak into this visit
		// (and the untracked fallback still applies).
		if rs.selectedIncoming != nil {
			delete(rs.selectedIncoming, convergence)
		}
		return convergence, nil
	}

	// Concurrency limit from budget.
	maxParallel := len(items)
	if e.workflow.Budget != nil && e.workflow.Budget.MaxParallelBranches > 0 && e.workflow.Budget.MaxParallelBranches < maxParallel {
		maxParallel = e.workflow.Budget.MaxParallelBranches
	}

	if err := e.validateFanOutEachWorkspaceSafety(routerNodeID, tmplEdge, convergence, len(items), maxParallel); err != nil {
		return "", err
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
	starts := make(map[string]string, len(items))
	for i := range items {
		starts[fmt.Sprintf("branch_%s_%d", routerNodeID, i)] = tmplEdge.To
	}
	parallel, err := e.ensureParallelInvocation(rs, routerNodeID, starts)
	if err != nil {
		return "", err
	}

	branchCtx, cancelBranches := context.WithCancel(ctx)
	defer cancelBranches()

	sem := make(chan struct{}, maxParallel)
	resultsCh := make(chan *branchResult, len(items))

	// runBranch executes one item's template subgraph (acquiring a concurrency
	// slot first). It does NOT send to resultsCh — the caller owns lifecycle.
	runBranch := func(i int) *branchResult {
		item := items[i]
		branchID := fmt.Sprintf("branch_%s_%d", routerNodeID, i)
		if err := parallel.waitResumeTurn(branchCtx, branchID); err != nil {
			return &branchResult{branchID: branchID, outputs: make(map[string]map[string]any), err: e.wrapContextErr(err)}
		}
		perBranchOutputs := copyOutputs(rs.outputs)
		if perBranchOutputs[routerNodeID] == nil {
			perBranchOutputs[routerNodeID] = make(map[string]any)
		}
		perBranchOutputs[routerNodeID][rn.ItemBinding] = item
		perBranchOutputs[routerNodeID]["item"] = item
		perBranchOutputs[routerNodeID]["index"] = i
		perBranchOutputs[routerNodeID]["count"] = len(items)

		slot := &branchSlot{sem: sem}
		if err := slot.acquire(branchCtx); err != nil {
			return &branchResult{
				branchID: branchID,
				outputs:  make(map[string]map[string]any),
				err:      e.wrapContextErr(branchCtx.Err()),
			}
		}
		defer slot.release()
		return e.execBranch(branchCtx, rs, branchID, tmplEdge, perBranchOutputs, parentArtifacts, convergence, slot, parallel)
	}

	// finishBranch applies the shared post-branch cancellation policy.
	finishBranch := func(result *branchResult) {
		if result != nil && result.err != nil {
			if errors.Is(result.err, ErrRunPaused) || errors.Is(result.err, ErrBudgetExceeded) || cancelOnFirstFailure {
				cancelBranches()
			}
		}
		resultsCh <- result
	}

	if !dag {
		// Plain fan-out: every item independent, all launched at once.
		for i := range items {
			go func(i int) {
				branchID := fmt.Sprintf("branch_%s_%d", routerNodeID, i)
				defer func() {
					if r := recover(); r != nil {
						parallel.releaseResumeWaiters(branchID)
						cancelBranches()
						resultsCh <- &branchResult{branchID: branchID, outputs: make(map[string]map[string]any), err: fmt.Errorf("panic in branch %s: %v", branchID, r)}
					}
				}()
				finishBranch(runBranch(i))
			}(i)
		}
	} else {
		// DAG: a goroutine per item that waits for its deps' `done` channels
		// before acquiring a slot. The semaphore bounds only RUNNING branches
		// (deps-waiting branches hold no slot), so independent items run up to
		// the cap while dependents stay parked until their deps finish.
		done := make([]chan struct{}, len(items))
		for i := range items {
			done[i] = make(chan struct{})
		}
		failed := make([]int32, len(items)) // atomic flags, 1 == failed/skipped

		for i := range items {
			go func(i int) {
				branchID := fmt.Sprintf("branch_%s_%d", routerNodeID, i)
				var once sync.Once
				closeDone := func() { once.Do(func() { close(done[i]) }) }
				defer func() {
					if r := recover(); r != nil {
						parallel.releaseResumeWaiters(branchID)
						cancelBranches()
						atomic.StoreInt32(&failed[i], 1)
						closeDone()
						resultsCh <- &branchResult{branchID: branchID, outputs: make(map[string]map[string]any), err: fmt.Errorf("panic in branch %s: %v", branchID, r)}
					}
				}()

				// Wait for every dependency to finish (or for cancellation).
				for _, d := range depsIdx[i] {
					select {
					case <-done[d]:
					case <-branchCtx.Done():
					}
				}

				// Cancelled while waiting → emit a cancel result, unblock dependents.
				if branchCtx.Err() != nil {
					atomic.StoreInt32(&failed[i], 1)
					closeDone()
					finishBranch(&branchResult{branchID: branchID, outputs: make(map[string]map[string]any), err: e.wrapContextErr(branchCtx.Err())})
					return
				}

				// A failed/skipped dependency means this item cannot run.
				for _, d := range depsIdx[i] {
					if atomic.LoadInt32(&failed[d]) == 1 {
						atomic.StoreInt32(&failed[i], 1)
						closeDone()
						finishBranch(&branchResult{branchID: branchID, outputs: make(map[string]map[string]any), err: fmt.Errorf("branch %s skipped: a dependency failed", branchID)})
						return
					}
				}

				result := runBranch(i)
				if result != nil && result.err != nil {
					atomic.StoreInt32(&failed[i], 1)
				}
				closeDone() // release dependents BEFORE the (possibly blocking) send
				finishBranch(result)
			}(i)
		}
	}

	// Collect results (ctx-aware drain, mirrors execFanOut).
	expectedIDs := make([]string, len(items))
	for i := range items {
		expectedIDs[i] = fmt.Sprintf("branch_%s_%d", routerNodeID, i)
	}
	results, ctxErr := e.collectBranches(ctx, branchCtx, cancelBranches, resultsCh, expectedIDs, rs, routerNodeID, "fan_out_each")
	parallel.retire()
	if ctxErr != nil {
		return "", e.wrapContextErr(ctxErr)
	}
	if err := pausedBranchError(results); err != nil {
		return "", err
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
		if allTerminatedAtDone(results) {
			strategy := ir.AwaitWaitAll
			if isBestEffort {
				strategy = ir.AwaitBestEffort
			}
			next, err := e.processConvergenceTerminal(rs, results, strategy)
			if err == nil {
				rs.parallel = nil
			}
			return next, err
		}
		convergenceNodeID = convergence
		if convergenceNodeID == "" {
			return "", fmt.Errorf("no convergence point found after fan_out_each from %s", routerNodeID)
		}
	}

	next, err := e.processConvergence(rs, convergenceNodeID, results)
	if err == nil {
		rs.parallel = nil
	}
	return next, err
}

// buildFanOutDAG resolves the per-item dependency graph for DAG scheduling.
// It returns, for each item index, the indices of the items it depends on.
// keyField identifies each item; depsField holds the array of ids it depends
// on. Errors out on a non-object item, missing/empty/duplicate key, an
// unknown or self dependency, or a dependency cycle (Kahn's algorithm).
func buildFanOutDAG(items []any, keyField, depsField string) ([][]int, error) {
	idToIdx := make(map[string]int, len(items))
	ids := make([]string, len(items))
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("DAG: item %d is not an object, cannot read key %q", i, keyField)
		}
		idv, ok := m[keyField]
		if !ok || idv == nil {
			return nil, fmt.Errorf("DAG: item %d is missing key field %q", i, keyField)
		}
		id := fmt.Sprintf("%v", idv)
		if id == "" {
			return nil, fmt.Errorf("DAG: item %d has an empty key %q", i, keyField)
		}
		if _, dup := idToIdx[id]; dup {
			return nil, fmt.Errorf("DAG: duplicate key %q (item ids must be unique)", id)
		}
		idToIdx[id] = i
		ids[i] = id
	}

	depsIdx := make([][]int, len(items))
	for i, it := range items {
		// Every item was already asserted as a map in the loop above (which
		// errors out otherwise), but re-check here rather than trust that
		// invariant across a refactor — a panic on a DAG-scheduled fan-out
		// would surface as a run crash, not a clean error.
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("DAG: item %d is not an object, cannot read key %q", i, keyField)
		}
		raw, ok := m[depsField]
		if !ok || raw == nil {
			continue // no dependencies
		}
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("DAG: item %q field %q must be an array of ids, got %T", ids[i], depsField, raw)
		}
		for _, dv := range arr {
			did := fmt.Sprintf("%v", dv)
			j, ok := idToIdx[did]
			if !ok {
				return nil, fmt.Errorf("DAG: item %q depends on unknown id %q", ids[i], did)
			}
			if j == i {
				return nil, fmt.Errorf("DAG: item %q depends on itself", ids[i])
			}
			depsIdx[i] = append(depsIdx[i], j)
		}
	}

	// Cycle detection — Kahn's algorithm. If a topological order can't consume
	// every node, a cycle exists.
	indeg := make([]int, len(items))
	dependents := make([][]int, len(items))
	for i := range items {
		indeg[i] = len(depsIdx[i])
		for _, d := range depsIdx[i] {
			dependents[d] = append(dependents[d], i)
		}
	}
	queue := make([]int, 0, len(items))
	for i := range items {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	processed := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		processed++
		for _, m := range dependents[n] {
			indeg[m]--
			if indeg[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	if processed < len(items) {
		return nil, fmt.Errorf("DAG: dependency cycle detected (%d of %d items are in a cycle)", len(items)-processed, len(items))
	}

	return depsIdx, nil
}

// resolveFanOutArray resolves a fan_out_each router's `over` template to a
// concrete slice of elements.
func (e *Engine) resolveFanOutArray(rn *ir.RouterNode, rs *runState) ([]any, error) {
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
func coerceToArray(val any, routerID, over string) ([]any, error) {
	switch v := val.(type) {
	case nil:
		return nil, fmt.Errorf("fan_out_each router %q: 'over' %q resolved to nil (did the source node run and produce this field?)", routerID, over)
	case []any:
		return v, nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return []any{}, nil
		}
		var arr []any
		if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			return nil, fmt.Errorf("fan_out_each router %q: 'over' %q resolved to a string that is not a JSON array: %w", routerID, over, err)
		}
		return arr, nil
	default:
		rv := reflect.ValueOf(val)
		if rv.Kind() == reflect.Slice {
			out := make([]any, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				out[i] = rv.Index(i).Interface()
			}
			return out, nil
		}
		return nil, fmt.Errorf("fan_out_each router %q: 'over' %q resolved to %T, expected an array", routerID, over, val)
	}
}
