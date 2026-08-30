package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Branch execution
// ---------------------------------------------------------------------------

// branchResult holds the outcome of a single parallel branch.
type branchResult struct {
	branchID         string
	startNodeID      string
	outputs          map[string]map[string]any
	artifacts        map[string]map[string]any // publish name → output
	artifactVersions map[string]int
	joinNodeID       string // the join node this branch converged to (empty if terminal)
	err              error
	eventErrors      int // count of event emission failures (best-effort events)
	// terminatedAtDone is true when the branch loop exited at an
	// *ir.DoneNode rather than at a convergence/error/cancel. Used by
	// best_effort fan_out to recognise the "every branch finished at
	// its own done" topology — without this flag the post-loop
	// convergence search would fail because joinNodeID is empty on each
	// branch and preComputedConvergence is also empty (the branches
	// diverge to distinct terminals).
	terminatedAtDone bool
	terminalNodeID   string // the *ir.DoneNode ID when terminatedAtDone
	// selectedIncoming is this branch's private copy of the edges that
	// fired into each node it executed (or the join it stopped at).
	// Concurrent branches must not write the trunk runState map.
	selectedIncoming map[string][]store.IncomingEdge
	// costUSD is the branch's cumulative LLM spend for this invocation,
	// seeded from the durable branch cursor so a resumed pass keeps
	// growing the same monotonic-max daily-cap ledger entry.
	costUSD float64
}

// errBranchPauseDeferred marks a branch that reached a human gate after a
// sibling had already persisted the invocation's one active interaction. It
// is intentionally distinct from ErrRunPaused: only the elected branch may
// report the whole run as durably paused.
var errBranchPauseDeferred = errors.New("runtime: branch human pause deferred to elected sibling")

func (e *Engine) branchNodeIteration(nodeID string, rs *runState) (int, string) {
	counters := branchIterationCounters(rs)
	path := branchCounterPath(counters)
	if path == "root" {
		path = ""
	}
	return e.currentLoopIteration(nodeID, counters), path
}

