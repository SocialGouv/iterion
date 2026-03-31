// Package runtime implements the workflow execution engine.
// It walks the compiled IR graph node by node, persists outputs and
// artifacts via the store, evaluates edge conditions and loop counters,
// and emits lifecycle events. It supports both sequential execution and
// parallel fan-out/join patterns via a bounded branch scheduler.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/recipe"
	"github.com/SocialGouv/iterion/store"
)

// ErrRunPaused is returned by Run or Resume when execution is suspended
// at a human node. This is not a failure — the run can be resumed via
// Engine.Resume.
var ErrRunPaused = errors.New("runtime: run paused waiting for human input")

// ErrRunCancelled is returned when a run is interrupted by context
// cancellation (e.g. SIGINT). Distinguished from failures so callers
// can handle cancellation gracefully.
var ErrRunCancelled = errors.New("runtime: run cancelled")

// NodeExecutor is the abstraction called by the engine to actually run a
// node (LLM call, tool invocation, etc.). The runtime itself is agnostic
// to the concrete implementation — tests supply stubs, production code
// plugs in real providers.
type NodeExecutor interface {
	// Execute runs the given node with the provided input and returns its
	// output. For terminal nodes (done/fail) this is never called.
	Execute(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error)
}

// Engine executes workflows. It supports sequential execution and
// parallel fan-out via bounded branch scheduling.
type Engine struct {
	workflow *ir.Workflow
	store    *store.RunStore
	executor NodeExecutor
}

// New creates a new Engine for a raw workflow.
func New(wf *ir.Workflow, s *store.RunStore, exec NodeExecutor) *Engine {
	return &Engine{workflow: wf, store: s, executor: exec}
}

// NewFromRecipe creates a new Engine by applying a recipe's presets onto
// the given workflow. The recipe merges preset variables, prompt overrides,
// and budget limits, producing a self-contained execution unit.
func NewFromRecipe(r *recipe.RecipeSpec, wf *ir.Workflow, s *store.RunStore, exec NodeExecutor) (*Engine, error) {
	applied, err := r.Apply(wf)
	if err != nil {
		return nil, fmt.Errorf("runtime: apply recipe %q: %w", r.Name, err)
	}
	return &Engine{workflow: applied, store: s, executor: exec}, nil
}

// runState holds the mutable runtime state passed through the execution loop.
type runState struct {
	runID            string
	runInputs        map[string]interface{}
	vars             map[string]interface{}
	outputs          map[string]map[string]interface{}
	loopCounters     map[string]int
	artifactVersions map[string]int
	budget           *SharedBudget // shared across branches, nil if no budget
}

// branchResult holds the outcome of a single parallel branch.
type branchResult struct {
	branchID         string
	outputs          map[string]map[string]interface{}
	artifactVersions map[string]int
	joinNodeID       string // the join node this branch converged to (empty if terminal)
	err              error
}

// Run executes the workflow. It creates a run, walks the graph from the
// entry node, and returns when a terminal node is reached, a human pause
// is hit (ErrRunPaused), or an error occurs.
func (e *Engine) Run(ctx context.Context, runID string, inputs map[string]interface{}) error {
	// Create run in store.
	if _, err := e.store.CreateRun(runID, e.workflow.Name, inputs); err != nil {
		return fmt.Errorf("runtime: create run: %w", err)
	}

	// Emit run_started.
	if err := e.emit(runID, store.EventRunStarted, "", nil); err != nil {
		return err
	}

	rs := &runState{
		runID:            runID,
		runInputs:        inputs,
		vars:             e.resolveVars(inputs),
		outputs:          make(map[string]map[string]interface{}),
		loopCounters:     make(map[string]int),
		artifactVersions: make(map[string]int),
		budget:           newSharedBudget(e.workflow.Budget),
	}

	return e.execLoop(ctx, rs, e.workflow.Entry)
}

// Resume resumes a paused run by recording human answers and continuing
// execution from the node immediately after the human checkpoint.
func (e *Engine) Resume(ctx context.Context, runID string, answers map[string]interface{}) error {
	// Load and validate run state.
	r, err := e.store.LoadRun(runID)
	if err != nil {
		return fmt.Errorf("runtime: load run for resume: %w", err)
	}
	if r.Status != store.RunStatusPausedWaitingHuman {
		return fmt.Errorf("runtime: cannot resume run %q with status %q", runID, r.Status)
	}
	if r.Checkpoint == nil {
		return fmt.Errorf("runtime: run %q has no checkpoint", runID)
	}

	cp := r.Checkpoint
	humanNodeID := cp.NodeID

	// Record answers on the interaction.
	interaction, err := e.store.LoadInteraction(runID, cp.InteractionID)
	if err != nil {
		return fmt.Errorf("runtime: load interaction for resume: %w", err)
	}
	now := time.Now().UTC()
	interaction.AnsweredAt = &now
	interaction.Answers = answers
	if err := e.store.WriteInteraction(interaction); err != nil {
		return fmt.Errorf("runtime: write answered interaction: %w", err)
	}

	// Emit human_answers_recorded.
	if err := e.emit(runID, store.EventHumanAnswersRecorded, humanNodeID, map[string]interface{}{
		"interaction_id": cp.InteractionID,
		"answers":        answers,
	}); err != nil {
		return err
	}

	// Store human answers as the output of the human node.
	outputs := cp.Outputs
	outputs[humanNodeID] = answers

	// Persist artifact if node has publish.
	humanNode, ok := e.workflow.Nodes[humanNodeID]
	if !ok {
		return fmt.Errorf("runtime: human node %q not found in workflow", humanNodeID)
	}
	artifactVersions := cp.ArtifactVersions
	if humanNode.Publish != "" {
		version := artifactVersions[humanNodeID]
		artifact := &store.Artifact{
			RunID:   runID,
			NodeID:  humanNodeID,
			Version: version,
			Data:    answers,
		}
		if err := e.store.WriteArtifact(artifact); err != nil {
			return fmt.Errorf("runtime: write human artifact: %w", err)
		}
		artifactVersions[humanNodeID] = version + 1
		_ = e.emit(runID, store.EventArtifactWritten, humanNodeID, map[string]interface{}{
			"publish": humanNode.Publish,
			"version": version,
		})
	}

	// Mark human node as finished.
	if err := e.emit(runID, store.EventNodeFinished, humanNodeID, nil); err != nil {
		return err
	}

	// Update status to running and emit run_resumed.
	if err := e.store.UpdateRunStatus(runID, store.RunStatusRunning, ""); err != nil {
		return fmt.Errorf("runtime: update status running: %w", err)
	}
	if err := e.emit(runID, store.EventRunResumed, "", nil); err != nil {
		return err
	}

	// Select edge from the human node to find the next node.
	loopCounters := cp.LoopCounters
	nextNodeID, err := e.selectEdge(runID, humanNodeID, answers, loopCounters)
	if err != nil {
		return e.failRunErr(runID, humanNodeID, err)
	}

	rs := &runState{
		runID:            runID,
		runInputs:        r.Inputs,
		vars:             cp.Vars,
		outputs:          outputs,
		loopCounters:     loopCounters,
		artifactVersions: artifactVersions,
		budget:           newSharedBudget(e.workflow.Budget),
	}

	return e.execLoop(ctx, rs, nextNodeID)
}

