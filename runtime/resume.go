package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/model"
	"github.com/SocialGouv/iterion/store"
)

// ---------------------------------------------------------------------------
// Resume — continue a paused run
// ---------------------------------------------------------------------------

// Resume resumes a paused or failed-resumable run. For paused runs, human
// answers are recorded and execution continues from the human node. For
// failed-resumable runs, execution restarts from the node after the last
// successfully completed one (re-executing the failed node).
func (e *Engine) Resume(ctx context.Context, runID string, answers map[string]interface{}) error {
	r, err := e.store.LoadRun(runID)
	if err != nil {
		return fmt.Errorf("runtime: load run for resume: %w", err)
	}
	switch r.Status {
	case store.RunStatusPausedWaitingHuman:
		return e.resumeFromPause(ctx, r, answers)
	case store.RunStatusFailedResumable, store.RunStatusCancelled:
		return e.resumeFromFailure(ctx, r)
	default:
		return fmt.Errorf("runtime: cannot resume run %q with status %q", runID, r.Status)
	}
}

// checkWorkflowHash validates that the workflow source has not changed since
// the run was started. When forceResume is set, a mismatch is logged as a
// warning instead of causing an error.
func (e *Engine) checkWorkflowHash(r *store.Run) error {
	if r.WorkflowHash == "" || e.workflowHash == "" {
		return nil
	}
	if r.WorkflowHash == e.workflowHash {
		return nil
	}
	shortHash := func(h string) string {
		if len(h) > 12 {
			return h[:12]
		}
		return h
	}
	if e.forceResume {
		if e.logger != nil {
			e.logger.Warn("workflow source has changed since run %q was started (expected %s, got %s); resuming anyway (--force)", r.ID, shortHash(r.WorkflowHash), shortHash(e.workflowHash))
		}
		return nil
	}
	return fmt.Errorf("runtime: workflow source has changed since run %q was started (expected hash %s, got %s); re-run from scratch or use --force", r.ID, shortHash(r.WorkflowHash), shortHash(e.workflowHash))
}

// rebuildArtifacts reconstructs the artifacts map from checkpoint outputs.
func (e *Engine) rebuildArtifacts(outputs map[string]map[string]interface{}) map[string]map[string]interface{} {
	artifacts := make(map[string]map[string]interface{})
	for nodeID, output := range outputs {
		if n, ok := e.workflow.Nodes[nodeID]; ok {
			if pub := nodePublish(n); pub != "" {
				artifacts[pub] = output
			}
		}
	}
	return artifacts
}