// execBranch runs a single parallel branch starting from the target of
// the given edge. It executes nodes sequentially until it reaches a
// convergence point, a terminal node, or encounters an error.
// convergenceNodeID is the pre-computed convergence point (may be empty
// if unknown; in that case, AwaitMode on individual nodes is checked).
func (e *Engine) execBranch(ctx context.Context, rs *runState, branchID string, startEdge *ir.Edge, parentOutputs map[string]map[string]any, parentArtifacts map[string]map[string]any, convergenceNodeID string, slot *branchSlot, parallel *parallelExecutionState) *branchResult {
	branchCP := parallel.branch(branchID)
	result := initBranchResult(rs, branchID, branchCP)
	result.startNodeID = startEdge.To
	if branchCP != nil && branchCP.StartNodeID != "" {
		result.startNodeID = branchCP.StartNodeID
	}
	branchRS := newBranchRunState(parallel.branchBase(rs), branchCP, result)
	// Branch-local helpers must observe cancellation when this invocation is
	// abandoned; the parallel epoch separately rejects its late durable writes.
	branchRS.ctx = ctx
	branchRS.outputs = mergeOutputs(parentOutputs, result.outputs)
	branchRS.artifacts = mergeOutputs(parentArtifacts, result.artifacts)
	if branchCP == nil || len(branchCP.SelectedIncoming) == 0 {
		recordIncoming(result.selectedIncoming, startEdge, true)
	}
	runID := rs.runID

	// result.costUSD is this branch's cumulative LLM spend, recorded into the
	// shared daily-cap ledger under ledgerKey. The key carries a per-invocation
	// sequence ("<runID>#<branchID>#<seq>") so concurrent branches don't
	// clobber each other's monotonic-max entry AND a fan-out re-run inside a
	// loop gets a fresh key each iteration (branchID alone repeats across
	// iterations, which would make the monotonic-max keep only the costliest
	// one instead of summing) — see recordBranchUsage. branchLedgerSeq is
	// process-local and restarts at zero on resume, so a resumed branch
	// re-uses its pre-pause key; initBranchResult seeds the accumulator from
	// the durable cursor so the monotonic-max entry keeps growing instead of
	// discarding everything the resumed pass spent.
	ledgerKey := fmt.Sprintf("%s#%s#%d", runID, branchID, rs.branchLedgerSeq.Add(1))

	// Emit branch_started (best-effort — branch can proceed without the event).
	if err := e.emitBranch(ctx, runID, branchID, store.EventBranchStarted, startEdge.To, nil); err != nil {
		e.logger.Warn("branch %s: failed to emit branch_started: %v", branchID, err)
		result.eventErrors++
	}

	// Always emit branch_finished, regardless of how the branch exits — the
	// started/finished pair tracks in-flight concurrency for observers.
	defer e.emitBranchFinishedDefer(ctx, runID, branchID, startEdge.To, result)

	// resumeGate names the human gate whose durable answers this pass consumed.
	// It stays set until checkpointResumedBranch lands the successor cursor;
	// an error exit in between (artifact write, no satisfiable outgoing edge,
	// a guard on an edited source, a cancel mid-node) hands the gate back so
	// the pending identity never outlives the branch that held it. A re-pause
	// (ErrRunPaused) re-elected the gate itself and keeps it.
	resumeGate := ""
	defer func() {
		if resumeGate != "" && result.err != nil && !errors.Is(result.err, ErrRunPaused) {
			e.abandonBranchResume(rs, branchRS, result, resumeGate, parallel)
		}
	}()

	currentNodeID := startEdge.To
	if branchCP != nil && branchCP.CurrentNodeID != "" {
		currentNodeID = branchCP.CurrentNodeID
	}
	defer func() {
		// A sibling pause cancels the invocation after its interaction is
		// durable. Persist this branch's current in-memory cursor once at that
		// cancellation boundary so completed linear work is not replayed on
		// every sibling resume. This is one write per interrupted branch/pause,
		// not one write per node.
		if result.err != nil && (errors.Is(result.err, ErrRunCancelled) || errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded)) {
			e.checkpointBranch(rs, branchRS, result, currentNodeID, false, parallel)
		}
	}()
	if branchCP != nil && branchCP.Completed {
		result.joinNodeID = branchCP.JoinNodeID
		result.terminatedAtDone = branchCP.TerminatedAtDone
		result.terminalNodeID = branchCP.TerminalNodeID
		return result
	}

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
			e.checkpointBranch(rs, branchRS, result, currentNodeID, true, parallel)
			return result
		}

		// Stop at terminal nodes within a branch.
		switch node.(type) {
		case *ir.DoneNode:
			result.terminatedAtDone = true
			result.terminalNodeID = currentNodeID
			e.checkpointBranch(rs, branchRS, result, currentNodeID, true, parallel)
			return result
		case *ir.FailNode:
			result.err = fmt.Errorf("branch %s reached fail node %q", branchID, currentNodeID)
			return result
		}

		// Check budget before execution.
		if e.checkPreExecBudget(ctx, branchRS, runID, branchID, currentNodeID, result) {
			return result
		}

		iter, iterPath := e.branchNodeIteration(currentNodeID, branchRS)
		var output map[string]any
		var done bool
		resumedHuman := false
		branchHuman := false
		if human, ok := node.(*ir.HumanNode); ok && human.Interaction != ir.InteractionLLM {
			branchHuman = true
			if human.Interaction == ir.InteractionReview || human.Interaction == ir.InteractionLLMOrHuman {
				e.emitBranchNodeStarted(ctx, runID, branchID, currentNodeID, node, iter, iterPath, result)
				result.err = fmt.Errorf("C245: human interaction %q is not supported inside an execution branch", human.Interaction)
				done = true
			} else if branchCP != nil && branchCP.ResumeAnswered {
				e.markPreNodeBoundary(branchRS, currentNodeID)
				output = deepCopyAnyMap(branchCP.ResumeAnswers)
				branchCP.ResumeAnswers = nil
				branchCP.ResumeAnswered = false
				if err := e.validateNodeOutput(currentNodeID, node, output); err != nil {
					// The interaction was already recorded as answered before the
					// branch restarted. Re-pause with a checkpoint that omits the bad
					// answer so the operator can correct it instead of wedging every
					// future resume on the same durable invalid payload.
					result.err = e.pauseBranchAtHuman(rs, branchRS, branchID, currentNodeID, node, parentOutputs, parentArtifacts, result, parallel, iter, iterPath, false)
					done = true
				} else {
					result.outputs[currentNodeID] = output
					resumedHuman = true
					resumeGate = currentNodeID
				}
			} else {
				done = true
				result.err = e.pauseBranchAtHuman(rs, branchRS, branchID, currentNodeID, node, parentOutputs, parentArtifacts, result, parallel, iter, iterPath, true)
			}
		} else {
			e.emitBranchNodeStarted(ctx, runID, branchID, currentNodeID, node, iter, iterPath, result)
			output, done = e.executeNodeForBranch(ctx, branchRS, runID, branchID, currentNodeID, node, parentOutputs, parentArtifacts, iter, result, slot)
		}
		if done {
			// A deferred gate never emitted node_started. A real pause keeps its
			// one start open until the answered resume emits node_finished. Every
			// other human-node failure closes the attempt in this invocation.
			if branchHuman && result.err != nil && !errors.Is(result.err, ErrRunPaused) && !errors.Is(result.err, errBranchPauseDeferred) {
				e.emitBranchNodeFailed(ctx, runID, branchID, currentNodeID, result.err, result)
			}
			return result
		}
		branchRS.outputs[currentNodeID] = output

		if e.recordBranchUsage(ctx, branchRS, runID, branchID, ledgerKey, currentNodeID, output, &result.costUSD, result) {
			return result
		}
		if parallel.isRetired() {
			result.err = e.wrapContextErr(context.Canceled)
			return result
		}

		if e.publishBranchArtifact(ctx, runID, branchID, currentNodeID, node, output, result, branchRS, parallel) {
			return result
		}

		// Emit node_finished with usage data.
		if err := e.emitBranch(ctx, runID, branchID, store.EventNodeFinished, currentNodeID, buildNodeFinishedData(e.sanitizeOutputForEvent(node, output))); err != nil {
			e.logger.Warn("branch %s: failed to emit node_finished: %v", branchID, err)
			result.eventErrors++
		}
		if e.onNodeFinished != nil {
			e.onNodeFinished(runID, currentNodeID, output)
		}

		selected, err := e.selectEdgeBranch(ctx, runID, branchID, currentNodeID, output, result, branchRS)
		if err != nil {
			result.err = err
			return result
		}

		currentNodeID = selected.To
		if resumedHuman {
			// Persist the successor and clear the old pending identity in one
			// snapshot before releasing siblings.
			e.checkpointResumedBranch(rs, branchRS, result, currentNodeID, parallel)
			resumeGate = ""
		} else if selected.IsBoundedIteration() {
			// A loop/foreach crossing is a restart boundary: persist its private
			// counters and successor cursor. Linear edges deliberately stay
			// in-memory; the next gate, iteration boundary, or branch completion
			// snapshots all progress at once instead of serializing every node on
			// the invocation-wide save mutex.
			e.checkpointBranch(rs, branchRS, result, currentNodeID, false, parallel)
		}
	}
}

