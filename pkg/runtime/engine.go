// Package runtime implements the workflow execution engine.
// It walks the compiled IR graph node by node, persists outputs and
// artifacts via the store, evaluates edge conditions and loop counters,
// and emits lifecycle events. It supports both sequential execution and
// parallel fan-out/join patterns via a bounded branch scheduler.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
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
	filePath         string                // absolute .iter source path, set via WithFilePath
	validateOutputs  bool                  // when true, validate node outputs against declared schemas
	forceResume      bool                  // when true, skip workflow hash check on resume
	workDir          string                // working directory for subprocesses + PROJECT_DIR expansion; defaults to os.Getwd() at Run() time
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

// WithFilePath records the absolute .iter source path on the run
// metadata so that resume (and the run console) can re-locate the
// workflow without the caller having to thread it back through the
// API. Optional — empty string is ignored.
func WithFilePath(path string) EngineOption {
	return func(e *Engine) { e.filePath = path }
}

// WithForceResume allows resuming a run even when the workflow source has
// changed since the run was started. The hash mismatch is logged as a warning
// instead of causing an error.
func WithForceResume(force bool) EngineOption {
	return func(e *Engine) { e.forceResume = force }
}

// WithWorkDir sets the working directory used for backend subprocesses and
// for resolving the `${PROJECT_DIR}` placeholder in workflow var defaults.
// When unset, defaults to os.Getwd() at Run() time. With worktree: auto on
// the workflow, the engine overrides this with the per-run worktree path.
func WithWorkDir(dir string) EngineOption {
	return func(e *Engine) { e.workDir = dir }
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
	runID        string
	runInputs    map[string]interface{}
	vars         map[string]interface{}
	outputs      map[string]map[string]interface{}
	artifacts    map[string]map[string]interface{} // publish name → output
	loopCounters map[string]int
	// loopPreviousOutput holds the snapshot of the source node output from
	// the PREVIOUS traversal of a given loop's edge — i.e., one iteration
	// behind the current one. Workflows reference it as
	// {{loop.<name>.previous_output[.field]}}; in the very first iteration
	// of a loop the value is nil. The snapshot is rotated through
	// loopCurrentOutput on each traversal to preserve the one-iteration lag.
	loopPreviousOutput map[string]map[string]interface{}
	loopCurrentOutput  map[string]map[string]interface{} // staging slot for the next iteration's "previous"
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
		loopPreviousOutput: make(map[string]map[string]interface{}),
		loopCurrentOutput:  make(map[string]map[string]interface{}),
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
	if e.workflowHash != "" || e.filePath != "" {
		if e.workflowHash != "" {
			run.WorkflowHash = e.workflowHash
		}
		if e.filePath != "" {
			run.FilePath = e.filePath
		}
		if err := e.store.SaveRun(run); err != nil {
			return fmt.Errorf("runtime: save workflow hash: %w", err)
		}
	}

	// Default workDir to process cwd if not set explicitly.
	if e.workDir == "" {
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			e.workDir = cwd
		}
	}

	// Worktree setup: when the workflow opts in with `worktree: auto`,
	// create a fresh git worktree for this run so all node executions
	// happen in an isolated checkout. The user's main working tree
	// (with WIP, build artefacts, etc.) is invisible by construction.
	var worktreeCleanup func()
	if e.workflow.Worktree == "auto" {
		wtPath, cleanup, wtErr := setupWorktree(e.store.Root(), runID, e.workDir, e.logger)
		if wtErr != nil {
			_ = e.store.UpdateRunStatus(runID, store.RunStatusFailed, wtErr.Error())
			return fmt.Errorf("runtime: worktree setup: %w", wtErr)
		}
		e.workDir = wtPath
		worktreeCleanup = cleanup
	}

	// Push workDir into the executor so backend subprocesses (claude_code,
	// codex) and tool nodes see it. Type-assert because NodeExecutor is a
	// minimal interface; only ClawExecutor implements SetWorkDir.
	type workDirSetter interface{ SetWorkDir(string) }
	if s, ok := e.executor.(workDirSetter); ok {
		s.SetWorkDir(e.workDir)
	}

	// Emit run_started.
	if err := e.emit(runID, store.EventRunStarted, "", nil); err != nil {
		return err
	}

	rs := e.newRunState(runID, inputs)
	rs.vars = e.resolveVars(inputs)

	// Refresh executor vars: PROJECT_DIR-aware expansion may have changed
	// values from what the CLI/server originally seeded.
	type varsSetter interface{ SetVars(map[string]interface{}) }
	if sv, ok := e.executor.(varsSetter); ok {
		sv.SetVars(rs.vars)
	}

	loopErr := e.execLoop(ctx, rs, e.workflow.Entry)
	e.evictRunSessions(runID, loopErr)

	// Worktree retention: remove on clean exit; preserve on error so the
	// operator can inspect what the run actually produced.
	if worktreeCleanup != nil {
		if loopErr == nil {
			worktreeCleanup()
		} else if e.logger != nil {
			e.logger.Info("runtime: worktree preserved for inspection: %s", e.workDir)
		}
	}

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
				nextNodeID, err := e.selectEdgeRS(rs, currentNodeID, rs.outputs[currentNodeID])
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

		// --- Compute nodes execute deterministically inside the engine ---
		// (no LLM, no shell-out). Keep this path before emitting node_started
		// so it shares the same envelope as other nodes.
		if cn, ok := node.(*ir.ComputeNode); ok {
			nextNodeID, err := e.execCompute(rs, currentNodeID, cn)
			if err != nil {
				return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
			}
			currentNodeID = nextNodeID
			continue
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
		nodeInput := e.buildNodeInputRS(currentNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)

		// --- Execute node ---
		// Thread the run ID into ctx so the executor can locate
		// per-node session state (used by Compactor implementations
		// to find the right messages list to compact + retry).
		execCtx := model.WithRunID(ctx, rs.runID)
		// Attach a template-data snapshot so the executor can resolve
		// `outputs.*`, `loop.*`, `artifacts.*`, and `run.*` refs in
		// prompt bodies. These namespaces complement the `input.*`
		// fields populated by edge `with`-mappings and the workflow-
		// level `vars.*`.
		execCtx = model.WithTemplateData(execCtx, e.buildTemplateData(rs))
		output, err := e.executor.Execute(execCtx, node, nodeInput)
		if err != nil {
			// Check if the delegate needs user interaction.
			var needsInput *model.ErrNeedsInteraction
			if errors.As(err, &needsInput) {
				return e.handleNeedsInteraction(ctx, rs, currentNodeID, node, needsInput, 0)
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
		nextNodeID, err := e.selectEdgeRS(rs, currentNodeID, output)
		if err != nil {
			return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
		}

		currentNodeID = nextNodeID
	}
}

// ---------------------------------------------------------------------------
// Compute nodes
// ---------------------------------------------------------------------------

// execCompute evaluates a ComputeNode's expressions deterministically and
// stores the result as the node's output. It mirrors the standard execution
// envelope (node_started → output → node_finished → checkpoint → edge select)
// without invoking the executor backend.
func (e *Engine) execCompute(rs *runState, nodeID string, cn *ir.ComputeNode) (string, error) {
	if err := e.emit(rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind": "compute",
	}); err != nil {
		return "", err
	}

	nodeInput := e.buildNodeInputRS(nodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)
	output := make(map[string]interface{}, len(cn.Exprs))

	exprCtx := e.exprContext(rs, nodeInput)
	for _, ce := range cn.Exprs {
		v, err := ce.AST.Eval(exprCtx)
		if err != nil {
			return "", &RuntimeError{
				Code:    ErrCodeExecutionFailed,
				Message: fmt.Sprintf("compute %q: field %q expression %q: %v", nodeID, ce.Key, ce.Raw, err),
				NodeID:  nodeID,
				Hint:    "check the compute node's expressions for type mismatches or unknown references",
			}
		}
		output[ce.Key] = v
	}

	rs.outputs[nodeID] = output
	delete(rs.nodeAttempts, nodeID)

	if err := e.validateNodeOutput(nodeID, cn, output); err != nil {
		return "", err
	}

	if err := e.emit(rs.runID, store.EventNodeFinished, nodeID, buildNodeFinishedData(output)); err != nil {
		return "", err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(nodeID, output)
	}

	if err := e.store.SaveCheckpoint(rs.runID, buildCheckpoint(rs, nodeID)); err != nil {
		e.logger.Error("failed to save checkpoint after compute %q: %v", nodeID, err)
	}

	return e.selectEdgeRS(rs, nodeID, output)
}

