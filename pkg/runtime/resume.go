package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
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
	r, err := e.store.LoadRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("runtime: load run for resume: %w", err)
	}
	switch r.Status {
	case store.RunStatusPausedWaitingHuman:
		return e.resumeFromPause(ctx, r, answers)
	case store.RunStatusFailedResumable, store.RunStatusCancelled, store.RunStatusPausedOperator:
		// paused_operator resumes via the same machinery as cancelled
		// runs: checkpoint preserved, no pending interaction, restart
		// from the node about to execute when the pause fired.
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
	interaction, err := e.store.LoadInteraction(ctx, runID, cp.InteractionID)
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
	if err := e.store.WriteInteraction(ctx, interaction); err != nil {
		return fmt.Errorf("runtime: write answered interaction: %w", err)
	}

	// Emit human_answers_recorded.
	if err := e.emit(ctx, runID, store.EventHumanAnswersRecorded, humanNodeID, map[string]interface{}{
		"interaction_id": cp.InteractionID,
		"answers":        answers,
	}); err != nil {
		return err
	}

	// Store human answers as the output of the human node. Deep-copy
	// the checkpoint's outputs so subsequent rs.outputs writes do not
	// retroactively mutate the persisted checkpoint object — sibling
	// to the fan-out deep-copy guarantee in commit bb34f844. Initialize
	// when the legacy/Mongo-omitted-shape produces a nil map.
	outputs := copyOutputs(cp.Outputs)
	if outputs == nil {
		outputs = make(map[string]map[string]interface{})
	}
	outputs[humanNodeID] = answers

	// Persist artifact if node has publish.
	humanNode, ok := e.workflow.Nodes[humanNodeID]
	if !ok {
		return fmt.Errorf("runtime: human node %q not found in workflow", humanNodeID)
	}
	artifactVersions := cp.ArtifactVersions
	if artifactVersions == nil {
		artifactVersions = make(map[string]int)
	}
	if pub := nodePublish(humanNode); pub != "" {
		version := artifactVersions[humanNodeID]
		artifact := &store.Artifact{
			RunID:   runID,
			NodeID:  humanNodeID,
			Version: version,
			Data:    answers,
		}
		if err := e.store.WriteArtifact(ctx, artifact); err != nil {
			return fmt.Errorf("runtime: write human artifact: %w", err)
		}
		artifactVersions[humanNodeID] = version + 1
		// The artifact itself is durably written; the event is
		// observational. Best-effort emit — log the failure so the
		// observability gap is visible rather than swallowing it
		// entirely on the resume path.
		if err := e.emit(ctx, runID, store.EventArtifactWritten, humanNodeID, map[string]interface{}{
			"publish": pub,
			"version": version,
		}); err != nil && e.logger != nil {
			e.logger.Warn("runtime: resume: failed to emit artifact_written for human node %q version %d: %v", humanNodeID, version, err)
		}
	}

	// Mark human node as finished.
	if err := e.emit(ctx, runID, store.EventNodeFinished, humanNodeID, nil); err != nil {
		return err
	}

	// Update status to running and emit run_resumed.
	if err := e.store.UpdateRunStatus(ctx, runID, store.RunStatusRunning, ""); err != nil {
		return fmt.Errorf("runtime: update status running: %w", err)
	}
	if err := e.emit(ctx, runID, store.EventRunResumed, "", nil); err != nil {
		return err
	}

	// Build runState before edge selection so failures are resumable.
	// Init maps when the checkpoint deserialised with omitted fields
	// (Mongo bson omitempty, legacy stores) — a nil map here would
	// crash selectEdgeRS the first time it tries `rs.loopCounters[X]++`.
	loopCounters := cp.LoopCounters
	if loopCounters == nil {
		loopCounters = make(map[string]int)
	}
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

	// Re-mirror bundle skills on resume so a `.botz` upgrade between
	// the original launch and the resume picks up the new skill bodies
	// inside <workDir>/.claude/skills/. Without this, an agent inside
	// a resumed paused run reads the v0.1.0 skill content even though
	// the host has v0.2.0 — the marker file logic preserves any user
	// customisation. See F-RT-7.
	if err := mirrorBundleSkills(e.workDir, e.bundle, e.logger); err != nil {
		return fmt.Errorf("runtime: bundle skills (resume): %w", err)
	}

	// Re-bootstrap the sandbox container (see resumeFromFailure for the
	// rationale — same lifecycle issue applies here: the original Run()
	// deferred shutdown when it exited to pause, so e.sandbox is nil
	// on resume and tool nodes downstream of the human would fall back
	// to host execution).
	repoRoot := r.RepoRoot
	if repoRoot == "" {
		repoRoot = engineRepoRoot(e.workDir)
	}
	sandboxCleanup, sbErr := e.startSandbox(ctx, runID, repoRoot, resolveWorktreeGitDir(repoRoot, r.WorkDir))
	if sbErr != nil {
		// Same rationale as resumeFromFailure: a sandbox-start failure
		// at resume time is almost always recoverable (stale container,
		// docker hiccup, image pull race). Marking failed_resumable
		// preserves the captured human answers + checkpoint so a
		// follow-up /resume can complete once docker is unblocked.
		//
		// PRESERVE the rich checkpoint (outputs, loop counters, artifact
		// versions) — a NodeID-only stub would wipe everything the bot
		// accumulated and force the next resume to restart from entry,
		// defeating the failed_resumable contract. Observed 2026-05-25
		// during the issue #5 dogfood (finding `resume-orphan-gap.md`):
		// repeated sandbox-failure resumes silently nil'd Outputs across
		// 4 attempts before a watchexec restart re-fired the entire bot
		// from `plan`, wasting 1.5h of prior work.
		preservedCp := r.Checkpoint
		if preservedCp == nil {
			preservedCp = &store.Checkpoint{NodeID: humanNodeID}
		}
		if err := e.store.FailRunResumable(ctx, runID, preservedCp, sbErr.Error()); err != nil {
			// FailRunResumable failed too — fall back to a hard
			// "failed" status flip so the run doesn't appear stuck.
			// If even that fails, log loudly: the store is in a bad
			// state and silently dropping the second failure would
			// leave the run in `running` forever.
			if uerr := e.store.UpdateRunStatus(ctx, runID, store.RunStatusFailed, sbErr.Error()); uerr != nil && e.logger != nil {
				e.logger.Warn("runtime: resume: failed both FailRunResumable (%v) and UpdateRunStatus (%v) for run %s after sandbox error: %v", err, uerr, runID, sbErr)
			}
		}
		return fmt.Errorf("runtime: sandbox: %w", sbErr)
	}
	defer sandboxCleanup()

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
	// Resumed worktree runs need the same persistent-branch + FF step
	// fresh launches get; without this the GC guard never lands and
	// commits are reachable only via reflog. See F-RT-1.
	if wtCtx := e.reconstructWorktreeContext(r); wtCtx != nil {
		e.finalizeOnExit(ctx, runID, wtCtx, nil, loopErr)
	}
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
	if err := e.store.UpdateRunStatus(ctx, runID, store.RunStatusRunning, ""); err != nil {
		return fmt.Errorf("runtime: update status running: %w", err)
	}
	resumeData := map[string]interface{}{
		"resumed_from": "failed",
		"restart_node": restartNodeID,
	}
	if cp == nil {
		resumeData["from_entry"] = true
	}
	if err := e.emit(ctx, runID, store.EventRunResumed, "", resumeData); err != nil {
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

	// Re-mirror bundle skills on resume (F-RT-7) — same rationale as
	// resumeFromPause: a bundle upgrade between launch and resume
	// would otherwise leave the agent reading stale skill content.
	if err := mirrorBundleSkills(e.workDir, e.bundle, e.logger); err != nil {
		return fmt.Errorf("runtime: bundle skills (resume): %w", err)
	}

	// Re-bootstrap the sandbox container. The original Run() process
	// owned it through a defer that ran on exit, so a resumed run finds
	// e.sandbox == nil and would otherwise fall back to running every
	// tool node on the host. Without this, recipes that rely on the
	// sandbox toolchain (modern dash for `set -o pipefail`, /workspace
	// bind mount, the slim image's git/jq/curl/node pinned versions,
	// ...) silently fail in confusing ways post-resume. r.RepoRoot is
	// the source of truth for the original repo root; engineRepoRoot
	// is the worktree-less fallback for older runs.
	repoRoot := r.RepoRoot
	if repoRoot == "" {
		repoRoot = engineRepoRoot(e.workDir)
	}
	sandboxCleanup, sbErr := e.startSandbox(ctx, runID, repoRoot, resolveWorktreeGitDir(repoRoot, r.WorkDir))
	if sbErr != nil {
		// Sandbox-start failures on resume are almost always recoverable
		// from the operator's side: stale containers (force-removable),
		// docker daemon hiccups, image pull races, etc. Marking the run
		// `failed_resumable` instead of `failed` keeps the door open for
		// a second /resume after the operator addresses the underlying
		// cause — the alternative (status=failed) is terminal and forces
		// a fresh launch that loses all committed per-package work.
		// PRESERVE the rich checkpoint — same rationale as the sister
		// branch in resumeFromPause: a NodeID-only stub here would wipe
		// Outputs/LoopCounters/etc and force the next resume to restart
		// from entry. Issue #5's 2026-05-25 dogfood showed this in
		// practice: 4 sandbox-related sub-failures across the run
		// silently degraded the checkpoint until a final watchexec
		// restart re-ran the bot from `plan`, throwing away 1.5h of
		// fix_claude iterations.
		preservedCp := cp
		if preservedCp == nil {
			preservedCp = &store.Checkpoint{NodeID: e.workflow.Entry}
		}
		if err := e.store.FailRunResumable(ctx, runID, preservedCp, sbErr.Error()); err != nil {
			_ = e.store.UpdateRunStatus(ctx, runID, store.RunStatusFailed, sbErr.Error())
		}
		return fmt.Errorf("runtime: sandbox: %w", sbErr)
	}
	defer sandboxCleanup()

	rs := e.newRunState(runID, r.Inputs)
	rs.vars = e.resolveVars(r.Inputs)

	if cp != nil {
		// Deep-copy outputs so subsequent writes on rs.outputs don't
		// retroactively mutate r.Checkpoint (which any HTTP read still
		// holding the run pointer could iterate concurrently). Same
		// sibling-isolation discipline as fan_out.go's copyOutputs.
		rs.outputs = copyOutputs(cp.Outputs)
		if rs.outputs == nil {
			rs.outputs = make(map[string]map[string]interface{})
		}
		rs.artifacts = e.rebuildArtifacts(rs.outputs)
		// Each below: init when the checkpoint deserialised with omitted
		// fields, otherwise selectEdgeRS / loop-counter increments
		// panic on the first write.
		if cp.LoopCounters != nil {
			rs.loopCounters = cp.LoopCounters
		}
		if cp.RoundRobinCounters != nil {
			rs.roundRobinCounters = cp.RoundRobinCounters
		}
		if cp.ArtifactVersions != nil {
			rs.artifactVersions = cp.ArtifactVersions
		}
		rs.nodeAttempts = restoreNodeAttempts(cp.NodeAttempts)
		restoreLoopSnapshots(rs, cp)
		// Fork rehydration: when the checkpoint carries a backend
		// conversation (claw) or session id (claude_code), pin them to
		// runState so the first execution of cp.NodeID injects them
		// into the input map. Cleared after the first injection so
		// downstream nodes don't accidentally rehydrate.
		if len(cp.BackendConversation) > 0 || cp.BackendSessionID != "" {
			rs.resumeBackend = resumeBackendState{
				nodeID:       cp.NodeID,
				conversation: cp.BackendConversation,
				sessionID:    cp.BackendSessionID,
			}
		}
	}
	rs.isWorktree = r.Worktree
	// When cp is nil, rs keeps the empty maps from newRunState — same
	// state shape as a fresh launch, only the run_id is preserved so
	// the studio's snapshot continuity stays intact.

	e.pushExecutorVars(rs.vars)

	loopErr := e.execLoop(ctx, rs, restartNodeID)
	e.evictRunSessions(runID, loopErr)
	// Mirrors resumeFromPause: a worktree run that fails resumably and
	// then completes on resume must finalize, otherwise its commits
	// stay reflog-only. See F-RT-1.
	if wtCtx := e.reconstructWorktreeContext(r); wtCtx != nil {
		e.finalizeOnExit(ctx, runID, wtCtx, nil, loopErr)
	}
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
	// Mirror the run's repo root onto the engine so resolveVars's
	// `${PROJECT_MEMORY_DIR}` expansion finds the same path it did
	// on the original launch — without this, a resumed run on a
	// dispatcher worktree falls back to e.workDir's encoded key
	// and the memory tree silently moves.
	e.repoRoot = r.RepoRoot
	type workDirSetter interface{ SetWorkDir(string) }
	if s, ok := e.executor.(workDirSetter); ok {
		s.SetWorkDir(e.workDir)
	}
	type repoRootSetter interface{ SetRepoRoot(string) }
	if s, ok := e.executor.(repoRootSetter); ok {
		s.SetRepoRoot(r.RepoRoot)
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
	iter := e.currentLoopIteration(nodeID, rs.loopCounters)
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind":      node.NodeKind().String(),
		"iteration": iter,
	}); err != nil {
		return false, err
	}

	// Check budget.
	if err := e.checkBudgetBeforeExec(rs, nodeID); err != nil {
		return false, err
	}

	// Build input and execute LLM.
	nodeInput := e.buildNodeInputRS(nodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)
	execCtx := model.WithLoopIteration(ctx, iter)
	output, err := e.executor.Execute(execCtx, node, nodeInput)
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
		if err := e.store.WriteArtifact(ctx, artifact); err != nil {
			return false, fmt.Errorf("runtime: write artifact: %w", err)
		}
		rs.artifactVersions[nodeID] = version + 1
		rs.artifacts[pub] = output
		_ = e.emit(rs.ctx, rs.runID, store.EventArtifactWritten, nodeID, map[string]interface{}{
			"publish": pub,
			"version": version,
		})
	}

	// Emit node_finished.
	nodeFinishedData := buildNodeFinishedData(sanitizeOutputForEvent(node, output))
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, nodeID, nodeFinishedData); err != nil {
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
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, map[string]interface{}{
		"kind":      node.NodeKind().String(),
		"iteration": e.currentLoopIteration(nodeID, rs.loopCounters),
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
	execCtx := e.ctxWithIteration(ctx, nodeID, rs.loopCounters)
	output, err := e.executor.Execute(execCtx, node, nodeInput)
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
		if err := e.store.WriteArtifact(ctx, artifact); err != nil {
			return fmt.Errorf("runtime: write artifact: %w", err)
		}
		rs.artifactVersions[nodeID] = version + 1
		rs.artifacts[pub] = output
		_ = e.emit(rs.ctx, rs.runID, store.EventArtifactWritten, nodeID, map[string]interface{}{
			"publish": pub,
			"version": version,
		})
	}

	// Emit node_finished.
	nodeFinishedData := buildNodeFinishedData(sanitizeOutputForEvent(node, output))
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, nodeID, nodeFinishedData); err != nil {
		return err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(nodeID, output)
	}

	// Checkpoint.
	if err := e.store.SaveCheckpoint(rs.ctx, rs.runID, buildCheckpoint(rs, nodeID)); err != nil {
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

// drainOperatorMessagesForPause empties the run's operator-queued
// chat-message inbox at pause time and returns the texts in FIFO
// order. Used by the claude_code / codex pause path — those backends
// can't accept mid-session stdin, so the operator's intent rides on
// the resume system prompt. Each transition emits a
// user_message_delivered event through the engine's event observer
// so WS subscribers (the studio chatbox) update their badge.
//
// Side-effect: before returning, every SkillRef attached to a drained
// message is mirrored into the run's .claude/skills/ directory via
// MirrorSingleSkill. Sticky — the skill stays loaded for the rest of
// the run. Mirror failures log at warn level but don't block the
// drain (the agent will see the text without the skill in those
// cases; the operator surfaces the gap via the catalog endpoint).
func (e *Engine) drainOperatorMessagesForPause(ctx context.Context, runID string) []string {
	msgs, _, _ := store.DrainPendingMessages(ctx, e.store, e.onEvent, runID)
	if len(msgs) == 0 {
		return nil
	}
	texts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		texts = append(texts, m.Text)
		for _, ref := range m.SkillRefs {
			if err := MirrorSingleSkill(e.workDir, e.bundle, ref, e.logger); err != nil && e.logger != nil {
				e.logger.Warn("queued-message skill mirror failed: ref=%s err=%v", ref, err)
			}
		}
	}
	return texts
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
	// Drain operator-queued chatbox messages and stamp them onto the
	// interaction questions under a reserved key. The resume path
	// reads the same key and folds the messages into the system
	// prompt so claude_code / codex (which can't accept mid-session
	// stdin) still see the operator's intent. claw drains the same
	// inbox between agent iterations (model.StoreInboxBinder), so on
	// most runs the queue is already empty by the time we land here.
	if queuedTexts := e.drainOperatorMessagesForPause(rs.ctx, rs.runID); len(queuedTexts) > 0 {
		if questions == nil {
			questions = map[string]interface{}{}
		}
		questions[delegate.QueuedOperatorMessagesKey] = queuedTexts
	}
	interaction := &store.Interaction{
		ID:          interactionID,
		RunID:       rs.runID,
		NodeID:      nodeID,
		RequestedAt: time.Now().UTC(),
		Questions:   questions,
	}
	if err := e.store.WriteInteraction(rs.ctx, interaction); err != nil {
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
	if err := e.emit(rs.ctx, rs.runID, store.EventHumanInputRequested, nodeID, eventData); err != nil {
		return err
	}

	// Emit run_paused.
	if err := e.emit(rs.ctx, rs.runID, store.EventRunPaused, nodeID, nil); err != nil {
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
	if err := e.store.PauseRun(rs.ctx, rs.runID, cp); err != nil {
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
	// Loop body membership is the authoritative signal — a node is "inside"
	// loop L iff it's reachable from L's entry within L.Body. The snapshot
	// reducer also consumes `iteration_path` (see currentLoopIterationPath
	// below) to disambiguate executions of the same node across nested
	// loops; this scalar `iteration` is retained for the studio's pip strip
	// + the legacy reducer fallback.
	for loopName, loop := range e.workflow.Loops {
		if loop == nil {
			continue
		}
		if !loop.Body[nodeID] {
			continue
		}
		if count, ok := loopCounters[loopName]; ok && count > maxIter {
			maxIter = count
		}
	}
	// Belt-and-suspenders: a node sitting exactly on a loop-bearing edge
	// (the loop's entry or back-edge endpoint) gets counted via the body
	// path above when the compiler marks it as such, but fall back to the
	// edge-endpoint scan for workflows whose Loop.Body is empty (older
	// IRs / hand-written test fixtures).
	for _, edge := range e.workflow.Edges {
		if edge.LoopName == "" {
			continue
		}
		if edge.From == nodeID || edge.To == nodeID {
			if count, ok := loopCounters[edge.LoopName]; ok && count > maxIter {
				maxIter = count
			}
		}
	}
	return maxIter
}

// currentLoopIterationPath returns a stable string encoding of the
// counters of EVERY loop currently containing nodeID. The snapshot
// reducers (backend + studio) use it to build a unique exec_id when a
// node sits in nested loops — observed live: validate_upgrade lives in
// fix_loop ⊂ package_loop ⊂ family_loop, and the single-int iteration
// scheme collapsed pkg N's attempt 0 and pkg N+1's attempt 0 onto the
// same exec because the family_loop counter dominated the max. Encoding
// `family=5,package=0,fix=3` gives each execution attempt a strictly
// unique identity regardless of which loop's counter happens to win.
//
// The encoding is `<loopName>=<count>` segments joined by `;` in
// LEXICOGRAPHIC loop-name order so the same {loops × counters} set
// always renders to the same string (stable across runs, replay-safe).
// Empty string when the node is in zero loops — reducers fall back to
// the scalar `iteration` field.
//
// Edge endpoint membership is also honoured here for the same belt-
// and-suspenders reason as currentLoopIteration: a workflow whose
// Loop.Body is empty (older IRs / hand-written fixtures) still gets a
// usable path keyed on the loop-bearing edges the node touches.
func (e *Engine) currentLoopIterationPath(nodeID string, loopCounters map[string]int) string {
	memberOf := make(map[string]struct{})
	for loopName, loop := range e.workflow.Loops {
		if loop == nil {
			continue
		}
		if loop.Body[nodeID] {
			memberOf[loopName] = struct{}{}
		}
	}
	for _, edge := range e.workflow.Edges {
		if edge.LoopName == "" {
			continue
		}
		if edge.From == nodeID || edge.To == nodeID {
			memberOf[edge.LoopName] = struct{}{}
		}
	}
	if len(memberOf) == 0 {
		return ""
	}
	names := make([]string, 0, len(memberOf))
	for n := range memberOf {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, loopCounters[n]))
	}
	return strings.Join(parts, ";")
}

// ctxWithIteration wraps ctx with the current loop iteration for nodeID
// so the executor can stamp Task.Iteration and downstream backends can
// tag their log output as [NodeID#iter/...].
func (e *Engine) ctxWithIteration(ctx context.Context, nodeID string, loopCounters map[string]int) context.Context {
	return model.WithLoopIteration(ctx, e.currentLoopIteration(nodeID, loopCounters))
}