// initBranchResult allocates a branch's result accumulator, copying the
// parent's artifact versions so the branch keeps incrementing from the
// correct version instead of resetting to 0 each fan-out cycle.
func initBranchResult(rs *runState, branchID string, cp *store.BranchCheckpoint) *branchResult {
	branchArtifactVersions := make(map[string]int, len(rs.artifactVersions))
	for k, v := range rs.artifactVersions {
		branchArtifactVersions[k] = v
	}
	result := &branchResult{
		branchID:         branchID,
		outputs:          make(map[string]map[string]any),
		artifacts:        make(map[string]map[string]any),
		artifactVersions: branchArtifactVersions,
		selectedIncoming: make(map[string][]store.IncomingEdge),
	}
	if cp != nil {
		result.outputs = copyOutputs(cp.Outputs)
		result.artifacts = copyOutputs(cp.Artifacts)
		if cp.ArtifactVersions != nil {
			result.artifactVersions = cloneMap(cp.ArtifactVersions)
		}
		if incoming := cloneIncoming(cp.SelectedIncoming); incoming != nil {
			result.selectedIncoming = incoming
		}
		result.costUSD = cp.CostUSD
	}
	return result
}

func newBranchRunState(parent *runState, cp *store.BranchCheckpoint, result *branchResult) *runState {
	local := cloneRunStateForBranch(parent)
	local.outputs = result.outputs
	local.artifacts = result.artifacts
	local.artifactVersions = result.artifactVersions
	local.selectedIncoming = result.selectedIncoming
	local.loopCounters = make(map[string]int)
	local.loopPreviousOutput = make(map[string]map[string]any)
	local.loopCurrentOutput = make(map[string]map[string]any)
	local.loopProgressSig = make(map[string]string)
	local.loopStaleness = make(map[string]int)
	local.loopBudgetMarks = make(map[string]loopBudgetMark)
	local.branchLocal = true
	local.enclosingLoopCounters = branchIterationCounters(parent)
	local.parallel = nil
	if cp != nil {
		local.loopCounters = cloneMap(cp.LoopCounters)
		if local.loopCounters == nil {
			local.loopCounters = make(map[string]int)
		}
		local.loopPreviousOutput = copyOutputs(cp.LoopPreviousOutput)
		local.loopCurrentOutput = copyOutputs(cp.LoopCurrentOutput)
		restoreLoopBudgetMarks(local, cp.LoopBudgetMarks)
	}
	return local
}

