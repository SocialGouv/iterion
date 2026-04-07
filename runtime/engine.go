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
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/ir"
	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/model"
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
	Execute(ctx context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error)
}

// Engine executes workflows. It supports sequential execution and
// parallel fan-out via bounded branch scheduling.
type Engine struct {
	workflow       *ir.Workflow
	store          *store.RunStore
	executor       NodeExecutor
	logger         *iterlog.Logger
	onNodeFinished func(nodeID string, output map[string]interface{})
	workflowHash   string // SHA-256 of the .iter source, set via WithWorkflowHash
}

// EngineOption configures an Engine.
type EngineOption func(*Engine)

// WithLogger sets a leveled logger for console output during execution.
func WithLogger(l *iterlog.Logger) EngineOption {
	return func(e *Engine) { e.logger = l }
}

// WithOnNodeFinished registers a callback invoked after each node finishes
// with the node's ID and output. The callback must be safe for concurrent use.
func WithOnNodeFinished(fn func(nodeID string, output map[string]interface{})) EngineOption {
	return func(e *Engine) { e.onNodeFinished = fn }
}

// WithWorkflowHash sets a hash of the .iter source so that Resume can
// detect if the workflow changed since the run was started.
func WithWorkflowHash(hash string) EngineOption {
	return func(e *Engine) { e.workflowHash = hash }
}

