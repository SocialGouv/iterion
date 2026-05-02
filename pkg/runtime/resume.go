package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Reserved input keys live in the delegate package (delegate.PriorAskUser*Key
// and delegate.Resume*Key) so runtime and executor can share them without
// either side needing the other's package.

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

	// Restore the per-run env (workDir + executor wiring) before the
	// runState is built so re-resolved vars use the right PROJECT_DIR.
	// See restoreRunEnv for the rationale; in particular cp.Vars is
	// NOT used as the source of truth — re-resolving from r.Inputs
	// ensures any post-launch engine fix (env-expansion, new var
	// defaults) applies on resume too.
	e.restoreRunEnv(r)

	rs := e.newRunState(runID, r.Inputs)
	rs.vars = e.resolveVars(r.Inputs)
	rs.outputs = outputs
	rs.artifacts = e.rebuildArtifacts(outputs)
	rs.loopCounters = loopCounters
	rs.roundRobinCounters = roundRobinCounters
	rs.artifactVersions = artifactVersions
	rs.nodeAttempts = restoreNodeAttempts(cp.NodeAttempts)
	restoreLoopSnapshots(rs, cp)

	// Push the freshly-resolved vars into the executor so substitutions
	// in tool commands and prompt templates see the same map the engine
	// just built.
	e.pushExecutorVars(rs.vars)

	// When the pause originated from a delegate (an agent/judge that
	// emitted _needs_interaction or called the native ask_user tool),
	// the paused node still owes its work — re-invoke it with the
	// answer merged into the input. Without this branch, the runtime
	// would treat the agent/judge as a finished human node and skip
	// past it, losing the verdict it was supposed to produce.
	if cp.BackendName != "" {
		node, ok := e.workflow.Nodes[humanNodeID]
		if !ok {
			return fmt.Errorf("runtime: paused node %q not found in workflow", humanNodeID)
		}
		ni := &model.ErrNeedsInteraction{
			NodeID:           humanNodeID,
			Questions:        cp.InteractionQuestions,
			SessionID:        cp.BackendSessionID,
			Backend:          cp.BackendName,
			Conversation:     cp.BackendConversation,
			PendingToolUseID: cp.BackendPendingToolUseID,
		}
		loopErr := e.reInvokeBackend(ctx, rs, humanNodeID, node, ni, answers, 0)
		e.evictRunSessions(runID, loopErr)
		return loopErr
	}

	// Select edge from the human node to find the next node.
	nextNodeID, err := e.selectEdgeRS(rs, humanNodeID, answers)
	if err != nil {
		return e.failRunErrWithCheckpoint(rs, humanNodeID, err)
	}

	loopErr := e.execLoop(ctx, rs, nextNodeID)
	e.evictRunSessions(runID, loopErr)
	return loopErr
}

// resumeFromFailure resumes a failed_resumable run by re-executing from
// the failing node (with checkpoint) or by restarting from the workflow
// entry node (no checkpoint). The "no checkpoint" path matters when a
// run failed on its very first node before any save_checkpoint fired —
// network cuts during plan, claude_code subprocess crashes, etc. Without
// it, those runs are dead-on-arrival because validateResumable lets
// them through but the engine refuses to resume.
func (e *Engine) resumeFromFailure(ctx context.Context, r *store.Run) error {
	runID := r.ID
	if err := e.checkWorkflowHash(r); err != nil {
		return err
	}

	cp := r.Checkpoint
	restartNodeID := e.workflow.Entry
	if cp != nil {
		restartNodeID = cp.NodeID
	}

	// Update status to running and emit run_resumed.
	if err := e.store.UpdateRunStatus(runID, store.RunStatusRunning, ""); err != nil {
		return fmt.Errorf("runtime: update status running: %w", err)
	}
	resumeData := map[string]interface{}{
		"resumed_from": "failed",
		"restart_node": restartNodeID,
	}
	if cp == nil {
		resumeData["from_entry"] = true
	}
	if err := e.emit(runID, store.EventRunResumed, "", resumeData); err != nil {
		return err
	}

	// Restore the per-run env (workDir + executor wiring). cp.Vars is
	// not the source of truth; we re-resolve from r.Inputs so any
	// engine-side fix (e.g. env-var expansion of overrides, see commit
	// e9bf189) applies on resume too. Without this, a run launched on
	// an old binary that froze `${PROJECT_DIR}` literally in cp.Vars
	// would re-fail at the same node even after a fixed binary takes
	// over.
	e.restoreRunEnv(r)

	rs := e.newRunState(runID, r.Inputs)
	rs.vars = e.resolveVars(r.Inputs)

	if cp != nil {
		rs.outputs = cp.Outputs
		rs.artifacts = e.rebuildArtifacts(cp.Outputs)
		rs.loopCounters = cp.LoopCounters
		if cp.RoundRobinCounters != nil {
			rs.roundRobinCounters = cp.RoundRobinCounters
		}
		if cp.ArtifactVersions != nil {
			rs.artifactVersions = cp.ArtifactVersions
		}
		rs.nodeAttempts = restoreNodeAttempts(cp.NodeAttempts)
		restoreLoopSnapshots(rs, cp)
	}
	// When cp is nil, rs keeps the empty maps from newRunState — same
	// state shape as a fresh launch, only the run_id is preserved so
	// the editor's snapshot continuity stays intact.

	e.pushExecutorVars(rs.vars)

	loopErr := e.execLoop(ctx, rs, restartNodeID)
	e.evictRunSessions(runID, loopErr)
	return loopErr
}