func branchCheckpointFromState(rs *runState, result *branchResult, currentNodeID string, completed bool) *store.BranchCheckpoint {
	return &store.BranchCheckpoint{
		BranchID:           result.branchID,
		StartNodeID:        result.startNodeID,
		CurrentNodeID:      currentNodeID,
		Outputs:            copyOutputs(result.outputs),
		Artifacts:          copyOutputs(result.artifacts),
		ArtifactVersions:   cloneMap(result.artifactVersions),
		LoopCounters:       cloneMap(rs.loopCounters),
		LoopPreviousOutput: copyOutputs(rs.loopPreviousOutput),
		LoopCurrentOutput:  copyOutputs(rs.loopCurrentOutput),
		LoopBudgetMarks:    snapshotLoopBudgetMarks(rs),
		SelectedIncoming:   cloneIncoming(result.selectedIncoming),
		JoinNodeID:         result.joinNodeID,
		TerminalNodeID:     result.terminalNodeID,
		Completed:          completed,
		TerminatedAtDone:   result.terminatedAtDone,
		CostUSD:            result.costUSD,
	}
}

// checkpointBranch persists a complete branch-local cursor while keeping the
// trunk anchored on the router. Callers use restart boundaries only (bounded
// iteration crossings and branch completion); human gates persist their cursor
// in pauseBranchAtHuman. SaveCheckpoint is best-effort, and the next durable
// branch boundary retries.
func (e *Engine) checkpointBranch(parent, branchRS *runState, result *branchResult, currentNodeID string, completed bool, parallel *parallelExecutionState) {
	e.checkpointBranchState(parent, branchRS, result, currentNodeID, completed, parallel, false)
}

func (e *Engine) checkpointResumedBranch(parent, branchRS *runState, result *branchResult, currentNodeID string, parallel *parallelExecutionState) {
	e.checkpointBranchState(parent, branchRS, result, currentNodeID, false, parallel, true)
}

// abandonBranchResume parks the branch back at gateNodeID with no consumed
// answers and no pending identity (see parallelExecutionState.abandonResume),
// and persists that snapshot so a restart cannot resurrect the stale gate. The
// write is best-effort like every branch checkpoint: under wait_all the trunk's
// failure checkpoint snapshots the same in-memory state right after.
func (e *Engine) abandonBranchResume(parent, branchRS *runState, result *branchResult, gateNodeID string, parallel *parallelExecutionState) {
	if parallel == nil || branchRS == nil {
		return
	}
	// The gate's output was the consumed answer; the re-ask produces a new one.
	delete(result.outputs, gateNodeID)
	delete(branchRS.outputs, gateNodeID)
	parallel.saveMu.Lock()
	defer parallel.saveMu.Unlock()
	branch := branchCheckpointFromState(branchRS, result, gateNodeID, false)
	if !parallel.abandonResume(branch) {
		return
	}
	ps := parallel.snapshot()
	if ps == nil {
		return
	}
	cp := buildCheckpointWithoutParallel(parent, ps.RouterNodeID)
	cp.Parallel = ps
	if err := e.store.SaveCheckpoint(parent.ctx, parent.runID, cp); err != nil && e.logger != nil {
		e.logger.Error("failed to save abandoned branch resume %s at %q: %v", result.branchID, gateNodeID, err)
	}
}

func (e *Engine) checkpointBranchState(parent, branchRS *runState, result *branchResult, currentNodeID string, completed bool, parallel *parallelExecutionState, completeResume bool) {
	if parallel == nil || branchRS == nil {
		return
	}
	parallel.saveMu.Lock()
	defer parallel.saveMu.Unlock()
	branch := branchCheckpointFromState(branchRS, result, currentNodeID, completed)
	var barrier chan struct{}
	var updated bool
	if completeResume {
		updated, barrier = parallel.updateResumedBranch(branch)
	} else {
		updated = parallel.updateBranch(branch)
	}
	if !updated {
		return
	}
	ps := parallel.snapshot()
	if ps == nil {
		return
	}
	cp := buildCheckpointWithoutParallel(parent, ps.RouterNodeID)
	cp.Parallel = ps
	if ps.PendingInteractionID != "" {
		cp.InteractionID = ps.PendingInteractionID
		cp.InteractionQuestions = deepCopyAnyMap(ps.PendingInteractionQuestions)
	}
	if err := e.store.SaveCheckpoint(parent.ctx, parent.runID, cp); err != nil && e.logger != nil {
		e.logger.Error("failed to save branch checkpoint %s at %q: %v", result.branchID, currentNodeID, err)
	}
	if barrier != nil {
		close(barrier)
	}
}