// execLoop is the shared execution loop used by both Run and Resume.
// It walks the graph from startNodeID until a terminal node, human pause,
// or error.
func (e *Engine) execLoop(ctx context.Context, rs *runState, startNodeID string) error {
	currentNodeID := startNodeID

	for {
		select {
		case <-ctx.Done():
			return e.handleContextDone(rs.runID, currentNodeID, ctx.Err())
		default:
		}

		node, ok := e.workflow.Nodes[currentNodeID]
		if !ok {
			return e.failRunWithCode(rs.runID, currentNodeID,
				fmt.Sprintf("node %q not found", currentNodeID),
				ErrCodeNodeNotFound,
				"check that the workflow graph is valid with 'iterion validate'")
		}

		// --- Terminal nodes ---
		if node.Kind == ir.NodeDone {
			if err := e.emit(rs.runID, store.EventNodeStarted, currentNodeID, nil); err != nil {
				return err
			}
			if err := e.emit(rs.runID, store.EventNodeFinished, currentNodeID, nil); err != nil {
				return err
			}
			if err := e.store.UpdateRunStatus(rs.runID, store.RunStatusFinished, ""); err != nil {
				return err
			}
			return e.emit(rs.runID, store.EventRunFinished, "", nil)
		}
		if node.Kind == ir.NodeFail {
			if err := e.emit(rs.runID, store.EventNodeStarted, currentNodeID, nil); err != nil {
				return err
			}
			if err := e.emit(rs.runID, store.EventNodeFinished, currentNodeID, nil); err != nil {
				return err
			}
			return e.failRun(rs.runID, currentNodeID, "workflow reached fail node")
		}

		// --- Human node ---
		if node.Kind == ir.NodeHuman {
			switch node.HumanMode {
			case ir.HumanAutoAnswer:
				// Intentional fall-through: auto_answer human nodes are
				// executed via the standard node path below (emit started →
				// budget check → executor.Execute → store output → emit
				// finished → select edge). The executor dispatches to
				// executeHumanLLM which handles model resolution and schema.
			case ir.HumanAutoOrPause:
				paused, err := e.execAutoOrPauseHuman(ctx, rs, currentNodeID, node)
				if err != nil {
					return err
				}
				if paused {
					return ErrRunPaused
				}
				// LLM decided no human needed — continue to edge selection.
				nextNodeID, err := e.selectEdge(rs.runID, currentNodeID, rs.outputs[currentNodeID], rs.loopCounters)
				if err != nil {
					return e.failRunErr(rs.runID, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			default:
				return e.pauseAtHuman(rs, currentNodeID, node)
			}
		}

		// --- Fan-out router: spawn parallel branches ---
		if node.Kind == ir.NodeRouter && node.RouterMode == ir.RouterFanOutAll {
			nextNodeID, err := e.execFanOut(ctx, rs, currentNodeID)
			if err != nil {
				return e.failRunErr(rs.runID, currentNodeID, err)
			}
			currentNodeID = nextNodeID
			continue
		}

		// --- Emit node_started ---
		if err := e.emit(rs.runID, store.EventNodeStarted, currentNodeID, map[string]interface{}{
			"kind": node.Kind.String(),
		}); err != nil {
			return err
		}

		// --- Check budget before execution ---
		if err := e.checkBudgetBeforeExec(rs, currentNodeID); err != nil {
			return err
		}

		// --- Build node input from edge mappings ---
		nodeInput := e.buildNodeInput(currentNodeID, rs.vars, rs.outputs, rs.runInputs)

		// --- Execute node ---
		output, err := e.executor.Execute(ctx, node, nodeInput)
		if err != nil {
			return e.failRun(rs.runID, currentNodeID, fmt.Sprintf("node %q execution failed: %v", currentNodeID, err))
		}

		// Store output.
		rs.outputs[currentNodeID] = output

		// Record budget usage and check limits.
		if err := e.recordAndCheckBudget(rs, currentNodeID, output); err != nil {
			return err
		}

		// Persist artifact if node has publish.
		if node.Publish != "" {
			version := rs.artifactVersions[currentNodeID]
			artifact := &store.Artifact{
				RunID:   rs.runID,
				NodeID:  currentNodeID,
				Version: version,
				Data:    output,
			}
			if err := e.store.WriteArtifact(artifact); err != nil {
				return fmt.Errorf("runtime: write artifact: %w", err)
			}
			rs.artifactVersions[currentNodeID] = version + 1

			if err := e.emit(rs.runID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
				"publish": node.Publish,
				"version": version,
			}); err != nil {
				return fmt.Errorf("runtime: artifact written but event emission failed (state inconsistency): %w", err)
			}
		}

		// --- Emit node_finished with usage data ---
		nodeFinishedData := buildNodeFinishedData(output)
		if err := e.emit(rs.runID, store.EventNodeFinished, currentNodeID, nodeFinishedData); err != nil {
			return err
		}

		// --- Select outgoing edge ---
		nextNodeID, err := e.selectEdge(rs.runID, currentNodeID, output, rs.loopCounters)
		if err != nil {
			return e.failRunErr(rs.runID, currentNodeID, err)
		}

		currentNodeID = nextNodeID
	}
}

// ---------------------------------------------------------------------------
// Fan-out / Join — parallel branch scheduler
// ---------------------------------------------------------------------------

// execFanOut handles a fan_out_all router node by spawning parallel
// branches for each outgoing edge, bounded by MaxParallelBranches.
// It returns the next node ID to continue from (after the join).
func (e *Engine) execFanOut(ctx context.Context, rs *runState, routerNodeID string) (string, error) {
	// Emit router node_started.
	if err := e.emit(rs.runID, store.EventNodeStarted, routerNodeID, map[string]interface{}{
		"kind": "router",
		"mode": "fan_out_all",
	}); err != nil {
		return "", err
	}

	// Router is a pass-through: its output = its input from incoming edges.
	routerInput := e.buildNodeInput(routerNodeID, rs.vars, rs.outputs, rs.runInputs)
	rs.outputs[routerNodeID] = routerInput

	// Emit router node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, routerNodeID, nil); err != nil {
		return "", err
	}

	// Collect all outgoing edges from the router.
	var fanEdges []*ir.Edge
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID {
			fanEdges = append(fanEdges, edge)
		}
	}
	if len(fanEdges) == 0 {
		return "", fmt.Errorf("fan_out_all router %q has no outgoing edges", routerNodeID)
	}

	// Validate workspace safety: at most one mutating branch in parallel.
	if err := e.validateWorkspaceSafety(fanEdges); err != nil {
		return "", err
	}

	// Determine concurrency limit from budget.
	maxParallel := len(fanEdges)
	if e.workflow.Budget != nil && e.workflow.Budget.MaxParallelBranches > 0 && e.workflow.Budget.MaxParallelBranches < maxParallel {
		maxParallel = e.workflow.Budget.MaxParallelBranches
	}

	// Snapshot parent outputs (branches read from this, write to their own map).
	parentOutputs := make(map[string]map[string]interface{})
	for k, v := range rs.outputs {
		parentOutputs[k] = v
	}

	// Launch branches with bounded concurrency.
	sem := make(chan struct{}, maxParallel)
	resultsCh := make(chan *branchResult, len(fanEdges))

	for _, edge := range fanEdges {
		branchID := fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To)

		go func(edge *ir.Edge, branchID string) {
			sem <- struct{}{}        // acquire semaphore slot
			defer func() { <-sem }() // release

			result := e.execBranch(ctx, rs, branchID, edge, parentOutputs)
			resultsCh <- result
		}(edge, branchID)
	}

	// Collect all results.
	results := make([]*branchResult, 0, len(fanEdges))
	for range fanEdges {
		results = append(results, <-resultsCh)
	}

	// Determine join node. Prefer the one reported by successful branches;
	// if all branches failed, discover it from the graph topology.
	joinNodeID := ""
	for _, r := range results {
		if r.joinNodeID != "" {
			if joinNodeID == "" {
				joinNodeID = r.joinNodeID
			} else if joinNodeID != r.joinNodeID {
				return "", fmt.Errorf("branches converge to different join nodes: %s vs %s", joinNodeID, r.joinNodeID)
			}
		}
	}
	if joinNodeID == "" {
		// All branches failed before reaching a join. Walk the graph
		// from each fan-out target to find the downstream join node.
		joinNodeID = e.findJoinForRouter(routerNodeID, fanEdges)
		if joinNodeID == "" {
			return "", fmt.Errorf("no join node found after fan_out from %s", routerNodeID)
		}
	}

	// Process the join.
	return e.processJoin(rs, joinNodeID, results)
}

