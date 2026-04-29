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
	workflow         *ir.Workflow
	store            *store.RunStore
	executor         NodeExecutor
	logger           *iterlog.Logger
	onNodeFinished   func(nodeID string, output map[string]interface{})
	onEvent          func(evt store.Event) // optional observer fired after every successful append
	recoveryDispatch RecoveryDispatch      // optional; consulted on node execution failure
	workflowHash     string                // SHA-256 of the .iter source, set via WithWorkflowHash
	validateOutputs  bool                  // when true, validate node outputs against declared schemas
	forceResume      bool                  // when true, skip workflow hash check on resume
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

// WithEventObserver registers a callback invoked after every successful
// event append (including branch_started/finished). It must be safe for
// concurrent use; the callback runs in the goroutine that emitted the
// event. Use it to fan out events to non-store observers (Prometheus,
// custom metrics) without changing the persistence layer.
func WithEventObserver(fn func(evt store.Event)) EngineOption {
	return func(e *Engine) { e.onEvent = fn }
}

// WithRecoveryDispatch installs the dispatcher consulted when a node's
// executor returns an error. The dispatcher decides between retry,
// compact-and-retry, pause for human, and terminal failure. When unset,
// every error falls straight through to failed_resumable (legacy
// behaviour).
//
// Build the dispatcher with recovery.Dispatch(recovery.DefaultRecipes()).
func WithRecoveryDispatch(d RecoveryDispatch) EngineOption {
	return func(e *Engine) { e.recoveryDispatch = d }
}

// WithWorkflowHash sets a hash of the .iter source so that Resume can
// detect if the workflow changed since the run was started.
func WithWorkflowHash(hash string) EngineOption {
	return func(e *Engine) { e.workflowHash = hash }
}

// WithForceResume allows resuming a run even when the workflow source has
// changed since the run was started. The hash mismatch is logged as a warning
// instead of causing an error.
func WithForceResume(force bool) EngineOption {
	return func(e *Engine) { e.forceResume = force }
}

// WithOutputValidation enables post-execution validation of node outputs
// against their declared output schemas. When enabled, a node whose output
// does not conform to its schema will cause the run to fail immediately.
func WithOutputValidation(enabled bool) EngineOption {
	return func(e *Engine) { e.validateOutputs = enabled }
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

	// nodeAttempts counts prior failed attempts per (nodeID, ErrorCode)
	// so the recovery dispatcher can apply per-class retry budgets and
	// reset them after a successful execution. Keys are created lazily.
	nodeAttempts map[string]map[ErrorCode]int
}

// newRunState builds a runState with all maps allocated. Resume paths
// then overwrite specific fields (outputs, loop counters, vars, etc.)
// from the persisted checkpoint.
func (e *Engine) newRunState(runID string, inputs map[string]interface{}) *runState {
	return &runState{
		runID:              runID,
		runInputs:          inputs,
		outputs:            make(map[string]map[string]interface{}),
		artifacts:          make(map[string]map[string]interface{}),
		loopCounters:       make(map[string]int),
		roundRobinCounters: make(map[string]int),
		artifactVersions:   make(map[string]int),
		nodeAttempts:       make(map[string]map[ErrorCode]int),
		budget:             newSharedBudget(e.workflow.Budget, e.logger),
	}
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

	rs := e.newRunState(runID, inputs)
	rs.vars = e.resolveVars(inputs)

	loopErr := e.execLoop(ctx, rs, e.workflow.Entry)
	e.evictRunSessions(runID, loopErr)
	return loopErr
}

// evictRunSessions clears any per-node session state still held by
// the executor for runID, except when the run is paused (human input
// awaited) — in which case Resume will pick up the same sessions.
func (e *Engine) evictRunSessions(runID string, loopErr error) {
	if errors.Is(loopErr, ErrRunPaused) {
		return
	}
	if ev, ok := e.executor.(interface{ EvictRun(string) }); ok {
		ev.EvictRun(runID)
	}
}

