package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/model"
	"github.com/SocialGouv/iterion/store"
)

// ---------------------------------------------------------------------------
// Resume — continue a paused run
// ---------------------------------------------------------------------------

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

	// Validate output against declared schema (optional).
	if err := e.validateNodeOutput(nodeID, node, output); err != nil {
		return false, e.failRunErr(rs.runID, nodeID, err)
	}

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
// Loop helpers
// ---------------------------------------------------------------------------

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