// execBranch runs a single parallel branch starting from the target of
// the given edge. It executes nodes sequentially until it reaches a join
// node, a terminal node, or encounters an error.
func (e *Engine) execBranch(ctx context.Context, rs *runState, branchID string, startEdge *ir.Edge, parentOutputs map[string]map[string]interface{}) *branchResult {
	result := &branchResult{
		branchID:         branchID,
		outputs:          make(map[string]map[string]interface{}),
		artifactVersions: make(map[string]int),
	}

	runID := rs.runID
	vars := rs.vars
	runInputs := rs.runInputs

	// Emit branch_started (best-effort — branch can proceed without the event).
	if err := e.emitBranch(runID, branchID, store.EventBranchStarted, startEdge.To, nil); err != nil {
		log.Printf("runtime: branch %s: failed to emit branch_started: %v", branchID, err)
	}

	currentNodeID := startEdge.To

	for {
		select {
		case <-ctx.Done():
			result.err = e.wrapContextErr(ctx.Err())
			return result
		default:
		}

		node, ok := e.workflow.Nodes[currentNodeID]
		if !ok {
			result.err = fmt.Errorf("node %q not found in branch %s", currentNodeID, branchID)
			return result
		}

		// Stop at join node — the branch has converged.
		if node.Kind == ir.NodeJoin {
			result.joinNodeID = currentNodeID
			return result
		}

		// Stop at terminal nodes within a branch.
		if node.Kind == ir.NodeDone || node.Kind == ir.NodeFail {
			if node.Kind == ir.NodeFail {
				result.err = fmt.Errorf("branch %s reached fail node %q", branchID, currentNodeID)
			}
			return result
		}

		// Check budget before execution (duration check).
		if rs.budget != nil {
			checks := rs.budget.Check()
			if exc := findExceeded(checks); exc != nil {
				if err := e.emitBranch(runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]interface{}{
					"dimension": exc.dimension,
					"used":      exc.used,
					"limit":     exc.limit,
				}); err != nil {
					log.Printf("runtime: branch %s: failed to emit budget_exceeded: %v", branchID, err)
				}
				result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
				return result
			}
		}

		// Emit node_started.
		if err := e.emitBranch(runID, branchID, store.EventNodeStarted, currentNodeID, map[string]interface{}{
			"kind": node.Kind.String(),
		}); err != nil {
			log.Printf("runtime: branch %s: failed to emit node_started: %v", branchID, err)
		}

		// Build input: merge parent outputs with branch-local outputs so
		// refs to upstream nodes (before the router) still resolve.
		merged := mergeOutputs(parentOutputs, result.outputs)
		nodeInput := e.buildNodeInput(currentNodeID, vars, merged, runInputs)

		// Execute.
		output, err := e.executor.Execute(ctx, node, nodeInput)
		if err != nil {
			result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
			if emitErr := e.emitBranch(runID, branchID, store.EventNodeFinished, currentNodeID, map[string]interface{}{
				"error": err.Error(),
			}); emitErr != nil {
				log.Printf("runtime: branch %s: failed to emit node_finished: %v", branchID, emitErr)
			}
			return result
		}

		result.outputs[currentNodeID] = output

		// Record budget usage and check limits.
		if rs.budget != nil {
			tokens, costUSD := extractUsage(output)
			checks := rs.budget.RecordUsage(tokens, costUSD)

			// Emit warnings.
			for _, w := range findWarnings(checks) {
				if err := e.emitBranch(runID, branchID, store.EventBudgetWarning, currentNodeID, map[string]interface{}{
					"dimension": w.dimension,
					"used":      w.used,
					"limit":     w.limit,
				}); err != nil {
					log.Printf("runtime: branch %s: failed to emit budget_warning: %v", branchID, err)
				}
			}

			// Fail on exceeded.
			if exc := findExceeded(checks); exc != nil {
				if err := e.emitBranch(runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]interface{}{
					"dimension": exc.dimension,
					"used":      exc.used,
					"limit":     exc.limit,
				}); err != nil {
					log.Printf("runtime: branch %s: failed to emit budget_exceeded: %v", branchID, err)
				}
				result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
				return result
			}
		}

		// Persist artifact if node has publish.
		if node.Publish != "" {
			version := result.artifactVersions[currentNodeID]
			artifact := &store.Artifact{
				RunID:   runID,
				NodeID:  currentNodeID,
				Version: version,
				Data:    output,
			}
			if err := e.store.WriteArtifact(artifact); err != nil {
				result.err = fmt.Errorf("node %q in branch %s: write artifact: %w", currentNodeID, branchID, err)
				return result
			}
			result.artifactVersions[currentNodeID] = version + 1
			if err := e.emitBranch(runID, branchID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
				"publish": node.Publish,
				"version": version,
			}); err != nil {
				log.Printf("runtime: branch %s: failed to emit artifact_written: %v", branchID, err)
			}
		}

		// Emit node_finished with usage data.
		if err := e.emitBranch(runID, branchID, store.EventNodeFinished, currentNodeID, buildNodeFinishedData(output)); err != nil {
			log.Printf("runtime: branch %s: failed to emit node_finished: %v", branchID, err)
		}

		// Select next edge (branch-local, no loop counters needed in branches).
		merged = mergeOutputs(parentOutputs, result.outputs)
		nextNodeID, err := e.selectEdgeBranch(runID, branchID, currentNodeID, output)
		if err != nil {
			result.err = err
			return result
		}

		currentNodeID = nextNodeID
	}
}

