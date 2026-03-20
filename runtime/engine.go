// Package runtime implements the sequential workflow execution engine.
// It walks the compiled IR graph node by node, persists outputs and
// artifacts via the store, evaluates edge conditions and loop counters,
// and emits lifecycle events.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/store"
)

// ErrRunPaused is returned by Run or Resume when execution is suspended
// at a human node. This is not a failure — the run can be resumed via
// Engine.Resume.
var ErrRunPaused = errors.New("runtime: run paused waiting for human input")

// NodeExecutor is the abstraction called by the engine to actually run a
// node (LLM call, tool invocation, etc.). The runtime itself is agnostic
// to the concrete implementation — tests supply stubs, production code
// plugs in real providers.
type NodeExecutor interface {
	// Execute runs the given node with the provided input and returns its
	// output. For terminal nodes (done/fail) this is never called.
	Execute(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error)
}

// Engine is the sequential runtime. It executes one node at a time,
// following edges until a terminal node is reached.
type Engine struct {
	workflow *ir.Workflow
	store    *store.RunStore
	executor NodeExecutor
}

// New creates a new sequential Engine.
func New(wf *ir.Workflow, s *store.RunStore, exec NodeExecutor) *Engine {
	return &Engine{workflow: wf, store: s, executor: exec}
}

// runState holds the mutable runtime state passed through the execution loop.
type runState struct {
	runID            string
	runInputs        map[string]interface{}
	vars             map[string]interface{}
	outputs          map[string]map[string]interface{}
	loopCounters     map[string]int
	artifactVersions map[string]int
}

// Run executes the workflow sequentially. It creates a run, walks the
// graph from the entry node, and returns when a terminal node is reached,
// a human pause is hit (ErrRunPaused), or an error occurs.
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
		return e.failRun(runID, humanNodeID, err.Error())
	}

	rs := &runState{
		runID:            runID,
		runInputs:        r.Inputs,
		vars:             cp.Vars,
		outputs:          outputs,
		loopCounters:     loopCounters,
		artifactVersions: artifactVersions,
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
			_ = e.store.UpdateRunStatus(rs.runID, store.RunStatusFailed, ctx.Err().Error())
			_ = e.emit(rs.runID, store.EventRunFailed, currentNodeID, map[string]interface{}{
				"error": ctx.Err().Error(),
			})
			return ctx.Err()
		default:
		}

		node, ok := e.workflow.Nodes[currentNodeID]
		if !ok {
			return e.failRun(rs.runID, currentNodeID, fmt.Sprintf("node %q not found", currentNodeID))
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

		// --- Human pause node ---
		if node.Kind == ir.NodeHuman {
			return e.pauseAtHuman(rs, currentNodeID, node)
		}

		// --- Emit node_started ---
		if err := e.emit(rs.runID, store.EventNodeStarted, currentNodeID, map[string]interface{}{
			"kind": node.Kind.String(),
		}); err != nil {
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

			_ = e.emit(rs.runID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
				"publish": node.Publish,
				"version": version,
			})
		}

		// --- Emit node_finished ---
		if err := e.emit(rs.runID, store.EventNodeFinished, currentNodeID, nil); err != nil {
			return err
		}

		// --- Select outgoing edge ---
		nextNodeID, err := e.selectEdge(rs.runID, currentNodeID, output, rs.loopCounters)
		if err != nil {
			return e.failRun(rs.runID, currentNodeID, err.Error())
		}

		currentNodeID = nextNodeID
	}
}

// pauseAtHuman suspends the run at a human node: persists an interaction,
// saves checkpoint state, and returns ErrRunPaused.
func (e *Engine) pauseAtHuman(rs *runState, nodeID string, node *ir.Node) error {
	// Emit node_started for the human node.
	if err := e.emit(rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind": node.Kind.String(),
	}); err != nil {
		return err
	}

	// Build questions from the node's input (edge mappings into this node).
	questions := e.buildNodeInput(nodeID, rs.vars, rs.outputs, nil)

	// Create interaction.
	interactionID := fmt.Sprintf("%s_%s", rs.runID, nodeID)
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

	// Save checkpoint.
	cp := &store.Checkpoint{
		NodeID:           nodeID,
		InteractionID:    interactionID,
		Outputs:          rs.outputs,
		LoopCounters:     rs.loopCounters,
		ArtifactVersions: rs.artifactVersions,
		Vars:             rs.vars,
	}
	if err := e.store.SaveCheckpoint(rs.runID, cp); err != nil {
		return fmt.Errorf("runtime: save checkpoint: %w", err)
	}

	// Update status to paused.
	if err := e.store.UpdateRunStatus(rs.runID, store.RunStatusPausedWaitingHuman, ""); err != nil {
		return fmt.Errorf("runtime: update status paused: %w", err)
	}

	return ErrRunPaused
}

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
		return "", fmt.Errorf("no outgoing edge from node %q", fromNodeID)
	}

	// Enforce loop counter.
	if selected.LoopName != "" {
		loop, ok := e.workflow.Loops[selected.LoopName]
		if !ok {
			return "", fmt.Errorf("edge references unknown loop %q", selected.LoopName)
		}
		count := loopCounters[selected.LoopName]
		if count >= loop.MaxIterations {
			return "", fmt.Errorf("loop %q exceeded max iterations (%d)", selected.LoopName, loop.MaxIterations)
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
	_ = e.emit(runID, store.EventEdgeSelected, "", data)

	return selected.To, nil
}

// buildNodeInput constructs the input map for a node by looking at the
// edge `with` mappings that target this node. If no mappings exist, the
// run-level inputs are used as a starting point for the entry node.
func (e *Engine) buildNodeInput(nodeID string, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	// Find the edge that targets this node and has `with` mappings.
	// In a sequential run there is at most one incoming edge per step.
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

// failRun marks a run as failed and emits the run_failed event.
func (e *Engine) failRun(runID, nodeID, reason string) error {
	_ = e.store.UpdateRunStatus(runID, store.RunStatusFailed, reason)
	_ = e.emit(runID, store.EventRunFailed, nodeID, map[string]interface{}{
		"error": reason,
	})
	return fmt.Errorf("runtime: %s", reason)
}