// restoreRunEnv re-establishes the engine's working directory from the
// persisted run record, then propagates it to the executor. Mirrors the
// pre-loop initialisation in Run(): without this, resumed runs that
// originally used `worktree: auto` would lose track of their per-run
// worktree path and tool nodes / backend subprocesses would land in the
// main checkout (or even the iterion process cwd) — fatal for
// commit_changes which expects to operate inside the run's worktree.
//
// The persisted r.WorkDir was added by Run() after worktree creation, so
// it always carries the right absolute path even when the worktree on
// disk was removed (in which case the failing tool will produce a clear
// error rather than silently ending up somewhere else).
func (e *Engine) restoreRunEnv(r *store.Run) {
	if r.WorkDir != "" {
		e.workDir = r.WorkDir
	} else if e.workDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			e.workDir = cwd
		}
	}
	type workDirSetter interface{ SetWorkDir(string) }
	if s, ok := e.executor.(workDirSetter); ok {
		s.SetWorkDir(e.workDir)
	}
}

// pushExecutorVars refreshes the executor's vars map. Used after every
// resolveVars on the resume path; the launch path does this inline in
// Run().
func (e *Engine) pushExecutorVars(vars map[string]interface{}) {
	type varsSetter interface{ SetVars(map[string]interface{}) }
	if sv, ok := e.executor.(varsSetter); ok {
		sv.SetVars(vars)
	}
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
	nodeInput := e.buildNodeInputRS(nodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)
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
	questions := e.buildNodeInputRS(nodeID, rs.vars, rs.outputs, nil, rs.artifacts, rs)
	return e.doPause(rs, nodeID, questions, nil, pauseInfo{})
}

// ---------------------------------------------------------------------------
// Delegate interaction handling
// ---------------------------------------------------------------------------

// maxInteractionDepth caps the number of consecutive interaction
// auto-responses for a single node within one run. Each ErrNeedsInteraction
// → handleNeedsInteraction → reInvokeBackend cycle increments depth; the
// guard fires before reaching the budget/iteration limits, surfacing a
// clear error rather than silently grinding on a runaway LLM that keeps
// re-asking. 5 is generous for legitimate multi-turn dialogues — most
// real interactions resolve in 1–2 rounds.
const maxInteractionDepth = 5

// handleNeedsInteraction is called when a delegate or LLM signals it needs
// user input. The behavior depends on the node's InteractionMode:
//   - InteractionHuman: pause the workflow for human input
//   - InteractionLLM: auto-respond using the interaction model
//   - InteractionLLMOrHuman: LLM decides whether to respond or escalate
//
// depth tracks the number of consecutive auto-respond cycles for the
// current node. External callers pass 0; the recursive callsite in
// reInvokeBackend forwards depth+1.
func (e *Engine) handleNeedsInteraction(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction, depth int) error {
	if depth > maxInteractionDepth {
		return e.failRunWithCheckpoint(rs, nodeID, fmt.Sprintf(
			"node %q exceeded interaction recursion depth (%d > %d) — backend kept escalating without converging",
			nodeID, depth, maxInteractionDepth))
	}
	switch nodeInteraction(node) {
	case ir.InteractionHuman:
		return e.pauseForBackendInteraction(rs, nodeID, ni)

	case ir.InteractionLLM:
		return e.handleInteractionLLM(ctx, rs, nodeID, node, ni, depth)

	case ir.InteractionLLMOrHuman:
		return e.handleInteractionLLMOrHuman(ctx, rs, nodeID, node, ni, depth)

	default:
		// InteractionNone should not reach here (executor wouldn't return ErrNeedsInteraction).
		return fmt.Errorf("runtime: node %q received interaction request but has interaction: none", nodeID)
	}
}