func (e *Engine) pauseBranchAtHuman(parent, branchRS *runState, branchID, nodeID string, node ir.Node, parentOutputs, parentArtifacts map[string]map[string]any, result *branchResult, parallel *parallelExecutionState, iter int, iterPath string, emitStarted bool) error {
	parallel.saveMu.Lock()
	defer parallel.saveMu.Unlock()
	branch := branchCheckpointFromState(branchRS, result, nodeID, false)
	elected, parked := parallel.beginPause(branchID, nodeID, branch)
	if !elected {
		if parked {
			e.persistParallelCheckpointLocked(parent, parallel, branchID, nodeID)
		}
		return errBranchPauseDeferred
	}
	pausePersisted := false
	defer func() {
		if !pausePersisted {
			parallel.abortPause(branchID, nodeID)
		}
	}()
	if emitStarted {
		e.emitBranchNodeStarted(parent.ctx, parent.runID, branchID, nodeID, node, iter, iterPath, result)
	}
	merged := mergeOutputs(parentOutputs, result.outputs)
	mergedArtifacts := mergeOutputs(parentArtifacts, result.artifacts)
	questions := e.buildNodeInputRS(nodeID, resolveScope{
		vars:           branchRS.vars,
		outputs:        merged,
		artifacts:      mergedArtifacts,
		rs:             branchRS,
		incomingByNode: result.selectedIncoming,
	})
	e.capturePauseBoundary(branchRS, nodeID)
	if queuedTexts := e.drainOperatorMessagesForPause(parent.ctx, parent.runID, nodeID); len(queuedTexts) > 0 {
		if questions == nil {
			questions = make(map[string]any)
		}
		questions[delegate.QueuedOperatorMessagesKey] = queuedTexts
	}
	executionCounters := branchIterationCounters(branchRS)
	scopeID := base64.RawURLEncoding.EncodeToString([]byte(branchID + ";" + branchCounterPath(executionCounters)))
	interactionID := e.interactionIDForPause(parent.runID, nodeID, executionCounters) + "_branch_" + scopeID
	parallel.setPendingInteraction(interactionID, questions)
	interaction := &store.Interaction{
		ID:          interactionID,
		RunID:       parent.runID,
		NodeID:      nodeID,
		RequestedAt: time.Now().UTC(),
		Questions:   questions,
	}
	if err := e.store.WriteInteraction(parent.ctx, interaction); err != nil {
		return fmt.Errorf("runtime: write branch interaction: %w", err)
	}
	eventData := map[string]any{
		"interaction_id": interactionID,
		"questions":      questions,
	}
	for key, value := range e.humanPauseExtra(nodeID, questions, branchRS) {
		eventData[key] = value
	}
	if err := e.emitBranch(parent.ctx, parent.runID, branchID, store.EventHumanInputRequested, nodeID, eventData); err != nil {
		return err
	}
	if err := e.emit(parent.ctx, parent.runID, store.EventRunPaused, nodeID, map[string]any{"branch_id": branchID}); err != nil {
		return err
	}
	ps := parallel.snapshot()
	cp := buildCheckpointWithoutParallel(parent, ps.RouterNodeID)
	cp.Parallel = ps
	cp.InteractionID = interactionID
	cp.InteractionQuestions = questions
	if err := e.store.PauseRun(parent.ctx, parent.runID, cp); err != nil {
		return fmt.Errorf("runtime: pause branch: %w", err)
	}
	pausePersisted = true
	return ErrRunPaused
}

// persistParallelCheckpointLocked writes the current invocation snapshot while
// the caller holds parallel.saveMu. It is used for a branch that reached a
// human gate but lost the one-interaction election: the elected sibling stays
// pending, while this branch's gate cursor must survive the next resume.
func (e *Engine) persistParallelCheckpointLocked(parent *runState, parallel *parallelExecutionState, branchID, nodeID string) {
	ps := parallel.snapshot()
	if ps == nil {
		return
	}
	cp := buildCheckpointWithoutParallel(parent, ps.RouterNodeID)
	cp.Parallel = ps
	if ps.PendingInteractionID != "" {
		cp.InteractionID = ps.PendingInteractionID
		cp.InteractionQuestions = deepCopyAnyMap(ps.PendingInteractionQuestions)
	}
	if err := e.store.SaveCheckpoint(parent.ctx, parent.runID, cp); err != nil && e.logger != nil {
		e.logger.Error("failed to save deferred branch checkpoint %s at %q: %v", branchID, nodeID, err)
	}
}