// resumeFromPause resumes a paused run by recording human answers and
// continuing execution from the node after the human checkpoint.
func (e *Engine) resumeFromPause(ctx context.Context, r *store.Run, answers map[string]interface{}) error {
	runID := r.ID
	if err := e.checkWorkflowHash(r); err != nil {
		return err
	}
	if r.Checkpoint == nil {
		return fmt.Errorf("runtime: run %q has no checkpoint", runID)
	}

	cp := r.Checkpoint
	humanNodeID := cp.NodeID

	// Record answers on the interaction. Fall back to the checkpoint's
	// embedded questions if the interaction file has been deleted.
	interaction, err := e.store.LoadInteraction(runID, cp.InteractionID)
	if err != nil && cp.InteractionQuestions != nil {
		interaction = &store.Interaction{
			ID:          cp.InteractionID,
			RunID:       runID,
			NodeID:      cp.NodeID,
			RequestedAt: r.UpdatedAt,
			Questions:   cp.InteractionQuestions,
		}
	} else if err != nil {
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

	// Build runState before edge selection so failures are resumable.
	loopCounters := cp.LoopCounters
	roundRobinCounters := cp.RoundRobinCounters
	if roundRobinCounters == nil {
		roundRobinCounters = make(map[string]int)
	}

	rs := e.newRunState(runID, r.Inputs)
	rs.vars = cp.Vars
	rs.outputs = outputs
	rs.artifacts = e.rebuildArtifacts(outputs)
	rs.loopCounters = loopCounters
	rs.roundRobinCounters = roundRobinCounters
	rs.artifactVersions = artifactVersions
	rs.nodeAttempts = restoreNodeAttempts(cp.NodeAttempts)

	// Select edge from the human node to find the next node.
	nextNodeID, err := e.selectEdge(runID, humanNodeID, answers, loopCounters)
	if err != nil {
		return e.failRunErrWithCheckpoint(rs, humanNodeID, err)
	}

	loopErr := e.execLoop(ctx, rs, nextNodeID)
	e.evictRunSessions(runID, loopErr)
	return loopErr
}

// resumeFromFailure resumes a failed_resumable run by re-executing from the
// node that follows the last successfully completed node (the one that failed).
func (e *Engine) resumeFromFailure(ctx context.Context, r *store.Run) error {
	runID := r.ID
	if err := e.checkWorkflowHash(r); err != nil {
		return err
	}
	if r.Checkpoint == nil {
		return fmt.Errorf("runtime: run %q has no checkpoint", runID)
	}

	cp := r.Checkpoint
	restartNodeID := cp.NodeID

	// Update status to running and emit run_resumed.
	if err := e.store.UpdateRunStatus(runID, store.RunStatusRunning, ""); err != nil {
		return fmt.Errorf("runtime: update status running: %w", err)
	}
	if err := e.emit(runID, store.EventRunResumed, "", map[string]interface{}{
		"resumed_from": "failed",
		"restart_node": restartNodeID,
	}); err != nil {
		return err
	}

	loopCounters := cp.LoopCounters
	roundRobinCounters := cp.RoundRobinCounters
	if roundRobinCounters == nil {
		roundRobinCounters = make(map[string]int)
	}
	artifactVersions := cp.ArtifactVersions
	if artifactVersions == nil {
		artifactVersions = make(map[string]int)
	}

	rs := e.newRunState(runID, r.Inputs)
	rs.vars = cp.Vars
	rs.outputs = cp.Outputs
	rs.artifacts = e.rebuildArtifacts(cp.Outputs)
	rs.loopCounters = loopCounters
	rs.roundRobinCounters = roundRobinCounters
	rs.artifactVersions = artifactVersions
	rs.nodeAttempts = restoreNodeAttempts(cp.NodeAttempts)

	loopErr := e.execLoop(ctx, rs, restartNodeID)
	e.evictRunSessions(runID, loopErr)
	return loopErr
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
		return false, e.failRunWithCheckpoint(rs, nodeID, fmt.Sprintf("human node %q auto_or_pause execution failed: %v", nodeID, err))
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
		return false, e.failRunErrWithCheckpoint(rs, nodeID, err)
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
	questions := e.buildNodeInput(nodeID, rs.vars, rs.outputs, nil, rs.artifacts)
	return e.doPause(rs, nodeID, questions, nil, "", "")
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
		return e.handleInteractionLLM(ctx, rs, nodeID, node, ni)

	case ir.InteractionLLMOrHuman:
		return e.handleInteractionLLMOrHuman(ctx, rs, nodeID, node, ni)

	default:
		// InteractionNone should not reach here (executor wouldn't return ErrNeedsInteraction).
		return fmt.Errorf("runtime: node %q received interaction request but has interaction: none", nodeID)
	}
}

// handleInteractionLLM invokes the interaction model to auto-respond to the
// delegate's questions, then re-invokes the backend with the answers.
func (e *Engine) handleInteractionLLM(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction) error {
	clawExec, ok := e.executor.(*model.ClawExecutor)
	if !ok {
		// Fallback to pause if executor doesn't support interaction LLM.
		return e.pauseForBackendInteraction(rs, nodeID, ni)
	}

	fields := interactionFields(node)
	answers, _, err := clawExec.ExecuteHumanLLMForInteraction(ctx, nodeID, ni, fields)
	if err != nil {
		return e.failRunWithCheckpoint(rs, nodeID,
			fmt.Sprintf("interaction LLM for node %q failed: %v", nodeID, err))
	}

	// Re-invoke the backend with the LLM-generated answers.
	return e.reInvokeBackend(ctx, rs, nodeID, node, ni, answers)
}

// handleInteractionLLMOrHuman invokes the interaction model to decide whether
// to auto-respond or escalate to a human. If the LLM sets needs_human_input=true,
// the run is paused for human input.
func (e *Engine) handleInteractionLLMOrHuman(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction) error {
	clawExec, ok := e.executor.(*model.ClawExecutor)
	if !ok {
		return e.pauseForBackendInteraction(rs, nodeID, ni)
	}

	fields := interactionFields(node)
	answers, needsHuman, err := clawExec.ExecuteHumanLLMForInteraction(ctx, nodeID, ni, fields)
	if err != nil {
		return e.failRunWithCheckpoint(rs, nodeID,
			fmt.Sprintf("interaction LLM for node %q failed: %v", nodeID, err))
	}

	if needsHuman {
		// LLM decided it needs human input — pause.
		return e.pauseForBackendInteraction(rs, nodeID, ni)
	}

	// LLM auto-responded — re-invoke the backend.
	return e.reInvokeBackend(ctx, rs, nodeID, node, ni, answers)
}