// selectEdgeBranch picks the next node for a branch. It is simpler than
// selectEdge: no loop counter enforcement, events carry a branch ID.
func (e *Engine) selectEdgeBranch(runID, branchID, fromNodeID string, output map[string]interface{}) (string, error) {
	var unconditional *ir.Edge
	var selected *ir.Edge

	for _, edge := range e.workflow.Edges {
		if edge.From != fromNodeID {
			continue
		}
		if edge.Condition == "" {
			if unconditional == nil {
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
			log.Printf("runtime: branch %s: node %q: condition field %q is %T, expected bool — edge to %q skipped",
				branchID, fromNodeID, edge.Condition, val, edge.To)
			continue
		}
		if edge.Negated {
			boolVal = !boolVal
		}
		if boolVal {
			selected = edge
			break
		}
	}

	if selected == nil {
		selected = unconditional
	}
	if selected == nil {
		return "", fmt.Errorf("no outgoing edge from node %q in branch %s", fromNodeID, branchID)
	}

	if err := e.emitBranch(runID, branchID, store.EventEdgeSelected, "", map[string]interface{}{
		"from": selected.From,
		"to":   selected.To,
	}); err != nil {
		log.Printf("runtime: branch %s: failed to emit edge_selected: %v", branchID, err)
	}

	return selected.To, nil
}

// processJoin aggregates branch results according to the join node's
// strategy, merges outputs into the run state, and returns the next
// node ID to continue execution from.
func (e *Engine) processJoin(rs *runState, joinNodeID string, results []*branchResult) (string, error) {
	joinNode, ok := e.workflow.Nodes[joinNodeID]
	if !ok {
		return "", fmt.Errorf("join node %q not found", joinNodeID)
	}

	// Emit join node_started.
	if err := e.emit(rs.runID, store.EventNodeStarted, joinNodeID, map[string]interface{}{
		"kind":     "join",
		"strategy": joinNode.JoinStrategy.String(),
	}); err != nil {
		return "", err
	}

	// Collect failed branches metadata.
	var failedBranches []map[string]interface{}
	for _, r := range results {
		if r.err != nil {
			failedBranches = append(failedBranches, map[string]interface{}{
				"branch_id": r.branchID,
				"error":     r.err.Error(),
			})
		}
	}

	// Apply join strategy.
	switch joinNode.JoinStrategy {
	case ir.JoinWaitAll:
		// All required branches must have succeeded.
		if len(failedBranches) > 0 {
			return "", fmt.Errorf("join %s (wait_all): %d branch(es) failed: %v",
				joinNodeID, len(failedBranches), failedBranches[0]["error"])
		}
	case ir.JoinBestEffort:
		// Proceed even with failures — failed branch metadata is exposed.
	}

	// Merge successful branch outputs into the run state.
	for _, r := range results {
		if r.err != nil {
			continue
		}
		for nodeID, output := range r.outputs {
			rs.outputs[nodeID] = output
		}
		for nodeID, version := range r.artifactVersions {
			rs.artifactVersions[nodeID] = version
		}
	}

	// Build join output: one entry per required node + failed branches metadata.
	joinOutput := make(map[string]interface{})
	for _, reqNodeID := range joinNode.Require {
		if output, ok := rs.outputs[reqNodeID]; ok {
			joinOutput[reqNodeID] = output
		}
	}
	if len(failedBranches) > 0 {
		joinOutput["_failed_branches"] = failedBranches
	}
	rs.outputs[joinNodeID] = joinOutput

	// Emit join_ready.
	joinData := map[string]interface{}{
		"strategy": joinNode.JoinStrategy.String(),
		"required": joinNode.Require,
	}
	if len(failedBranches) > 0 {
		joinData["failed_branches"] = failedBranches
	}
	if err := e.emit(rs.runID, store.EventJoinReady, joinNodeID, joinData); err != nil {
		log.Printf("runtime: failed to emit join_ready: %v", err)
	}

	// Emit join node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, joinNodeID, nil); err != nil {
		return "", err
	}

	// Select next edge from the join.
	nextNodeID, err := e.selectEdge(rs.runID, joinNodeID, joinOutput, rs.loopCounters)
	if err != nil {
		return "", err
	}

	return nextNodeID, nil
}