// handleInteractionLLM invokes the interaction model to auto-respond to the
// delegate's questions, then re-invokes the backend with the answers.
func (e *Engine) handleInteractionLLM(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction, depth int) error {
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
	return e.reInvokeBackend(ctx, rs, nodeID, node, ni, answers, depth)
}

// handleInteractionLLMOrHuman invokes the interaction model to decide whether
// to auto-respond or escalate to a human. If the LLM sets needs_human_input=true,
// the run is paused for human input.
func (e *Engine) handleInteractionLLMOrHuman(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction, depth int) error {
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
	return e.reInvokeBackend(ctx, rs, nodeID, node, ni, answers, depth)
}

// reInvokeBackend re-invokes the delegate backend with the LLM-provided
// answers merged into the node input. It uses the delegate's session ID
// for session continuity so the backend can resume where it left off.
//
// When the prior interaction came from the native ask_user tool, the
// question text is also relayed via reserved keys so the executor can
// prepend a "[PRIOR INTERACTION]" block to the user prompt — without
// this, claw's stateless re-invocation would lose the question and the
// LLM might call ask_user with the same question again.
func (e *Engine) reInvokeBackend(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction, answers map[string]interface{}, depth int) error {
	// Build the input for re-invocation: original node input + answers.
	nodeInput := e.buildNodeInputRS(nodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)
	for k, v := range answers {
		nodeInput[k] = v
	}
	if q, ok := ni.Questions[delegate.AskUserQuestionKey]; ok {
		nodeInput[delegate.PriorAskUserQuestionKey] = q
		if a, ok := answers[delegate.AskUserQuestionKey]; ok {
			nodeInput[delegate.PriorAskUserAnswerKey] = a
		}
	}

	// When the backend captured the LLM's conversation at the pause point
	// (claw), relay it through the executor so the Task carries
	// ResumeConversation/ResumePendingToolUseID/ResumeAnswer. The backend
	// then rehydrates the message history and continues the agent loop
	// instead of restarting from system+user prompts.
	if len(ni.Conversation) > 0 && ni.PendingToolUseID != "" {
		nodeInput[delegate.ResumeConversationKey] = ni.Conversation
		nodeInput[delegate.ResumePendingToolUseIDKey] = ni.PendingToolUseID
		if a, ok := answers[delegate.AskUserQuestionKey].(string); ok {
			nodeInput[delegate.ResumeAnswerKey] = a
		}
	}

	// Re-execute the node. The executor will use the session ID for
	// delegate re-invocation if the backend supports it.
	output, err := e.executor.Execute(ctx, node, nodeInput)
	if err != nil {
		// Check for another interaction request (recursive). depth+1
		// so the maxInteractionDepth guard in handleNeedsInteraction
		// fires before a runaway LLM chains escalations to budget
		// exhaustion.
		var needsInput *model.ErrNeedsInteraction
		if errors.As(err, &needsInput) {
			return e.handleNeedsInteraction(ctx, rs, nodeID, node, needsInput, depth+1)
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
	nextNodeID, err := e.selectEdgeRS(rs, nodeID, output)
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
	pi := pauseInfo{
		BackendSessionID:        ni.SessionID,
		BackendName:             ni.Backend,
		BackendConversation:     ni.Conversation,
		BackendPendingToolUseID: ni.PendingToolUseID,
	}
	if err := e.doPause(rs, nodeID, ni.Questions, eventExtra, pi); err != nil {
		return err
	}
	return ErrRunPaused
}

// pauseInfo bundles the optional backend-side state captured at pause
// time. It travels into the checkpoint so resume can either re-invoke
// the backend with the original session ID (CLI backends) or replay the
// persisted conversation (claw).
type pauseInfo struct {
	BackendSessionID        string
	BackendName             string
	BackendConversation     json.RawMessage
	BackendPendingToolUseID string
}

// doPause is the unified implementation for pausing a run. It writes the
// interaction record, emits pause events, and saves the checkpoint.
func (e *Engine) doPause(rs *runState, nodeID string, questions map[string]interface{}, eventExtra map[string]interface{}, info pauseInfo) error {
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
	cp.BackendSessionID = info.BackendSessionID
	cp.BackendName = info.BackendName
	cp.BackendConversation = info.BackendConversation
	cp.BackendPendingToolUseID = info.BackendPendingToolUseID
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