// reInvokeBackend re-invokes the delegate backend with the LLM-provided
// answers merged into the node input. It uses the delegate's session ID
// for session continuity so the backend can resume where it left off.
func (e *Engine) reInvokeBackend(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction, answers map[string]interface{}) error {
	// Build the input for re-invocation: original node input + answers.
	nodeInput := e.buildNodeInput(nodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts)
	for k, v := range answers {
		nodeInput[k] = v
	}

	// Re-execute the node. The executor will use the session ID for
	// delegate re-invocation if the backend supports it.
	output, err := e.executor.Execute(ctx, node, nodeInput)
	if err != nil {
		// Check for another interaction request (recursive).
		var needsInput *model.ErrNeedsInteraction
		if errors.As(err, &needsInput) {
			return e.handleNeedsInteraction(ctx, rs, nodeID, node, needsInput)
		}
		return e.failRunWithCheckpoint(rs, nodeID,
			fmt.Sprintf("node %q re-invocation failed: %v", nodeID, err))
	}

	// Store the output and continue execution normally.
	rs.outputs[nodeID] = output

	// Validate output.
	if err := e.validateNodeOutput(nodeID, node, output); err != nil {
		return e.failRunErrWithCheckpoint(rs, nodeID, err)
	}

	// Record budget.
	if err := e.recordAndCheckBudget(rs, nodeID, output); err != nil {
		return err
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
			return fmt.Errorf("runtime: write artifact: %w", err)
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
		return err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(nodeID, output)
	}

	// Checkpoint.
	if err := e.store.SaveCheckpoint(rs.runID, buildCheckpoint(rs, nodeID)); err != nil {
		e.logger.Error("failed to save checkpoint after re-invocation of node %q: %v", nodeID, err)
	}

	// Select next edge.
	nextNodeID, err := e.selectEdge(rs.runID, nodeID, output, rs.loopCounters)
	if err != nil {
		return e.failRunErrWithCheckpoint(rs, nodeID, err)
	}

	loopErr := e.execLoop(ctx, rs, nextNodeID)
	e.evictRunSessions(rs.runID, loopErr)
	return loopErr
}

// interactionFields extracts InteractionFields from a node that supports them.
func interactionFields(node ir.Node) ir.InteractionFields {
	switch n := node.(type) {
	case *ir.AgentNode:
		return n.InteractionFields
	case *ir.JudgeNode:
		return n.InteractionFields
	case *ir.HumanNode:
		return n.InteractionFields
	default:
		return ir.InteractionFields{}
	}
}

// pauseForBackendInteraction creates an interaction record and pauses the
// workflow, saving the backend's session ID for re-invocation on resume.
func (e *Engine) pauseForBackendInteraction(rs *runState, nodeID string, ni *model.ErrNeedsInteraction) error {
	eventExtra := map[string]interface{}{
		"source":  "delegate",
		"backend": ni.Backend,
	}
	if err := e.doPause(rs, nodeID, ni.Questions, eventExtra, ni.SessionID, ni.Backend); err != nil {
		return err
	}
	return ErrRunPaused
}

// doPause is the unified implementation for pausing a run. It writes the
// interaction record, emits pause events, and saves the checkpoint.
func (e *Engine) doPause(rs *runState, nodeID string, questions map[string]interface{}, eventExtra map[string]interface{}, backendSessionID, backendName string) error {
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
	eventData := map[string]interface{}{
		"interaction_id": interactionID,
		"questions":      questions,
	}
	for k, v := range eventExtra {
		eventData[k] = v
	}
	if err := e.emit(rs.runID, store.EventHumanInputRequested, nodeID, eventData); err != nil {
		return err
	}

	// Emit run_paused.
	if err := e.emit(rs.runID, store.EventRunPaused, nodeID, nil); err != nil {
		return err
	}

	// Atomically save checkpoint and set status to paused in a single write.
	cp := buildCheckpoint(rs, nodeID)
	cp.InteractionID = interactionID
	cp.InteractionQuestions = questions
	cp.BackendSessionID = backendSessionID
	cp.BackendName = backendName
	if err := e.store.PauseRun(rs.runID, cp); err != nil {
		return fmt.Errorf("runtime: pause run: %w", err)
	}

	return nil
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