// emitBranchNodeStarted records one logical branch-node attempt. Human gates
// call it only after winning the pause election; answered resumes continue the
// original attempt and therefore skip a duplicate start.
func (e *Engine) emitBranchNodeStarted(ctx context.Context, runID, branchID, nodeID string, node ir.Node, iter int, iterPath string, result *branchResult) {
	data := map[string]any{
		"kind":      node.NodeKind().String(),
		"iteration": iter,
	}
	if iterPath != "" {
		data["iteration_path"] = iterPath
	}
	if err := e.emitBranch(ctx, runID, branchID, store.EventNodeStarted, nodeID, data); err != nil {
		e.logger.Warn("branch %s: failed to emit node_started: %v", branchID, err)
		result.eventErrors++
	}
}

func (e *Engine) emitBranchNodeFailed(ctx context.Context, runID, branchID, nodeID string, nodeErr error, result *branchResult) {
	if err := e.emitBranch(ctx, runID, branchID, store.EventNodeFinished, nodeID, map[string]any{"error": nodeErr.Error()}); err != nil {
		e.logger.Warn("branch %s: failed to emit node_finished: %v", branchID, err)
		result.eventErrors++
	}
}

// emitBranchFinishedDefer emits branch_finished (with the branch's terminal
// error / join node, if any). Always invoked via defer so the
// started/finished pair closes on every exit path — observers (e.g. the
// Prometheus parallel-branches gauge) rely on it to track in-flight
// concurrency. result is taken by pointer so the deferred read sees the
// branch's final state.
func (e *Engine) emitBranchFinishedDefer(ctx context.Context, runID, branchID, startNodeID string, result *branchResult) {
	data := map[string]any{}
	if result.err != nil {
		data["error"] = result.err.Error()
	}
	if result.joinNodeID != "" {
		data["join_node"] = result.joinNodeID
	}
	if err := e.emitBranch(ctx, runID, branchID, store.EventBranchFinished, startNodeID, data); err != nil {
		e.logger.Warn("branch %s: failed to emit branch_finished: %v", branchID, err)
		result.eventErrors++
	}
}

// checkPreExecBudget emits budget_exceeded and sets result.err (returning
// true → caller returns the branch) when the run budget is soft-exceeded or
// hard-limited before executing a node. Both checks emit the event before
// failing; emission failures are best-effort (counted on result.eventErrors).
func (e *Engine) checkPreExecBudget(ctx context.Context, rs *runState, runID, branchID, currentNodeID string, result *branchResult) bool {
	if rs.budget == nil {
		return false
	}
	checks := rs.budget.Check()
	if exc := findExceeded(checks); exc != nil {
		// Branch-local predictive pricing is deliberately disabled because
		// sibling spend shares one budget. Exit grace is therefore unsafe in
		// a branch: once the trunk reaches a fan-out over cap, every branch
		// refuses its next node boundary.
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]any{
			"dimension": exc.dimension,
			"used":      exc.used,
			"limit":     exc.limit,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit budget_exceeded: %v", branchID, err)
			result.eventErrors++
		}
		result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
		return true
	}
	if hl := findHardLimited(checks); hl != nil {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]any{
			"dimension":  hl.dimension,
			"used":       hl.used,
			"limit":      hl.limit,
			"hard_limit": true,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit budget hard limit: %v", branchID, err)
			result.eventErrors++
		}
		result.err = fmt.Errorf("%w: hard limit %s at %.0f%% (%.0f/%.0f)", ErrBudgetExceeded, hl.dimension, (hl.used/hl.limit)*100, hl.used, hl.limit)
		return true
	}
	return false
}