// findJoinForRouter walks outgoing edges from the router's targets to
// find a downstream join node. This is used when all branches failed
// before reaching the join, so we can still process the join.
func (e *Engine) findJoinForRouter(routerNodeID string, fanEdges []*ir.Edge) string {
	// BFS from each fan-out target to find a join node.
	for _, startEdge := range fanEdges {
		visited := map[string]bool{}
		queue := []string{startEdge.To}
		for len(queue) > 0 {
			nodeID := queue[0]
			queue = queue[1:]
			if visited[nodeID] {
				continue
			}
			visited[nodeID] = true

			node, ok := e.workflow.Nodes[nodeID]
			if !ok {
				continue
			}
			if node.Kind == ir.NodeJoin {
				return nodeID
			}
			// Follow outgoing edges.
			for _, edge := range e.workflow.Edges {
				if edge.From == nodeID {
					queue = append(queue, edge.To)
				}
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Human node execution
// ---------------------------------------------------------------------------

// execAutoOrPauseHuman handles a human node in auto_or_pause mode.
// It calls the executor (LLM) to produce answers plus a needs_human_input flag.
// Returns (true, nil) if the run was paused, (false, nil) if the LLM answered.
func (e *Engine) execAutoOrPauseHuman(ctx context.Context, rs *runState, nodeID string, node *ir.Node) (bool, error) {
	// Emit node_started.
	if err := e.emit(rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind": node.Kind.String(),
	}); err != nil {
		return false, err
	}

	// Check budget.
	if err := e.checkBudgetBeforeExec(rs, nodeID); err != nil {
		return false, err
	}

	// Build input and execute LLM.
	nodeInput := e.buildNodeInput(nodeID, rs.vars, rs.outputs, rs.runInputs)
	output, err := e.executor.Execute(ctx, node, nodeInput)
	if err != nil {
		return false, e.failRun(rs.runID, nodeID, fmt.Sprintf("human node %q auto_or_pause execution failed: %v", nodeID, err))
	}

	// Record budget usage.
	if err := e.recordAndCheckBudget(rs, nodeID, output); err != nil {
		return false, err
	}

	// Inspect the needs_human_input flag.
	needsHuman := false
	if v, ok := output["needs_human_input"]; ok {
		if b, ok := v.(bool); ok {
			needsHuman = b
		}
	}

	// Strip the wrapper field from the output.
	delete(output, "needs_human_input")

	if needsHuman {
		if err := e.persistPause(rs, nodeID); err != nil {
			return false, err
		}
		return true, nil
	}

	// LLM decided no human input needed — store output and continue.
	rs.outputs[nodeID] = output

	// Persist artifact if node has publish.
	if node.Publish != "" {
		version := rs.artifactVersions[nodeID]
		artifact := &store.Artifact{
			RunID:   rs.runID,
			NodeID:  nodeID,
			Version: version,
			Data:    output,
		}
		if err := e.store.WriteArtifact(artifact); err != nil {
			return false, fmt.Errorf("runtime: write artifact: %w", err)
		}
		rs.artifactVersions[nodeID] = version + 1
		_ = e.emit(rs.runID, store.EventArtifactWritten, nodeID, map[string]interface{}{
			"publish": node.Publish,
			"version": version,
		})
	}

	// Emit node_finished.
	nodeFinishedData := buildNodeFinishedData(output)
	if err := e.emit(rs.runID, store.EventNodeFinished, nodeID, nodeFinishedData); err != nil {
		return false, err
	}

	return false, nil
}

// ---------------------------------------------------------------------------
// Human pause
// ---------------------------------------------------------------------------

// pauseAtHuman suspends the run at a human node: persists an interaction,
// saves checkpoint state, and returns ErrRunPaused.
func (e *Engine) pauseAtHuman(rs *runState, nodeID string, node *ir.Node) error {
	// Emit node_started for the human node.
	if err := e.emit(rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind": node.Kind.String(),
	}); err != nil {
		return err
	}

	if err := e.persistPause(rs, nodeID); err != nil {
		return err
	}

	return ErrRunPaused
}

// persistPause writes the interaction, emits pause events, and saves the
// checkpoint. It contains the shared logic used by both pauseAtHuman and
// execAutoOrPauseHuman. The caller is responsible for emitting node_started
// before calling this method.
func (e *Engine) persistPause(rs *runState, nodeID string) error {
	// Build questions from the node's input (edge mappings into this node).
	questions := e.buildNodeInput(nodeID, rs.vars, rs.outputs, nil)

	// Create interaction. Include loop iteration in the ID so that
	// human nodes inside loops produce unique interactions per iteration.
	interactionID := fmt.Sprintf("%s_%s", rs.runID, nodeID)
	if loopIter := e.currentLoopIteration(nodeID, rs.loopCounters); loopIter > 0 {
		interactionID = fmt.Sprintf("%s_%s_%d", rs.runID, nodeID, loopIter)
	}
	interaction := &store.Interaction{
		ID:          interactionID,
		RunID:       rs.runID,
		NodeID:      nodeID,
		RequestedAt: time.Now().UTC(),
		Questions:   questions,
	}
	if err := e.store.WriteInteraction(interaction); err != nil {
		return fmt.Errorf("runtime: write interaction: %w", err)
	}

	// Emit human_input_requested.
	if err := e.emit(rs.runID, store.EventHumanInputRequested, nodeID, map[string]interface{}{
		"interaction_id": interactionID,
		"questions":      questions,
	}); err != nil {
		return err
	}

	// Emit run_paused.
	if err := e.emit(rs.runID, store.EventRunPaused, nodeID, nil); err != nil {
		return err
	}

	// Atomically save checkpoint and set status to paused in a single write.
	cp := &store.Checkpoint{
		NodeID:           nodeID,
		InteractionID:    interactionID,
		Outputs:          rs.outputs,
		LoopCounters:     rs.loopCounters,
		ArtifactVersions: rs.artifactVersions,
		Vars:             rs.vars,
	}
	if err := e.store.PauseRun(rs.runID, cp); err != nil {
		return fmt.Errorf("runtime: pause run: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Edge selection
// ---------------------------------------------------------------------------

// selectEdge picks the next node by evaluating outgoing edges from the
// current node. Conditional edges are checked first; the first matching
// unconditional edge serves as fallback. Loop counters are enforced.
func (e *Engine) selectEdge(runID, fromNodeID string, output map[string]interface{}, loopCounters map[string]int) (string, error) {
	var unconditional *ir.Edge
	var selected *ir.Edge

	for _, edge := range e.workflow.Edges {
		if edge.From != fromNodeID {
			continue
		}

		if edge.Condition == "" {
			// Unconditional edge — keep first as fallback.
			if unconditional == nil {
				unconditional = edge
			}
			continue
		}

		// Evaluate condition against output.
		val, ok := output[edge.Condition]
		if !ok {
			continue
		}
		boolVal, isBool := val.(bool)
		if !isBool {
			log.Printf("runtime: node %q: condition field %q is %T, expected bool — edge to %q skipped",
				fromNodeID, edge.Condition, val, edge.To)
			continue
		}
		if edge.Negated {
			boolVal = !boolVal
		}
		if boolVal {
			selected = edge
			break
		}
	}

	if selected == nil {
		selected = unconditional
	}
	if selected == nil {
		return "", &RuntimeError{
			Code:    ErrCodeNoOutgoingEdge,
			Message: fmt.Sprintf("no outgoing edge from node %q", fromNodeID),
			NodeID:  fromNodeID,
			Hint:    "ensure the node's output matches at least one edge condition, or add an unconditional fallback edge",
		}
	}

	// Enforce loop counter.
	if selected.LoopName != "" {
		loop, ok := e.workflow.Loops[selected.LoopName]
		if !ok {
			return "", fmt.Errorf("edge references unknown loop %q", selected.LoopName)
		}
		count := loopCounters[selected.LoopName]
		if count >= loop.MaxIterations {
			return "", &RuntimeError{
				Code:    ErrCodeLoopExhausted,
				Message: fmt.Sprintf("loop %q exceeded max iterations (%d)", selected.LoopName, loop.MaxIterations),
				NodeID:  fromNodeID,
				Hint:    fmt.Sprintf("increase max_iterations for loop %q or review the loop exit condition", selected.LoopName),
			}
		}
		loopCounters[selected.LoopName] = count + 1
	}

	// Emit edge_selected.
	data := map[string]interface{}{
		"from": selected.From,
		"to":   selected.To,
	}
	if selected.Condition != "" {
		data["condition"] = selected.Condition
		data["negated"] = selected.Negated
	}
	if selected.LoopName != "" {
		data["loop"] = selected.LoopName
		data["iteration"] = loopCounters[selected.LoopName]
	}
	if err := e.emit(runID, store.EventEdgeSelected, "", data); err != nil {
		log.Printf("runtime: failed to emit edge_selected: %v", err)
	}

	return selected.To, nil
}

// ---------------------------------------------------------------------------
// Input resolution
// ---------------------------------------------------------------------------

// buildNodeInput constructs the input map for a node by looking at the
// edge `with` mappings that target this node. If no mappings exist, the
// run-level inputs are used as a starting point for the entry node.
func (e *Engine) buildNodeInput(nodeID string, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// Find the edge that targets this node and has `with` mappings.
	for _, edge := range e.workflow.Edges {
		if edge.To != nodeID || len(edge.With) == 0 {
			continue
		}
		// Only use mappings from an edge whose source has already produced output.
		if _, ok := outputs[edge.From]; !ok && edge.From != "" {
			continue
		}
		for _, dm := range edge.With {
			val := e.resolveMapping(dm, vars, outputs, runInputs)
			if val != nil {
				result[dm.Key] = val
			}
		}
		if len(result) > 0 {
			return result
		}
	}

	// Fallback: for the entry node use run-level inputs.
	if nodeID == e.workflow.Entry {
		for k, v := range runInputs {
			result[k] = v
		}
	}

	return result
}

// resolveMapping resolves a DataMapping's references to concrete values.
// For simplicity in the minimal runtime, if there is exactly one ref we
// return the resolved value directly; otherwise we return the raw template.
func (e *Engine) resolveMapping(dm *ir.DataMapping, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}) interface{} {
	if len(dm.Refs) == 1 {
		return e.resolveRef(dm.Refs[0], vars, outputs, runInputs)
	}
	// Multiple refs or no refs: return raw template as-is.
	return dm.Raw
}

// resolveRef resolves a single Ref to a concrete value.
func (e *Engine) resolveRef(ref *ir.Ref, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}) interface{} {
	switch ref.Kind {
	case ir.RefVars:
		if len(ref.Path) > 0 {
			return vars[ref.Path[0]]
		}
	case ir.RefInput:
		if len(ref.Path) > 0 {
			return runInputs[ref.Path[0]]
		}
	case ir.RefOutputs:
		if len(ref.Path) == 0 {
			return nil
		}
		nodeOut := outputs[ref.Path[0]]
		if nodeOut == nil {
			return nil
		}
		if len(ref.Path) == 1 {
			return nodeOut
		}
		return nodeOut[ref.Path[1]]
	case ir.RefArtifacts:
		// Artifacts are resolved via the same outputs map for now.
		if len(ref.Path) > 0 {
			return outputs[ref.Path[0]]
		}
	}
	return nil
}

// resolveVars builds the vars map from workflow variable defaults.
func (e *Engine) resolveVars(inputs map[string]interface{}) map[string]interface{} {
	vars := make(map[string]interface{})
	for name, v := range e.workflow.Vars {
		if v.HasDefault {
			vars[name] = v.Default
		}
	}
	// Inputs can override vars.
	for k, v := range inputs {
		if _, isVar := e.workflow.Vars[k]; isVar {
			vars[k] = v
		}
	}
	return vars
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mergeOutputs creates a merged view of parent and branch outputs.
// Branch outputs take precedence over parent outputs.
func mergeOutputs(parent, branch map[string]map[string]interface{}) map[string]map[string]interface{} {
	merged := make(map[string]map[string]interface{}, len(parent)+len(branch))
	for k, v := range parent {
		merged[k] = v
	}
	for k, v := range branch {
		merged[k] = v
	}
	return merged
}

// emit is a convenience wrapper for appending an event.
func (e *Engine) emit(runID string, typ store.EventType, nodeID string, data map[string]interface{}) error {
	_, err := e.store.AppendEvent(runID, store.Event{
		Type:   typ,
		NodeID: nodeID,
		Data:   data,
	})
	if err != nil {
		return fmt.Errorf("runtime: emit %s: %w", typ, err)
	}
	return nil
}

// emitBranch appends an event with a branch ID.
func (e *Engine) emitBranch(runID, branchID string, typ store.EventType, nodeID string, data map[string]interface{}) error {
	_, err := e.store.AppendEvent(runID, store.Event{
		Type:     typ,
		BranchID: branchID,
		NodeID:   nodeID,
		Data:     data,
	})
	if err != nil {
		return fmt.Errorf("runtime: emit %s (branch %s): %w", typ, branchID, err)
	}
	return nil
}

// failRun marks a run as failed and emits the run_failed event.
// If reason is already a RuntimeError it preserves the code and hint.
func (e *Engine) failRun(runID, nodeID, reason string) error {
	return e.failRunWithCode(runID, nodeID, reason, ErrCodeExecutionFailed, "")
}

// failRunErr marks a run as failed, preserving a structured error if present.
// Store/event errors are propagated so callers know whether the failure was persisted.
func (e *Engine) failRunErr(runID, nodeID string, origErr error) error {
	var rtErr *RuntimeError
	if errors.As(origErr, &rtErr) {
		if storeErr := e.store.UpdateRunStatus(runID, store.RunStatusFailed, rtErr.Message); storeErr != nil {
			log.Printf("runtime: failed to persist run failure status: %v", storeErr)
			return fmt.Errorf("runtime: node %q failed (%s) and could not persist failure: %w", nodeID, rtErr.Message, storeErr)
		}
		if err := e.emit(runID, store.EventRunFailed, nodeID, map[string]interface{}{
			"error": rtErr.Message,
			"code":  string(rtErr.Code),
		}); err != nil {
			log.Printf("runtime: failed to emit run_failed event: %v", err)
		}
		if rtErr.NodeID == "" {
			rtErr.NodeID = nodeID
		}
		return rtErr
	}
	return e.failRun(runID, nodeID, origErr.Error())
}

// failRunWithCode marks a run as failed and returns a structured RuntimeError.
// If the store update fails, the store error is returned instead of the runtime
// error so callers know the failure state was not persisted.
func (e *Engine) failRunWithCode(runID, nodeID, reason string, code ErrorCode, hint string) error {
	if storeErr := e.store.UpdateRunStatus(runID, store.RunStatusFailed, reason); storeErr != nil {
		log.Printf("runtime: failed to persist run failure status: %v", storeErr)
		return fmt.Errorf("runtime: node %q failed (%s) and could not persist failure: %w", nodeID, reason, storeErr)
	}
	if err := e.emit(runID, store.EventRunFailed, nodeID, map[string]interface{}{
		"error": reason,
		"code":  string(code),
	}); err != nil {
		log.Printf("runtime: failed to emit run_failed event: %v", err)
	}
	return &RuntimeError{
		Code:    code,
		Message: reason,
		NodeID:  nodeID,
		Hint:    hint,
	}
}

// handleContextDone handles context cancellation or deadline exceeded at
// the top-level execution loop. It distinguishes user cancellation
// (SIGINT / context.Canceled) from timeouts (context.DeadlineExceeded).
func (e *Engine) handleContextDone(runID, nodeID string, ctxErr error) error {
	if errors.Is(ctxErr, context.Canceled) {
		if err := e.store.UpdateRunStatus(runID, store.RunStatusCancelled, "run cancelled"); err != nil {
			log.Printf("runtime: failed to persist cancellation status: %v", err)
		}
		if err := e.emit(runID, store.EventRunCancelled, nodeID, map[string]interface{}{
			"reason": "context cancelled",
		}); err != nil {
			log.Printf("runtime: failed to emit run_cancelled event: %v", err)
		}
		return fmt.Errorf("%w: interrupted at node %s", ErrRunCancelled, nodeID)
	}
	// context.DeadlineExceeded → treat as a timeout failure.
	reason := fmt.Sprintf("timeout: %s", ctxErr.Error())
	if err := e.store.UpdateRunStatus(runID, store.RunStatusFailed, reason); err != nil {
		log.Printf("runtime: failed to persist timeout failure status: %v", err)
	}
	if err := e.emit(runID, store.EventRunFailed, nodeID, map[string]interface{}{
		"error": reason,
	}); err != nil {
		log.Printf("runtime: failed to emit run_failed event: %v", err)
	}
	return fmt.Errorf("runtime: %s at node %s", reason, nodeID)
}

// currentLoopIteration returns the current loop iteration for a node.
// If the node participates in multiple loops, returns the max counter.
// Returns 0 if the node is not in any loop.
func (e *Engine) currentLoopIteration(nodeID string, loopCounters map[string]int) int {
	maxIter := 0
	for _, edge := range e.workflow.Edges {
		if edge.LoopName == "" {
			continue
		}
		// Check if this node is on a loop-bearing edge.
		if edge.From == nodeID || edge.To == nodeID {
			if count, ok := loopCounters[edge.LoopName]; ok && count > maxIter {
				maxIter = count
			}
		}
	}
	return maxIter
}

// wrapContextErr wraps a context error for branch-level reporting.
func (e *Engine) wrapContextErr(ctxErr error) error {
	if errors.Is(ctxErr, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrRunCancelled, ctxErr)
	}
	return ctxErr
}

// ---------------------------------------------------------------------------
// Budget helpers
// ---------------------------------------------------------------------------

// checkBudgetBeforeExec checks time-based budget limits before a node runs.
func (e *Engine) checkBudgetBeforeExec(rs *runState, nodeID string) error {
	if rs.budget == nil {
		return nil
	}
	checks := rs.budget.Check()
	if exc := findExceeded(checks); exc != nil {
		_ = e.emit(rs.runID, store.EventBudgetExceeded, nodeID, map[string]interface{}{
			"dimension": exc.dimension,
			"used":      exc.used,
			"limit":     exc.limit,
		})
		return e.failRunWithCode(rs.runID, nodeID,
			fmt.Sprintf("budget exceeded: %s (%.0f/%.0f)", exc.dimension, exc.used, exc.limit),
			ErrCodeBudgetExceeded,
			fmt.Sprintf("increase the %s budget or optimize the workflow", exc.dimension))
	}
	return nil
}

// recordAndCheckBudget records usage from a node execution and emits
// budget_warning / budget_exceeded events as needed.
func (e *Engine) recordAndCheckBudget(rs *runState, nodeID string, output map[string]interface{}) error {
	if rs.budget == nil {
		return nil
	}

	tokens, costUSD := extractUsage(output)
	checks := rs.budget.RecordUsage(tokens, costUSD)

	// Emit warnings.
	for _, w := range findWarnings(checks) {
		_ = e.emit(rs.runID, store.EventBudgetWarning, nodeID, map[string]interface{}{
			"dimension": w.dimension,
			"used":      w.used,
			"limit":     w.limit,
		})
	}

	// Fail on exceeded.
	if exc := findExceeded(checks); exc != nil {
		_ = e.emit(rs.runID, store.EventBudgetExceeded, nodeID, map[string]interface{}{
			"dimension": exc.dimension,
			"used":      exc.used,
			"limit":     exc.limit,
		})
		return e.failRunWithCode(rs.runID, nodeID,
			fmt.Sprintf("budget exceeded: %s (%.0f/%.0f)", exc.dimension, exc.used, exc.limit),
			ErrCodeBudgetExceeded,
			fmt.Sprintf("increase the %s budget or optimize the workflow", exc.dimension))
	}

	return nil
}

// extractUsage reads conventional _tokens and _cost_usd keys from a node
// output. Returns zeros if absent.
func extractUsage(output map[string]interface{}) (tokens int, costUSD float64) {
	if v, ok := output["_tokens"]; ok {
		switch t := v.(type) {
		case int:
			tokens = t
		case float64:
			tokens = int(t)
		case int64:
			tokens = int(t)
		}
	}
	if v, ok := output["_cost_usd"]; ok {
		switch t := v.(type) {
		case float64:
			costUSD = t
		case int:
			costUSD = float64(t)
		}
	}
	return
}

// buildNodeFinishedData builds the data payload for a node_finished event,
// including usage metrics (_tokens, _cost_usd) and a snapshot of the output.
func buildNodeFinishedData(output map[string]interface{}) map[string]interface{} {
	if output == nil {
		return nil
	}
	data := map[string]interface{}{
		"output": output,
	}
	if v, ok := output["_tokens"]; ok {
		data["_tokens"] = v
	}
	if v, ok := output["_cost_usd"]; ok {
		data["_cost_usd"] = v
	}
	return data
}

// ---------------------------------------------------------------------------
// Workspace mutation safety
// ---------------------------------------------------------------------------

// readOnlyTools is the set of built-in tool names that are guaranteed to
// never modify the workspace. These are safe for parallel execution.
var readOnlyTools = map[string]bool{
	"git_diff":        true,
	"git_status":      true,
	"read_file":       true,
	"list_files":      true,
	"search_codebase": true,
	"tree":            true,
}

// isMutatingNode returns true if the node may modify the workspace.
// Tool nodes are always mutating. Agent/judge nodes are mutating only
// if they have at least one tool that is not in the read-only set.
func isMutatingNode(node *ir.Node) bool {
	if node.Kind == ir.NodeTool {
		return true
	}
	if node.Kind == ir.NodeAgent || node.Kind == ir.NodeJudge {
		for _, t := range node.Tools {
			if !readOnlyTools[t] {
				return true
			}
		}
	}
	return false
}

// branchContainsMutation walks from startNodeID to a join/terminal node
// and returns true if any node along the path may mutate the workspace.
func (e *Engine) branchContainsMutation(startNodeID string) bool {
	visited := map[string]bool{}
	queue := []string{startNodeID}
	for len(queue) > 0 {
		nodeID := queue[0]
		queue = queue[1:]
		if visited[nodeID] {
			continue
		}
		visited[nodeID] = true

		node, ok := e.workflow.Nodes[nodeID]
		if !ok {
			continue
		}
		// Stop walking at join or terminal nodes.
		if node.Kind == ir.NodeJoin || node.Kind == ir.NodeDone || node.Kind == ir.NodeFail {
			continue
		}
		if isMutatingNode(node) {
			return true
		}
		for _, edge := range e.workflow.Edges {
			if edge.From == nodeID {
				queue = append(queue, edge.To)
			}
		}
	}
	return false
}

// validateWorkspaceSafety checks that at most one branch in a fan-out
// contains mutating nodes. Returns an error if the topology is unsafe.
func (e *Engine) validateWorkspaceSafety(fanEdges []*ir.Edge) error {
	mutatingCount := 0
	var mutatingBranches []string
	for _, edge := range fanEdges {
		if e.branchContainsMutation(edge.To) {
			mutatingCount++
			mutatingBranches = append(mutatingBranches, edge.To)
		}
	}
	if mutatingCount > 1 {
		return &RuntimeError{
			Code:    ErrCodeWorkspaceSafety,
			Message: fmt.Sprintf("workspace safety violation: %d branches contain mutating nodes %v", mutatingCount, mutatingBranches),
			Hint:    "at most 1 mutating branch is allowed in parallel on the same workspace; move tool nodes to separate sequential steps",
		}
	}
	return nil
}