// ---------------------------------------------------------------------------
// Edge selection
// ---------------------------------------------------------------------------

// selectEdgeRS picks the next node by evaluating outgoing edges from the
// current node, threading the runState so expression-form `when` clauses
// can resolve `{{loop.*}}` / `{{run.*}}` namespaces and so loop edges
// snapshot the source node's output as `loop.<name>.previous_output` for
// the next iteration. Conditional edges are checked first; the first
// matching unconditional edge serves as fallback. When a loop's counter is
// exhausted that edge is skipped — enabling graceful exit patterns like
// `fix_loop -> outer_loop` or `loop_edge -> done`.
func (e *Engine) selectEdgeRS(rs *runState, fromNodeID string, output map[string]interface{}) (string, error) {
	selected := e.evaluateEdgesWithLoopsRS(fromNodeID, "main", output, rs)
	if selected == nil {
		return "", &RuntimeError{
			Code:    ErrCodeNoOutgoingEdge,
			Message: fmt.Sprintf("no outgoing edge from node %q", fromNodeID),
			NodeID:  fromNodeID,
			Hint:    "ensure the node's output matches at least one edge condition, or add an unconditional fallback edge",
		}
	}

	if selected.LoopName != "" {
		rs.loopCounters[selected.LoopName] = rs.loopCounters[selected.LoopName] + 1
		// Rotate snapshots so {{loop.<name>.previous_output}} reads the
		// snapshot from the PRIOR traversal (one iteration behind), not the
		// current one. The current iteration's source output is staged in
		// loopCurrentOutput and only promoted to loopPreviousOutput at the
		// NEXT loop-edge crossing for the same loop name.
		if rs.loopPreviousOutput != nil {
			if staged, ok := rs.loopCurrentOutput[selected.LoopName]; ok {
				rs.loopPreviousOutput[selected.LoopName] = staged
			}
			snap := make(map[string]interface{}, len(output))
			for k, v := range output {
				snap[k] = v
			}
			rs.loopCurrentOutput[selected.LoopName] = snap
		}
	}

	data := map[string]interface{}{
		"from": selected.From,
		"to":   selected.To,
	}
	if selected.Condition != "" {
		data["condition"] = selected.Condition
		data["negated"] = selected.Negated
	}
	if selected.ExpressionSrc != "" {
		data["expression"] = selected.ExpressionSrc
	}
	if selected.LoopName != "" {
		data["loop"] = selected.LoopName
		data["iteration"] = rs.loopCounters[selected.LoopName]
	}
	if err := e.emit(rs.runID, store.EventEdgeSelected, "", data); err != nil {
		e.logger.Warn("failed to emit edge_selected: %v", err)
	}

	return selected.To, nil
}