// executeNodeForBranch builds the node's input (parent outputs merged with
// branch-local outputs so upstream refs still resolve; the parent rs feeds
// {{loop.*}} / {{run.*}} read-only), runs the executor, records the output,
// and validates it against the declared schema. Returns the output and a done
// flag: done=true (with result.err set) when execution or validation failed.
// On an execution error it emits node_finished with the error so the event
// log stays paired.
func (e *Engine) executeNodeForBranch(ctx context.Context, rs *runState, runID, branchID, currentNodeID string, node ir.Node, parentOutputs, parentArtifacts map[string]map[string]any, iter int, result *branchResult, slot *branchSlot) (map[string]any, bool) {
	merged := mergeOutputs(parentOutputs, result.outputs)
	mergedArt := mergeOutputs(parentArtifacts, result.artifacts)
	branchScope := resolveScope{
		vars:           rs.vars,
		outputs:        merged,
		runInputs:      rs.runInputs,
		artifacts:      mergedArt,
		rs:             rs,
		incomingByNode: result.selectedIncoming,
	}

	// Executor-less special nodes (compute / subbot / emit / wait) are
	// engine-special, not executor-backed — they share the SAME body helpers
	// the main loop uses (ADR-051 "unify branch/main special-node dispatch"),
	// resolved against this branch's merged scope. They carry no `needs:`
	// resource lease here (subbot acquires its own inside runSubbotNode) and
	// never shell out, so they bypass the resource-lease + executor path below;
	// the surrounding execBranch loop still emits their node_started/finished
	// pair and runs publishBranchArtifact. A wait parked here holds this
	// branch's semaphore slot only until the emitting branch fires the event
	// (or the mandatory timeout) — released on park, see launchBranches.
	if output, done, handled := e.executeSpecialNodeForBranch(ctx, rs, branchID, currentNodeID, node, branchScope, result, slot); handled {
		return output, done
	}

	if llm, ok := node.(ir.LLMNode); ok && llm.GetSession() == ir.SessionPersist {
		result.err = fmt.Errorf("node %q has session: persist inside a fan-out branch (C243)", currentNodeID)
		return nil, true
	}

	nodeInput := e.buildNodeInputRS(currentNodeID, branchScope)

	// Acquire this node's declared resources (`needs:`) before running. This
	// is the spot that bounds resource-heavy work INSIDE a fan-out: N branches
	// whose agent `needs: godot` are capped at the resource's capacity even
	// when max_parallel_branches is higher or unset. Released on return
	// (defer) so a failed branch node still frees its slot.
	releaseResources, leases, aerr := e.acquireResources(ctx, rs, ir.NodeNeeds(node))
	if aerr != nil {
		result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, aerr)
		return nil, true
	}
	defer releaseResources()
	if len(leases) > 0 {
		nodeInput[leaseInputKey] = leases // surface leased instance ids to the branch node
	}

	execCtx := model.WithLoopIteration(ctx, iter)
	output, err := e.executor.Execute(execCtx, node, nodeInput)
	if err != nil {
		result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
		if emitErr := e.emitBranch(ctx, runID, branchID, store.EventNodeFinished, currentNodeID, map[string]any{
			"error": err.Error(),
		}); emitErr != nil {
			e.logger.Warn("branch %s: failed to emit node_finished: %v", branchID, emitErr)
			result.eventErrors++
		}
		return nil, true
	}

	// Nested llm routers skip execLLMRouter (trunk only) and hit the
	// executor directly. Overlay the selection onto the payload the
	// router received so {{input.x}} on outgoing edges matches the
	// trunk contract.
	if rn, ok := node.(*ir.RouterNode); ok && rn.RouterMode == ir.RouterLLM {
		output = mergeRouterPassThrough(nodeInput, output)
	}

	result.outputs[currentNodeID] = output

	if err := e.validateNodeOutput(currentNodeID, node, output); err != nil {
		result.err = fmt.Errorf("node %q in branch %s: %w", currentNodeID, branchID, err)
		return nil, true
	}
	return output, false
}

// recordBranchUsage records a node's token/cost usage against the per-branch
// daily-cap ledger and the run budget, emits budget warnings, and fails the
// branch (returning true with result.err set) when a budget dimension is
// exceeded. The daily-cap key is "<runID>#<branchID>" so concurrent branches
// accumulate independently; the per-run budget pause decision stays on the
// trunk's pre-exec path, branches only contribute spend.
func (e *Engine) recordBranchUsage(ctx context.Context, rs *runState, runID, branchID, ledgerKey, currentNodeID string, output map[string]any, branchCostUSD *float64, result *branchResult) bool {
	tokens, costUSD := extractUsage(output)

	if e.dailyCap != nil && costUSD > 0 {
		*branchCostUSD += costUSD
		if _, err := e.dailyCap.Record(ctx, ledgerKey, *branchCostUSD); err != nil {
			e.logger.Warn("branch %s: daily spend cap record failed: %v", branchID, err)
		}
	}

	if rs.budget == nil {
		return false
	}
	checks := rs.budget.RecordUsage(tokens, costUSD)

	for _, w := range findWarnings(checks) {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetWarning, currentNodeID, budgetWarningData(w)); err != nil {
			e.logger.Warn("branch %s: failed to emit budget_warning: %v", branchID, err)
			result.eventErrors++
		}
	}

	if exc := findExceeded(checks); exc != nil {
		if err := e.emitBranch(ctx, runID, branchID, store.EventBudgetExceeded, currentNodeID, map[string]any{
			"dimension": exc.dimension,
			"used":      exc.used,
			"limit":     exc.limit,
		}); err != nil {
			e.logger.Warn("branch %s: failed to emit budget_exceeded: %v", branchID, err)
			result.eventErrors++
		}
		result.err = fmt.Errorf("%w: %s (%.0f/%.0f)", ErrBudgetExceeded, exc.dimension, exc.used, exc.limit)
		return true
	}
	return false
}