// New creates a new Engine for a raw workflow.
func New(wf *ir.Workflow, s *store.RunStore, exec NodeExecutor, opts ...EngineOption) *Engine {
	e := &Engine{workflow: wf, store: s, executor: exec}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// NewFromRecipe creates a new Engine by applying a recipe's presets onto
// the given workflow. The recipe merges preset variables, prompt overrides,
// and budget limits, producing a self-contained execution unit.
func NewFromRecipe(r *recipe.RecipeSpec, wf *ir.Workflow, s *store.RunStore, exec NodeExecutor, opts ...EngineOption) (*Engine, error) {
	applied, err := r.Apply(wf)
	if err != nil {
		return nil, fmt.Errorf("runtime: apply recipe %q: %w", r.Name, err)
	}
	e := &Engine{workflow: applied, store: s, executor: exec}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// runState holds the mutable runtime state passed through the execution loop.
type runState struct {
	runID              string
	runInputs          map[string]interface{}
	vars               map[string]interface{}
	outputs            map[string]map[string]interface{}
	artifacts          map[string]map[string]interface{} // publish name → output
	loopCounters       map[string]int
	roundRobinCounters map[string]int
	artifactVersions   map[string]int
	budget             *SharedBudget // shared across branches, nil if no budget
}

// branchResult holds the outcome of a single parallel branch.
type branchResult struct {
	branchID         string
	outputs          map[string]map[string]interface{}
	artifacts        map[string]map[string]interface{} // publish name → output
	artifactVersions map[string]int
	joinNodeID       string // the join node this branch converged to (empty if terminal)
	err              error
	eventErrors      int // count of event emission failures (best-effort events)
}

// Run executes the workflow. It creates a run, walks the graph from the
// entry node, and returns when a terminal node is reached, a human pause
// is hit (ErrRunPaused), or an error occurs.
func (e *Engine) Run(ctx context.Context, runID string, inputs map[string]interface{}) error {
	// Create run in store.
	run, err := e.store.CreateRun(runID, e.workflow.Name, inputs)
	if err != nil {
		return fmt.Errorf("runtime: create run: %w", err)
	}
	if e.workflowHash != "" {
		run.WorkflowHash = e.workflowHash
		if err := e.store.SaveRun(run); err != nil {
			return fmt.Errorf("runtime: save workflow hash: %w", err)
		}
	}

	// Emit run_started.
	if err := e.emit(runID, store.EventRunStarted, "", nil); err != nil {
		return err
	}

	rs := &runState{
		runID:              runID,
		runInputs:          inputs,
		vars:               e.resolveVars(inputs),
		outputs:            make(map[string]map[string]interface{}),
		artifacts:          make(map[string]map[string]interface{}),
		loopCounters:       make(map[string]int),
		roundRobinCounters: make(map[string]int),
		artifactVersions:   make(map[string]int),
		budget:             newSharedBudget(e.workflow.Budget),
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
	// Warn if the workflow source has changed since the run was started.
	if r.WorkflowHash != "" && e.workflowHash != "" && r.WorkflowHash != e.workflowHash {
		return fmt.Errorf("runtime: workflow source has changed since run %q was started (expected hash %s, got %s); re-run from scratch or use the original .iter file", runID, r.WorkflowHash[:12], e.workflowHash[:12])
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
	if pub := nodePublish(humanNode); pub != "" {
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
			"publish": pub,
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

	roundRobinCounters := cp.RoundRobinCounters
	if roundRobinCounters == nil {
		roundRobinCounters = make(map[string]int)
	}

	// Rebuild artifacts map from outputs for nodes that have publish.
	resumeArtifacts := make(map[string]map[string]interface{})
	for nodeID, output := range outputs {
		if n, ok := e.workflow.Nodes[nodeID]; ok {
			if pub := nodePublish(n); pub != "" {
				resumeArtifacts[pub] = output
			}
		}
	}

	rs := &runState{
		runID:              runID,
		runInputs:          r.Inputs,
		vars:               cp.Vars,
		outputs:            outputs,
		artifacts:          resumeArtifacts,
		loopCounters:       loopCounters,
		roundRobinCounters: roundRobinCounters,
		artifactVersions:   artifactVersions,
		budget:             newSharedBudget(e.workflow.Budget),
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
		switch node.(type) {
		case *ir.DoneNode:
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
		case *ir.FailNode:
			if err := e.emit(rs.runID, store.EventNodeStarted, currentNodeID, nil); err != nil {
				return err
			}
			if err := e.emit(rs.runID, store.EventNodeFinished, currentNodeID, nil); err != nil {
				return err
			}
			return e.failRun(rs.runID, currentNodeID, "workflow reached fail node")
		default:
			// non-terminal — continue below
		}

		// --- Human node ---
		if hn, ok := node.(*ir.HumanNode); ok {
			switch hn.Interaction {
			case ir.InteractionLLM:
				// Intentional fall-through: LLM interaction human nodes are
				// executed via the standard node path below (emit started →
				// budget check → executor.Execute → store output → emit
				// finished → select edge). The executor dispatches to
				// executeHumanLLM which handles model resolution and schema.
			case ir.InteractionLLMOrHuman:
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
				// InteractionHuman (default) and InteractionNone both pause.
				return e.pauseAtHuman(rs, currentNodeID, node)
			}
		}

		// --- Router nodes ---
		if rn, ok := node.(*ir.RouterNode); ok {
			switch rn.RouterMode {
			case ir.RouterFanOutAll:
				nextNodeID, err := e.execFanOut(ctx, rs, currentNodeID)
				if err != nil {
					return e.failRunErr(rs.runID, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			case ir.RouterRoundRobin:
				nextNodeID, err := e.execRoundRobin(ctx, rs, currentNodeID)
				if err != nil {
					return e.failRunErr(rs.runID, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			case ir.RouterLLM:
				nextNodeID, err := e.execLLMRouter(ctx, rs, currentNodeID)
				if err != nil {
					return e.failRunErr(rs.runID, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			}
			// RouterCondition falls through to normal execution path.
		}

		// --- Emit node_started ---
		if err := e.emit(rs.runID, store.EventNodeStarted, currentNodeID, map[string]interface{}{
			"kind": node.NodeKind().String(),
		}); err != nil {
			return err
		}

		// --- Check budget before execution ---
		if err := e.checkBudgetBeforeExec(rs, currentNodeID); err != nil {
			return err
		}

		// --- Build node input from edge mappings ---
		nodeInput := e.buildNodeInput(currentNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts)

		// --- Execute node ---
		output, err := e.executor.Execute(ctx, node, nodeInput)
		if err != nil {
			// Check if the delegate needs user interaction.
			var needsInput *model.ErrNeedsInteraction
			if errors.As(err, &needsInput) {
				return e.handleNeedsInteraction(ctx, rs, currentNodeID, node, needsInput)
			}
			return e.failRun(rs.runID, currentNodeID, fmt.Sprintf("node %q execution failed: %v", currentNodeID, err))
		}

		// Store output.
		rs.outputs[currentNodeID] = output

		// Record budget usage and check limits.
		if err := e.recordAndCheckBudget(rs, currentNodeID, output); err != nil {
			return err
		}

		// Persist artifact if node has publish.
		if pub := nodePublish(node); pub != "" {
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
			rs.artifacts[pub] = output

			if err := e.emit(rs.runID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
				"publish": pub,
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
		if e.onNodeFinished != nil {
			e.onNodeFinished(currentNodeID, output)
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
	routerInput := e.buildNodeInput(routerNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts)
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

	// Pre-compute convergence point so branches know where to stop.
	preComputedConvergence := e.findConvergencePoint(routerNodeID, fanEdges)

	// Deep-copy parent outputs and artifacts so branches can't mutate shared state.
	parentOutputs := deepCopyOutputs(rs.outputs)
	parentArtifacts := deepCopyOutputs(rs.artifacts)

	// Launch branches with bounded concurrency.
	sem := make(chan struct{}, maxParallel)
	resultsCh := make(chan *branchResult, len(fanEdges))

	for _, edge := range fanEdges {
		branchID := fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To)

		go func(edge *ir.Edge, branchID string) {
			sem <- struct{}{}        // acquire semaphore slot
			defer func() { <-sem }() // release

			// Recover from panics so a single branch doesn't crash the process.
			defer func() {
				if r := recover(); r != nil {
					resultsCh <- &branchResult{
						branchID: branchID,
						outputs:  make(map[string]map[string]interface{}),
						err:      fmt.Errorf("panic in branch %s: %v", branchID, r),
					}
				}
			}()

			result := e.execBranch(ctx, rs, branchID, edge, parentOutputs, parentArtifacts, preComputedConvergence)
			resultsCh <- result
		}(edge, branchID)
	}

	// Collect all results.
	results := make([]*branchResult, 0, len(fanEdges))
	for range fanEdges {
		results = append(results, <-resultsCh)
	}

	// Determine convergence point. Prefer the one reported by successful branches;
	// if all branches failed, discover it from the graph topology.
	convergenceNodeID := ""
	for _, r := range results {
		if r.joinNodeID != "" {
			if convergenceNodeID == "" {
				convergenceNodeID = r.joinNodeID
			} else if convergenceNodeID != r.joinNodeID {
				return "", fmt.Errorf("branches converge to different nodes: %s vs %s", convergenceNodeID, r.joinNodeID)
			}
		}
	}
	if convergenceNodeID == "" {
		// All branches failed before reaching convergence. Walk the graph
		// from each fan-out target to find the downstream convergence point.
		convergenceNodeID = e.findConvergencePoint(routerNodeID, fanEdges)
		if convergenceNodeID == "" {
			return "", fmt.Errorf("no convergence point found after fan_out from %s", routerNodeID)
		}
	}

	// Process convergence.
	return e.processConvergence(rs, convergenceNodeID, results)
}

// execRoundRobin handles a round_robin router node by selecting a single
// outgoing edge based on a cyclical counter. Unlike fan_out_all, it does
// not spawn parallel branches — it picks one target and returns to the
// main execution loop.
func (e *Engine) execRoundRobin(ctx context.Context, rs *runState, routerNodeID string) (string, error) {
	// Collect unconditional outgoing edges from the router.
	var edges []*ir.Edge
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID && edge.Condition == "" {
			edges = append(edges, edge)
		}
	}
	if len(edges) == 0 {
		return "", fmt.Errorf("round_robin router %q has no outgoing edges", routerNodeID)
	}

	// Cyclical selection: counter % len(edges).
	counter := rs.roundRobinCounters[routerNodeID]
	selected := edges[counter%len(edges)]
	rs.roundRobinCounters[routerNodeID] = counter + 1

	// Emit router node_started with round-robin metadata.
	if err := e.emit(rs.runID, store.EventNodeStarted, routerNodeID, map[string]interface{}{
		"kind":              "router",
		"mode":              "round_robin",
		"round_robin_index": counter,
		"selected_target":   selected.To,
	}); err != nil {
		return "", err
	}

	// Router is a pass-through: its output = its input from incoming edges.
	routerInput := e.buildNodeInput(routerNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts)
	rs.outputs[routerNodeID] = routerInput

	// Emit router node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, routerNodeID, nil); err != nil {
		return "", err
	}

	return selected.To, nil
}

// ---------------------------------------------------------------------------
// LLM Router — LLM-based route selection
// ---------------------------------------------------------------------------

// execLLMRouter handles an LLM router node by calling the LLM to decide
// which outgoing edge(s) to take. For single mode, it picks one target;
// for multi mode, it fans out to the selected subset.
func (e *Engine) execLLMRouter(ctx context.Context, rs *runState, routerNodeID string) (string, error) {
	node := e.workflow.Nodes[routerNodeID]
	rn, ok := node.(*ir.RouterNode)
	if !ok {
		return "", fmt.Errorf("runtime: node %q is not a RouterNode", routerNodeID)
	}

	// Emit node_started.
	if err := e.emit(rs.runID, store.EventNodeStarted, routerNodeID, map[string]interface{}{
		"kind": "router",
		"mode": "llm",
	}); err != nil {
		return "", err
	}

	// Build router input.
	routerInput := e.buildNodeInput(routerNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts)

	// Collect outgoing edge targets as candidates.
	// NOTE: order follows edge declaration order in the .iter file, which the
	// LLM sees in its prompt. This is deterministic but may introduce ordering
	// bias in the LLM's selection.
	var candidates []string
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID {
			candidates = append(candidates, edge.To)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("llm router %q has no outgoing edges", routerNodeID)
	}

	// Inject candidates into input for the executor.
	routerInput["_route_candidates"] = candidates

	// Check budget before LLM call.
	if err := e.checkBudgetBeforeExec(rs, routerNodeID); err != nil {
		return "", err
	}

	// Execute LLM call via the executor.
	output, err := e.executor.Execute(ctx, node, routerInput)
	if err != nil {
		return "", fmt.Errorf("llm router %q: %w", routerNodeID, err)
	}

	rs.outputs[routerNodeID] = output

	// Record budget usage and check limits.
	if err := e.recordAndCheckBudget(rs, routerNodeID, output); err != nil {
		return "", err
	}

	// Dispatch based on single/multi mode.
	if rn.RouterMulti {
		return e.execLLMRouterMulti(ctx, rs, routerNodeID, output, candidates)
	}
	return e.execLLMRouterSingle(rs, routerNodeID, output, candidates)
}

// execLLMRouterSingle handles single-route LLM selection.
func (e *Engine) execLLMRouterSingle(rs *runState, routerNodeID string, output map[string]interface{}, candidates []string) (string, error) {
	selected, ok := output["selected_route"].(string)
	if !ok || selected == "" {
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q did not produce a valid selected_route", routerNodeID),
			NodeID:  routerNodeID,
		}
	}

	// Validate selection is a valid candidate.
	valid := false
	for _, c := range candidates {
		if c == selected {
			valid = true
			break
		}
	}
	if !valid {
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q selected %q which is not a valid target (candidates: %v)", routerNodeID, selected, candidates),
			NodeID:  routerNodeID,
		}
	}

	reasoning, _ := output["reasoning"].(string)

	// Emit node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, routerNodeID, map[string]interface{}{
		"selected_route": selected,
		"reasoning":      reasoning,
	}); err != nil {
		return "", err
	}

	// Emit edge_selected.
	if err := e.emit(rs.runID, store.EventEdgeSelected, routerNodeID, map[string]interface{}{
		"from": routerNodeID,
		"to":   selected,
	}); err != nil {
		return "", err
	}

	return selected, nil
}

// execLLMRouterMulti handles multi-route LLM selection by fanning out
// to the LLM-selected subset of outgoing edges.
func (e *Engine) execLLMRouterMulti(ctx context.Context, rs *runState, routerNodeID string, output map[string]interface{}, candidates []string) (string, error) {
	selectedRaw, ok := output["selected_routes"]
	if !ok {
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q did not produce selected_routes", routerNodeID),
			NodeID:  routerNodeID,
		}
	}

	// Parse selected routes from the output.
	var selected []string
	switch v := selectedRaw.(type) {
	case []interface{}:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return "", &RuntimeError{
					Code:    ErrCodeExecutionFailed,
					Message: fmt.Sprintf("llm router %q: selected_routes contains non-string element", routerNodeID),
					NodeID:  routerNodeID,
				}
			}
			selected = append(selected, s)
		}
	case []string:
		selected = v
	default:
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q: selected_routes is %T, expected array", routerNodeID, selectedRaw),
			NodeID:  routerNodeID,
		}
	}

	if len(selected) == 0 {
		return "", &RuntimeError{
			Code:    ErrCodeExecutionFailed,
			Message: fmt.Sprintf("llm router %q selected zero routes", routerNodeID),
			NodeID:  routerNodeID,
		}
	}

	// Validate all selections are valid candidates.
	candidateSet := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		candidateSet[c] = true
	}
	for _, s := range selected {
		if !candidateSet[s] {
			return "", &RuntimeError{
				Code:    ErrCodeExecutionFailed,
				Message: fmt.Sprintf("llm router %q selected %q which is not a valid target (candidates: %v)", routerNodeID, s, candidates),
				NodeID:  routerNodeID,
			}
		}
	}

	reasoning, _ := output["reasoning"].(string)

	// Emit node_finished.
	if err := e.emit(rs.runID, store.EventNodeFinished, routerNodeID, map[string]interface{}{
		"selected_routes": selected,
		"reasoning":       reasoning,
	}); err != nil {
		return "", err
	}

	// Filter workflow edges to only the LLM-selected targets.
	selectedSet := make(map[string]bool, len(selected))
	for _, s := range selected {
		selectedSet[s] = true
	}
	var fanEdges []*ir.Edge
	for _, edge := range e.workflow.Edges {
		if edge.From == routerNodeID && selectedSet[edge.To] {
			fanEdges = append(fanEdges, edge)
		}
	}

	// Validate workspace safety.
	if err := e.validateWorkspaceSafety(fanEdges); err != nil {
		return "", err
	}

	// Determine concurrency limit from budget.
	maxParallel := len(fanEdges)
	if e.workflow.Budget != nil && e.workflow.Budget.MaxParallelBranches > 0 && e.workflow.Budget.MaxParallelBranches < maxParallel {
		maxParallel = e.workflow.Budget.MaxParallelBranches
	}

	// Pre-compute convergence point so branches know where to stop.
	llmPreComputedConvergence := e.findConvergencePoint(routerNodeID, fanEdges)

	// Deep-copy parent outputs and artifacts so branches can't mutate shared state.
	parentOutputs := deepCopyOutputs(rs.outputs)
	parentArtifacts := deepCopyOutputs(rs.artifacts)

	// Launch branches with bounded concurrency.
	sem := make(chan struct{}, maxParallel)
	resultsCh := make(chan *branchResult, len(fanEdges))

	for _, edge := range fanEdges {
		branchID := fmt.Sprintf("branch_%s_%s", routerNodeID, edge.To)

		go func(edge *ir.Edge, branchID string) {
			sem <- struct{}{}
			defer func() { <-sem }()

			// Recover from panics so a single branch doesn't crash the process.
			defer func() {
				if r := recover(); r != nil {
					resultsCh <- &branchResult{
						branchID: branchID,
						outputs:  make(map[string]map[string]interface{}),
						err:      fmt.Errorf("panic in branch %s: %v", branchID, r),
					}
				}
			}()

			result := e.execBranch(ctx, rs, branchID, edge, parentOutputs, parentArtifacts, llmPreComputedConvergence)
			resultsCh <- result
		}(edge, branchID)
	}

	// Collect all results.
	results := make([]*branchResult, 0, len(fanEdges))
	for range fanEdges {
		results = append(results, <-resultsCh)
	}

	// Determine convergence point.
	convergenceNodeID := ""
	for _, r := range results {
		if r.joinNodeID != "" {
			if convergenceNodeID == "" {
				convergenceNodeID = r.joinNodeID
			} else if convergenceNodeID != r.joinNodeID {
				return "", fmt.Errorf("branches converge to different nodes: %s vs %s", convergenceNodeID, r.joinNodeID)
			}
		}
	}
	if convergenceNodeID == "" {
		convergenceNodeID = e.findConvergencePoint(routerNodeID, fanEdges)
		if convergenceNodeID == "" {
			return "", fmt.Errorf("no convergence point found after llm router fan-out from %s", routerNodeID)
		}
	}

	return e.processConvergence(rs, convergenceNodeID, results)
}

// ---------------------------------------------------------------------------
// Branch execution
// ---------------------------------------------------------------------------

// execBranch runs a single parallel branch starting from the target of
// the given edge. It executes nodes sequentially until it reaches a
// convergence point, a terminal node, or encounters an error.
// convergenceNodeID is the pre-computed convergence point (may be empty
// if unknown; in that case, AwaitMode on individual nodes is checked).
func (e *Engine) execBranch(ctx context.Context, rs *runState, branchID string, startEdge *ir.Edge, parentOutputs map[string]map[string]interface{}, parentArtifacts map[string]map[string]interface{}, convergenceNodeID string) *branchResult {
	result := &branchResult{
		branchID:         branchID,
		outputs:          make(map[string]map[string]interface{}),
		artifacts:        make(map[string]map[string]interface{}),
		artifactVersions: make(map[string]int),
	}

	runID := rs.runID
	vars := rs.vars
	runInputs := rs.runInputs

	// Emit branch_started (best-effort — branch can proceed without the event).
	if err := e.emitBranch(runID, branchID, store.EventBranchStarted, startEdge.To, nil); err != nil {
		log.Printf("runtime: branch %s: failed to emit branch_started: %v", branchID, err)
		result.eventErrors++
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

		// Stop at convergence point — the branch has reached the
		// pre-computed node where parallel branches reconverge.
		if convergenceNodeID != "" && currentNodeID == convergenceNodeID {
			result.joinNodeID = currentNodeID
			return result
		}

		// Stop at terminal nodes within a branch.
		switch node.(type) {
		case *ir.DoneNode:
			return result
		case *ir.FailNode:
			result.err = fmt.Errorf("branch %s reached fail node %q", branchID, currentNodeID)
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
					result.eventErrors++
				}
				result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
				return result
			}
		}

		// Emit node_started.
		if err := e.emitBranch(runID, branchID, store.EventNodeStarted, currentNodeID, map[string]interface{}{
			"kind": node.NodeKind().String(),
		}); err != nil {
			log.Printf("runtime: branch %s: failed to emit node_started: %v", branchID, err)
			result.eventErrors++
		}

		// Build input: merge parent outputs with branch-local outputs so
		// refs to upstream nodes (before the router) still resolve.
		merged := mergeOutputs(parentOutputs, result.outputs)
		mergedArt := mergeOutputs(parentArtifacts, result.artifacts)
		nodeInput := e.buildNodeInput(currentNodeID, vars, merged, runInputs, mergedArt)

		// Execute.
		output, err := e.executor.Execute(ctx, node, nodeInput)
		if err != nil {
			result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
			if emitErr := e.emitBranch(runID, branchID, store.EventNodeFinished, currentNodeID, map[string]interface{}{
				"error": err.Error(),
			}); emitErr != nil {
				log.Printf("runtime: branch %s: failed to emit node_finished: %v", branchID, emitErr)
				result.eventErrors++
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
					result.eventErrors++
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
					result.eventErrors++
				}
				result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
				return result
			}
		}

		// Persist artifact if node has publish.
		if pub := nodePublish(node); pub != "" {
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
			result.artifacts[pub] = output
			if err := e.emitBranch(runID, branchID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
				"publish": pub,
				"version": version,
			}); err != nil {
				log.Printf("runtime: branch %s: failed to emit artifact_written: %v", branchID, err)
				result.eventErrors++
			}
		}

		// Emit node_finished with usage data.
		if err := e.emitBranch(runID, branchID, store.EventNodeFinished, currentNodeID, buildNodeFinishedData(output)); err != nil {
			log.Printf("runtime: branch %s: failed to emit node_finished: %v", branchID, err)
			result.eventErrors++
		}
		if e.onNodeFinished != nil {
			e.onNodeFinished(currentNodeID, output)
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

// processConvergence aggregates branch results according to the convergence
// node's await strategy, merges outputs into the run state, builds the
// convergence node's input from multi-edge with-mappings, and returns
// the convergence node ID for the main loop to continue execution.
func (e *Engine) processConvergence(rs *runState, convergenceNodeID string, results []*branchResult) (string, error) {
	convNode, ok := e.workflow.Nodes[convergenceNodeID]
	if !ok {
		return "", fmt.Errorf("convergence node %q not found", convergenceNodeID)
	}

	// Determine await strategy: use node's explicit setting, default to wait_all.
	strategy := nodeAwaitMode(convNode)
	if strategy == ir.AwaitNone {
		strategy = ir.AwaitWaitAll
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

	// Apply await strategy.
	switch strategy {
	case ir.AwaitWaitAll:
		if len(failedBranches) > 0 {
			return "", fmt.Errorf("convergence at %s (wait_all): %d branch(es) failed: %v",
				convergenceNodeID, len(failedBranches), failedBranches[0]["error"])
		}
	case ir.AwaitBestEffort:
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
		for name, output := range r.artifacts {
			rs.artifacts[name] = output
		}
		for nodeID, version := range r.artifactVersions {
			rs.artifactVersions[nodeID] = version
		}
	}

	// Add failed branches metadata to outputs so it's available via with-mappings.
	if len(failedBranches) > 0 {
		// Expose as a special output on the convergence node.
		if rs.outputs[convergenceNodeID] == nil {
			rs.outputs[convergenceNodeID] = make(map[string]interface{})
		}
		rs.outputs[convergenceNodeID]["_failed_branches"] = failedBranches
	}

	// Emit convergence_ready event.
	convData := map[string]interface{}{
		"strategy": strategy.String(),
	}
	if len(failedBranches) > 0 {
		convData["failed_branches"] = failedBranches
	}
	if err := e.emit(rs.runID, store.EventJoinReady, convergenceNodeID, convData); err != nil {
		log.Printf("runtime: failed to emit convergence_ready: %v", err)
	}

	// Return the convergence node ID — the main loop will execute it normally.
	return convergenceNodeID, nil
}

// findConvergencePoint walks outgoing edges from the router's targets to
// find a downstream convergence point (a node with AwaitMode != AwaitNone,
// or a node that receives edges from multiple distinct sources).
// Terminal nodes (done/fail) can be convergence points when multiple
// branches target them directly.
// This is also called pre-emptively before branches start so that each
// branch knows where to stop.
func (e *Engine) findConvergencePoint(routerNodeID string, fanEdges []*ir.Edge) string {
	// Build in-degree map: count distinct sources per target.
	inSources := make(map[string]map[string]bool)
	for _, edge := range e.workflow.Edges {
		if _, ok := inSources[edge.To]; !ok {
			inSources[edge.To] = make(map[string]bool)
		}
		inSources[edge.To][edge.From] = true
	}

	// BFS from each fan-out target to find a convergence point.
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
			// Convergence point: explicitly marked OR has multiple distinct incoming sources.
			if nodeAwaitMode(node) != ir.AwaitNone || len(inSources[nodeID]) > 1 {
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
func (e *Engine) execAutoOrPauseHuman(ctx context.Context, rs *runState, nodeID string, node ir.Node) (bool, error) {
	// Emit node_started.
	if err := e.emit(rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind": node.NodeKind().String(),
	}); err != nil {
		return false, err
	}

	// Check budget.
	if err := e.checkBudgetBeforeExec(rs, nodeID); err != nil {
		return false, err
	}

	// Build input and execute LLM.
	nodeInput := e.buildNodeInput(nodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts)
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
	if pub := nodePublish(node); pub != "" {
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
		rs.artifacts[pub] = output
		_ = e.emit(rs.runID, store.EventArtifactWritten, nodeID, map[string]interface{}{
			"publish": pub,
			"version": version,
		})
	}

	// Emit node_finished.
	nodeFinishedData := buildNodeFinishedData(output)
	if err := e.emit(rs.runID, store.EventNodeFinished, nodeID, nodeFinishedData); err != nil {
		return false, err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(nodeID, output)
	}

	return false, nil
}

// ---------------------------------------------------------------------------
// Human pause
// ---------------------------------------------------------------------------

// pauseAtHuman suspends the run at a human node: persists an interaction,
// saves checkpoint state, and returns ErrRunPaused.
func (e *Engine) pauseAtHuman(rs *runState, nodeID string, node ir.Node) error {
	// Emit node_started for the human node.
	if err := e.emit(rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind": node.NodeKind().String(),
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
	questions := e.buildNodeInput(nodeID, rs.vars, rs.outputs, nil, rs.artifacts)

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
		NodeID:             nodeID,
		InteractionID:      interactionID,
		Outputs:            rs.outputs,
		LoopCounters:       rs.loopCounters,
		RoundRobinCounters: rs.roundRobinCounters,
		ArtifactVersions:   rs.artifactVersions,
		Vars:               rs.vars,
	}
	if err := e.store.PauseRun(rs.runID, cp); err != nil {
		return fmt.Errorf("runtime: pause run: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Delegate interaction handling
// ---------------------------------------------------------------------------

// handleNeedsInteraction is called when a delegate or LLM signals it needs
// user input. The behavior depends on the node's InteractionMode:
//   - InteractionHuman: pause the workflow for human input
//   - InteractionLLM: auto-respond using the interaction model
//   - InteractionLLMOrHuman: LLM decides whether to respond or escalate
func (e *Engine) handleNeedsInteraction(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction) error {
	switch nodeInteraction(node) {
	case ir.InteractionHuman:
		return e.pauseForBackendInteraction(rs, nodeID, ni)

	case ir.InteractionLLM:
		// TODO(phase5): invoke interaction_model to auto-respond,
		// then re-invoke the backend with the answers.
		// For now, fall through to pause.
		return e.pauseForBackendInteraction(rs, nodeID, ni)

	case ir.InteractionLLMOrHuman:
		// TODO(phase5): invoke interaction_model to decide whether
		// to auto-respond or escalate to human.
		// For now, fall through to pause.
		return e.pauseForBackendInteraction(rs, nodeID, ni)

	default:
		// InteractionNone should not reach here (executor wouldn't return ErrNeedsInteraction).
		return fmt.Errorf("runtime: node %q received interaction request but has interaction: none", nodeID)
	}
}

// pauseForBackendInteraction creates an interaction record and pauses the
// workflow, saving the backend's session ID for re-invocation on resume.
func (e *Engine) pauseForBackendInteraction(rs *runState, nodeID string, ni *model.ErrNeedsInteraction) error {
	interactionID := fmt.Sprintf("%s_%s", rs.runID, nodeID)
	if loopIter := e.currentLoopIteration(nodeID, rs.loopCounters); loopIter > 0 {
		interactionID = fmt.Sprintf("%s_%s_%d", rs.runID, nodeID, loopIter)
	}

	interaction := &store.Interaction{
		ID:          interactionID,
		RunID:       rs.runID,
		NodeID:      nodeID,
		RequestedAt: time.Now().UTC(),
		Questions:   ni.Questions,
	}
	if err := e.store.WriteInteraction(interaction); err != nil {
		return fmt.Errorf("runtime: write interaction: %w", err)
	}

	if err := e.emit(rs.runID, store.EventHumanInputRequested, nodeID, map[string]interface{}{
		"interaction_id": interactionID,
		"questions":      ni.Questions,
		"source":         "delegate",
		"backend":        ni.Backend,
	}); err != nil {
		return err
	}

	if err := e.emit(rs.runID, store.EventRunPaused, nodeID, nil); err != nil {
		return err
	}

	cp := &store.Checkpoint{
		NodeID:             nodeID,
		InteractionID:      interactionID,
		Outputs:            rs.outputs,
		LoopCounters:       rs.loopCounters,
		RoundRobinCounters: rs.roundRobinCounters,
		ArtifactVersions:   rs.artifactVersions,
		Vars:               rs.vars,
		BackendSessionID:   ni.SessionID,
		BackendName:        ni.Backend,
	}
	if err := e.store.PauseRun(rs.runID, cp); err != nil {
		return fmt.Errorf("runtime: pause run: %w", err)
	}

	return ErrRunPaused
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
// edge `with` mappings that target this node. For convergence points,
// mappings from ALL resolved incoming edges are merged. If no mappings
// exist, the run-level inputs are used for the entry node.
func (e *Engine) buildNodeInput(nodeID string, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// Merge with-mappings from ALL edges targeting this node whose source
	// has already produced output.
	for _, edge := range e.workflow.Edges {
		if edge.To != nodeID || len(edge.With) == 0 {
			continue
		}
		// Only use mappings from an edge whose source has already produced output.
		if _, ok := outputs[edge.From]; !ok && edge.From != "" {
			continue
		}

		// Build effective input context: {{input.X}} in with-mappings should
		// resolve from the edge source's output (e.g. a router's pass-through
		// input) with run-level inputs as fallback.
		effectiveInputs := runInputs
		if sourceOut := outputs[edge.From]; sourceOut != nil {
			effectiveInputs = make(map[string]interface{}, len(runInputs)+len(sourceOut))
			for k, v := range runInputs {
				effectiveInputs[k] = v
			}
			for k, v := range sourceOut {
				effectiveInputs[k] = v
			}
		}

		for _, dm := range edge.With {
			val := e.resolveMapping(dm, vars, outputs, effectiveInputs, artifacts)
			if val != nil {
				result[dm.Key] = val
			}
		}
	}

	if len(result) > 0 {
		return result
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
func (e *Engine) resolveMapping(dm *ir.DataMapping, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}) interface{} {
	if len(dm.Refs) == 1 {
		return e.resolveRef(dm.Refs[0], vars, outputs, runInputs, artifacts)
	}
	// Multiple refs or no refs: return raw template as-is.
	return dm.Raw
}

// resolveRef resolves a single Ref to a concrete value.
func (e *Engine) resolveRef(ref *ir.Ref, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}) interface{} {
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
		if len(ref.Path) > 0 {
			return artifacts[ref.Path[0]]
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

// deepCopyOutputs creates a deep copy of the outputs map so that concurrent
// branches cannot mutate shared parent state.
func deepCopyOutputs(src map[string]map[string]interface{}) map[string]map[string]interface{} {
	dst := make(map[string]map[string]interface{}, len(src))
	for k, v := range src {
		inner := make(map[string]interface{}, len(v))
		for ik, iv := range v {
			inner[ik] = iv
		}
		dst[k] = inner
	}
	return dst
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
	e.logEvent(typ, nodeID, "", data)
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
	e.logEvent(typ, nodeID, branchID, data)
	return nil
}

// logEvent writes a human-friendly console log for a given event type.
func (e *Engine) logEvent(typ store.EventType, nodeID, branchID string, data map[string]interface{}) {
	l := e.logger
	if l == nil {
		return
	}

	prefix := nodeID
	if branchID != "" {
		prefix = branchID + "/" + nodeID
	}

	switch typ {
	case store.EventRunStarted:
		l.Logf(iterlog.LevelInfo, "🚀", "Run started: %s", e.workflow.Name)
	case store.EventRunFinished:
		l.Logf(iterlog.LevelInfo, "✅", "Run finished")
	case store.EventRunFailed:
		reason := ""
		if data != nil {
			if r, ok := data["error"].(string); ok {
				reason = r
			}
		}
		l.Error("Run failed: %s", reason)
	case store.EventRunCancelled:
		l.Error("Run cancelled")
	case store.EventNodeStarted:
		kind := ""
		if data != nil {
			if k, ok := data["kind"].(string); ok {
				kind = k
			}
		}
		l.Logf(iterlog.LevelInfo, "📍", "Node started: %s [%s]", prefix, kind)
	case store.EventNodeFinished:
		tokens := ""
		if data != nil {
			if t, ok := data["_tokens"]; ok {
				tokens = fmt.Sprintf(" (%v tokens)", t)
			}
		}
		l.Logf(iterlog.LevelInfo, "✅", "Node finished: %s%s", prefix, tokens)
		if data != nil {
			if preview := formatOutputPreview(data); preview != "" {
				l.Logf(iterlog.LevelInfo, "💬", "%s", preview)
			}
		}
	case store.EventEdgeSelected:
		to := ""
		cond := ""
		if data != nil {
			if t, ok := data["to"].(string); ok {
				to = t
			}
			if c, ok := data["condition"].(string); ok {
				cond = c
			}
		}
		if cond != "" {
			l.Logf(iterlog.LevelInfo, "➡️ ", "Edge: %s → %s (condition: %s)", nodeID, to, cond)
		} else {
			l.Logf(iterlog.LevelInfo, "➡️ ", "Edge: %s → %s", nodeID, to)
		}
	case store.EventBranchStarted:
		l.Logf(iterlog.LevelInfo, "🔀", "Branch started: %s", branchID)
	case store.EventJoinReady:
		l.Logf(iterlog.LevelInfo, "🔗", "Join ready: %s", nodeID)
	case store.EventArtifactWritten:
		l.Logf(iterlog.LevelInfo, "💾", "Artifact written: %s", nodeID)
	case store.EventHumanInputRequested:
		l.Logf(iterlog.LevelInfo, "👤", "Human input requested: %s", nodeID)
	case store.EventRunPaused:
		l.Logf(iterlog.LevelInfo, "⏸️ ", "Run paused (waiting for human input)")
	case store.EventRunResumed:
		l.Logf(iterlog.LevelInfo, "▶️ ", "Run resumed")
	case store.EventHumanAnswersRecorded:
		l.Logf(iterlog.LevelInfo, "📝", "Human answers recorded: %s", nodeID)
	case store.EventBudgetWarning:
		l.Warn("Budget warning: %s", nodeID)
	case store.EventBudgetExceeded:
		l.Warn("Budget exceeded: %s", nodeID)
	}
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

// formatOutputPreview builds a human-readable single-line summary of a
// node_finished event's data. It returns an empty string when there is
// nothing meaningful to display.
func formatOutputPreview(data map[string]interface{}) string {
	if data == nil {
		return ""
	}

	// Regular nodes wrap output under data["output"]; router events put
	// fields like selected_route/reasoning directly in data.
	output, ok := data["output"].(map[string]interface{})
	if !ok {
		output = data
	}

	// Collect user-visible fields (skip internal _-prefixed keys).
	type kv struct {
		key string
		val interface{}
	}

	var fields []kv
	for k, v := range output {
		if strings.HasPrefix(k, "_") {
			continue
		}
		fields = append(fields, kv{k, v})
	}
	if len(fields) == 0 {
		return ""
	}

	// Special case: text-only output — show a preview of the text.
	if len(fields) == 1 && fields[0].key == "text" {
		s, _ := fields[0].val.(string)
		if s == "" {
			return ""
		}
		return truncatePreview(s, 1000)
	}

	// Priority ordering for known fields.
	priority := map[string]int{
		"verdict":         0,
		"approved":        1,
		"selected_route":  2,
		"selected_routes": 3,
		"reasoning":       10,
		"feedback":        11,
		"summary":         12,
		"text":            13,
	}
	sort.SliceStable(fields, func(i, j int) bool {
		pi, oki := priority[fields[i].key]
		pj, okj := priority[fields[j].key]
		if oki && okj {
			return pi < pj
		}
		if oki {
			return true
		}
		if okj {
			return false
		}
		return fields[i].key < fields[j].key
	})

	// Format each field as "key: value".
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%s: %s", f.key, formatFieldValue(f.val)))
	}

	result := strings.Join(parts, " | ")
	return truncatePreview(result, 1200)
}

// formatFieldValue formats a single output field value for display.
func formatFieldValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return truncatePreview(val, 200)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case []interface{}:
		items := make([]string, 0, len(val))
		for _, item := range val {
			s := fmt.Sprintf("%v", item)
			if len(s) > 80 {
				s = s[:80] + "..."
			}
			items = append(items, s)
			if len(items) >= 5 {
				items = append(items, fmt.Sprintf("... (%d total)", len(val)))
				break
			}
		}
		return "[" + strings.Join(items, ", ") + "]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// truncatePreview returns s truncated to maxLen characters, with "..."
// appended if truncated. Newlines are replaced with spaces for single-line display.
func truncatePreview(s string, maxLen int) string {
	// Replace newlines with spaces.
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// Node field accessors — thin wrappers over ir.Node* exported helpers.
// ---------------------------------------------------------------------------

var (
	nodePublish     = ir.NodePublish
	nodeAwaitMode   = ir.NodeAwaitMode
	nodeInteraction = ir.NodeInteraction
	isTerminalNode  = ir.IsTerminalNode
)

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
// Nodes with Readonly=true are never considered mutating.
func isMutatingNode(node ir.Node) bool {
	switch n := node.(type) {
	case *ir.ToolNode:
		return true
	case *ir.AgentNode:
		if n.Readonly {
			return false
		}
		for _, t := range n.Tools {
			if !readOnlyTools[t] {
				return true
			}
		}
	case *ir.JudgeNode:
		if n.Readonly {
			return false
		}
		for _, t := range n.Tools {
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
		// Stop walking at convergence points or terminal nodes.
		if nodeAwaitMode(node) != ir.AwaitNone || isTerminalNode(node) {
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