// ---------------------------------------------------------------------------
// Input resolution
// ---------------------------------------------------------------------------

// buildNodeInputRS constructs the input map for a node by looking at the
// edge `with` mappings that target this node. For convergence points,
// mappings from ALL resolved incoming edges are merged. If no mappings
// exist, the run-level inputs are used for the entry node. The runState
// is required so that `{{loop.*}}` / `{{run.*}}` references resolve
// against the run's iteration state. Pass nil for rs only in tests that
// don't exercise those namespaces.
func (e *Engine) buildNodeInputRS(nodeID string, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}, rs *runState) map[string]interface{} {
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
			val := e.resolveMapping(dm, vars, outputs, effectiveInputs, artifacts, rs)
			// Include nil values too: a ref that resolves to nil
			// (e.g. `{{outputs.fixer.pushback}}` before the fixer
			// has run, `{{loop.X.previous_output}}` on iteration 1)
			// is still a *valid* mapping — the field exists, its
			// value is just empty. Dropping it would leave
			// `{{input.<key>}}` placeholders unresolved in
			// downstream prompts, surfacing template syntax to the
			// LLM instead of an empty string.
			result[dm.Key] = val
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
func (e *Engine) resolveMapping(dm *ir.DataMapping, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}, rs *runState) interface{} {
	if len(dm.Refs) == 1 {
		return e.resolveRef(dm.Refs[0], vars, outputs, runInputs, artifacts, rs)
	}
	return dm.Raw
}

// resolveRef resolves a single Ref to a concrete value. The runState is
// required for `loop` and `run` namespace resolution; pass nil to skip
// those (they'll resolve to nil).
func (e *Engine) resolveRef(ref *ir.Ref, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}, rs *runState) interface{} {
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
	case ir.RefLoop:
		if rs == nil || len(ref.Path) < 2 {
			return nil
		}
		return resolveLoopPath(ref.Path, rs, e.workflow.Loops)
	case ir.RefRun:
		if rs == nil || len(ref.Path) == 0 {
			return nil
		}
		switch ref.Path[0] {
		case "id":
			return rs.runID
		}
	}
	return nil
}

// resolveLoopPath resolves a {{loop.<name>.<field>[.subfield…]}} reference.
// Recognized fields:
//
//	iteration       — current loop counter (int64)
//	max             — declared maximum iterations (int64)
//	previous_output — snapshot of the source node output at the previous
//	                   traversal of this loop's edge; sub-fields drill in.
func resolveLoopPath(path []string, rs *runState, loops map[string]*ir.Loop) interface{} {
	loopName := path[0]
	switch path[1] {
	case "iteration":
		return int64(rs.loopCounters[loopName])
	case "max":
		if l, ok := loops[loopName]; ok {
			return int64(l.MaxIterations)
		}
		return nil
	case "previous_output":
		return drillPath(rs.loopPreviousOutput[loopName], path[2:])
	}
	return nil
}