// publishBranchArtifact persists the node's output as a versioned artifact
// when the node declares publish, bumps the in-memory version, registers it
// under the publish name, and emits artifact_written. A write-store failure
// aborts the branch (returns true); an event-emit failure is best-effort. The
// persisted artifact records the OLD version while the in-memory map advances
// to the next.
func (e *Engine) publishBranchArtifact(ctx context.Context, runID, branchID, currentNodeID string, node ir.Node, output map[string]any, result *branchResult, branchRS *runState, parallel *parallelExecutionState) bool {
	pub := nodePublish(node)
	if pub == "" {
		return false
	}
	version := result.artifactVersions[currentNodeID]
	if parallel != nil {
		executionKey := branchID + "/" + currentNodeID + "/" + branchCounterPath(branchIterationCounters(branchRS))
		version = parallel.artifactVersion(currentNodeID, executionKey, version)
	}
	artifact := &store.Artifact{
		RunID:   runID,
		NodeID:  currentNodeID,
		Version: version,
		Data:    output,
	}
	if err := e.store.WriteArtifact(ctx, artifact); err != nil {
		result.err = fmt.Errorf("node %q in branch %s: write artifact: %w", currentNodeID, branchID, err)
		return true
	}
	result.artifactVersions[currentNodeID] = version + 1
	result.artifacts[pub] = output
	branchRS.artifacts[pub] = output
	if err := e.emitBranch(ctx, runID, branchID, store.EventArtifactWritten, currentNodeID, map[string]any{
		"publish": pub,
		"version": version,
	}); err != nil {
		e.logger.Warn("branch %s: failed to emit artifact_written: %v", branchID, err)
		result.eventErrors++
	}
	return false
}

// selectEdgeBranch is the branch-local twin of selectEdgeRS. It applies the
// same bounded loop/foreach bookkeeping to a private runState and emits the
// selection with the branch identity.
func (e *Engine) selectEdgeBranch(ctx context.Context, runID, branchID, fromNodeID string, output map[string]any, result *branchResult, rs *runState) (*ir.Edge, error) {
	selected := e.evaluateEdgesWithLoopsRS(fromNodeID, fmt.Sprintf("branch %s", branchID), output, rs)
	if selected == nil {
		return nil, fmt.Errorf("no outgoing edge from node %q in branch %s", fromNodeID, branchID)
	}

	if selected.LoopName == "" {
		for loopName, loop := range e.workflow.Loops {
			if loop == nil || len(loop.Entries) == 0 || !loop.Entries[selected.To] || loop.Body[selected.From] {
				continue
			}
			markLoopBudget(rs, loopName)
			if prior := rs.loopCounters[loopName]; prior > 0 {
				rs.loopCounters[loopName] = 0
				delete(rs.loopPreviousOutput, loopName)
				delete(rs.loopCurrentOutput, loopName)
				delete(rs.loopProgressSig, loopName)
				delete(rs.loopStaleness, loopName)
			}
		}
	}
	if selected.ForeachName != "" {
		key := foreachCounterKey(selected.ForeachName)
		rs.loopCounters[key]++
	}
	if selected.LoopName != "" {
		rs.loopCounters[selected.LoopName]++
		markLoopBudget(rs, selected.LoopName)
		if staged, ok := rs.loopCurrentOutput[selected.LoopName]; ok {
			rs.loopPreviousOutput[selected.LoopName] = staged
		}
		snap := make(map[string]any, len(output))
		for k, v := range output {
			snap[k] = deepCopyValue(v)
		}
		rs.loopCurrentOutput[selected.LoopName] = snap
	}

	data := map[string]any{
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
	if err := e.emitBranch(ctx, runID, branchID, store.EventEdgeSelected, "", data); err != nil {
		e.logger.Warn("branch %s: failed to emit edge_selected: %v", branchID, err)
	}

	if result != nil {
		recordIncoming(result.selectedIncoming, selected, true)
	}
	return selected, nil
}

// ---------------------------------------------------------------------------
// Convergence / Join
// ---------------------------------------------------------------------------