// execLoop is the shared execution loop used by both Run and Resume.
// It walks the graph from startNodeID until a terminal node, human pause,
// or error.
func (e *Engine) execLoop(ctx context.Context, rs *runState, startNodeID string) error {
	currentNodeID := startNodeID

	for {
		select {
		case <-ctx.Done():
			return e.handleContextDoneWithCheckpoint(rs, currentNodeID, ctx.Err())
		default:
		}

		node, ok := e.workflow.Nodes[currentNodeID]
		if !ok {
			return e.failRunWithCheckpoint(rs, currentNodeID,
				fmt.Sprintf("node %q not found", currentNodeID))
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
					return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
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
					return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			case ir.RouterRoundRobin:
				nextNodeID, err := e.execRoundRobin(ctx, rs, currentNodeID)
				if err != nil {
					return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			case ir.RouterLLM:
				nextNodeID, err := e.execLLMRouter(ctx, rs, currentNodeID)
				if err != nil {
					return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
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
		// Thread the run ID into ctx so the executor can locate
		// per-node session state (used by Compactor implementations
		// to find the right messages list to compact + retry).
		execCtx := model.WithRunID(ctx, rs.runID)
		output, err := e.executor.Execute(execCtx, node, nodeInput)
		if err != nil {
			// Check if the delegate needs user interaction.
			var needsInput *model.ErrNeedsInteraction
			if errors.As(err, &needsInput) {
				return e.handleNeedsInteraction(ctx, rs, currentNodeID, node, needsInput)
			}
			// Recovery dispatch (when wired via WithRecoveryDispatch):
			// classify the error, look up a recipe, and either retry,
			// pause, or fail terminally. Without a dispatcher, every
			// failure produces failed_resumable as before. The
			// run-ID-enriched ctx is passed so Compact() can locate
			// the per-node session.
			retry, recoveryErr := e.handleNodeFailure(execCtx, rs, currentNodeID, err)
			if recoveryErr != nil {
				return recoveryErr
			}
			if retry {
				continue
			}
			return e.failRunWithCheckpoint(rs, currentNodeID, fmt.Sprintf("node %q execution failed: %v", currentNodeID, err))
		}

		// Reset per-node retry counters on success so a future failure
		// starts fresh.
		delete(rs.nodeAttempts, currentNodeID)

		// Store output.
		rs.outputs[currentNodeID] = output

		// Validate output against declared schema (optional).
		if err := e.validateNodeOutput(currentNodeID, node, output); err != nil {
			return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
		}

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

		// Best-effort checkpoint for resume-from-failed.
		if err := e.store.SaveCheckpoint(rs.runID, buildCheckpoint(rs, currentNodeID)); err != nil {
			e.logger.Error("failed to save checkpoint after node %q: %v", currentNodeID, err)
		}

		// --- Select outgoing edge ---
		nextNodeID, err := e.selectEdge(rs.runID, currentNodeID, output, rs.loopCounters)
		if err != nil {
			return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
		}

		currentNodeID = nextNodeID
	}
}

// ---------------------------------------------------------------------------
// Edge selection
// ---------------------------------------------------------------------------

// selectEdge picks the next node by evaluating outgoing edges from the
// current node. Conditional edges are checked first; the first matching
// unconditional edge serves as fallback. Loop counters are enforced.
// When an edge's loop is exhausted, that edge is skipped and the next
// matching edge is tried — this enables fallback patterns like
// fix_loop → outer_loop.
func (e *Engine) selectEdge(runID, fromNodeID string, output map[string]interface{}, loopCounters map[string]int) (string, error) {
	selected := e.evaluateEdgesWithLoops(fromNodeID, "main", output, loopCounters)
	if selected == nil {
		return "", &RuntimeError{
			Code:    ErrCodeNoOutgoingEdge,
			Message: fmt.Sprintf("no outgoing edge from node %q", fromNodeID),
			NodeID:  fromNodeID,
			Hint:    "ensure the node's output matches at least one edge condition, or add an unconditional fallback edge",
		}
	}

	// Increment loop counter for the selected edge.
	if selected.LoopName != "" {
		loopCounters[selected.LoopName] = loopCounters[selected.LoopName] + 1
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
		e.logger.Warn("failed to emit edge_selected: %v", err)
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

	// Fallback: for the entry node merge workflow var defaults with run-level
	// inputs so that {{input.X}} references resolve to the var default when
	// --var X=... was not provided on the CLI. Without this, vars declared
	// with a default like `scope_notes: string = ""` are missing from the
	// entry node's input map and the placeholder is left literal in prompts.
	// CLI inputs override defaults.
	if nodeID == e.workflow.Entry {
		for name, v := range e.workflow.Vars {
			if v.HasDefault {
				result[name] = v.Default
			}
		}
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