// buildTemplateData assembles a model.TemplateData snapshot from the
// current run state. It is attached to ctx before each node execution
// so the executor can resolve `outputs.*`, `loop.*`, `artifacts.*`,
// and `run.*` refs in prompt bodies. Maps are passed by reference —
// the executor must treat them as read-only.
func (e *Engine) buildTemplateData(rs *runState) *model.TemplateData {
	loopMax := make(map[string]int, len(e.workflow.Loops))
	for name, l := range e.workflow.Loops {
		if l != nil {
			loopMax[name] = l.MaxIterations
		}
	}
	return &model.TemplateData{
		Outputs:            rs.outputs,
		LoopCounters:       rs.loopCounters,
		LoopMaxIterations:  loopMax,
		LoopPreviousOutput: rs.loopPreviousOutput,
		Artifacts:          rs.artifacts,
		RunID:              rs.runID,
	}
}

// drillPath walks a nested map[string]interface{} structure by the given
// path, returning the final value (or nil if any segment is missing or
// non-map). Used by every reference resolver that needs to descend into
// node outputs / artifacts / loop snapshots.
func drillPath(root interface{}, path []string) interface{} {
	cur := root
	for _, key := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

// resolveVars builds the vars map from workflow variable defaults,
// coercing user-provided override strings to the declared type.
//
// Coercion is necessary because the CLI's --var flag and the HTTP
// /api/runs endpoint both deliver vars as raw strings. Without
// coercion, an explicit "--var loop_count=3" stores the var as the
// string "3", which then fails downstream comparisons against the
// typed defaults (e.g. "input.count >= vars.loop_count" tries to
// compare a number against a string and aborts the run with an
// opaque "cannot compare X >= string" error). Defaults from the
// .iter source are already typed by the IR compiler — we coerce
// only on overrides.
func (e *Engine) resolveVars(inputs map[string]interface{}) map[string]interface{} {
	vars := make(map[string]interface{})
	// expandFn lets var defaults reference ${PROJECT_DIR} (resolved to the
	// engine's workDir, possibly the worktree path) and any other env var.
	// Applied to string defaults only — typed defaults (int/float/bool/json)
	// pass through unchanged. User-provided overrides further down are taken
	// at face value.
	expandFn := func(key string) string {
		if key == "PROJECT_DIR" {
			return e.workDir
		}
		return os.Getenv(key)
	}
	for name, v := range e.workflow.Vars {
		if v.HasDefault {
			if s, ok := v.Default.(string); ok {
				vars[name] = os.Expand(s, expandFn)
			} else {
				vars[name] = v.Default
			}
		}
	}
	for k, v := range inputs {
		decl, isVar := e.workflow.Vars[k]
		if !isVar {
			continue
		}
		coerced, err := coerceVarValue(v, decl.Type)
		if err != nil {
			// Fall back to whatever the caller passed; the engine's
			// downstream type checks will surface a clear error if
			// the value really is incompatible. The alternative —
			// failing the run here — would be more aggressive than
			// the previous behaviour.
			e.logger.Warn("runtime: var %q: coerce to %s failed: %v (using raw value)", k, decl.Type, err)
			vars[k] = v
			continue
		}
		vars[k] = coerced
	}
	return vars
}

// coerceVarValue narrows a user-provided override (typically a
// string from --var or POST /api/runs) to the type declared in the
// IR for that var. Already-typed values pass through.
func coerceVarValue(v interface{}, vt ir.VarType) (interface{}, error) {
	s, isStr := v.(string)
	if !isStr {
		return v, nil
	}
	switch vt {
	case ir.VarString:
		return s, nil
	case ir.VarBool:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no", "":
			return false, nil
		default:
			return nil, fmt.Errorf("invalid bool %q", s)
		}
	case ir.VarInt:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid int %q: %w", s, err)
		}
		return n, nil
	case ir.VarFloat:
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %q: %w", s, err)
		}
		return n, nil
	case ir.VarJSON:
		// Parse JSON; if the user gave us non-JSON text, leave it
		// as a string — JSON expressions accept either.
		var out interface{}
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return s, nil
		}
		return out, nil
	case ir.VarStringArray:
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return []interface{}{}, nil
		}
		// Accept either JSON array form (["a","b"]) or
		// comma-separated (a,b).
		if strings.HasPrefix(trimmed, "[") {
			var arr []interface{}
			if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
				return arr, nil
			}
		}
		parts := strings.Split(trimmed, ",")
		out := make([]interface{}, len(parts))
		for i, p := range parts {
			out[i] = strings.TrimSpace(p)
		}
		return out, nil
	default:
		return s, nil
	}
}
