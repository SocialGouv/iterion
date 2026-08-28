package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/SocialGouv/iterion/pkg/artifactlabels"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// execLoop is the shared execution loop used by both Run and Resume.
// It walks the graph from startNodeID until a terminal node, human pause,
// or error.
func (e *Engine) execLoop(ctx context.Context, rs *runState, startNodeID string) error {
	// Pin the loop ctx onto runState so every emit/AppendEvent uses a live
	// context. The normal launch sets rs.ctx in runInitState, but the resume
	// paths (resumeFromFailure / resumeFromPause) build rs without it — a nil
	// rs.ctx then panics on the first AppendEvent retry (backoffOrCancel →
	// nil ctx.Done()). Setting it here covers every caller.
	rs.ctx = ctx

	// Baseline the loop prices before anything runs, so a loop containing
	// the workflow entry — entered by no edge — is measured from zero.
	// Covers every caller for the same reason rs.ctx is pinned here.
	e.baselineUnpricedLoops(rs)

	currentNodeID := startNodeID

	for {
		select {
		case <-ctx.Done():
			return e.handleContextDoneWithCheckpoint(rs, currentNodeID, ctx.Err())
		default:
		}
		// Operator pause: when WithPauseSignal is wired and the channel
		// is closed, save a checkpoint and return ErrRunPausedOperator.
		// Checked AFTER ctx.Done() so cancel always wins over pause if
		// both fire concurrently (cancel is the stronger signal — it
		// also closes ctx).
		if e.pauseSignal != nil {
			select {
			case <-e.pauseSignal:
				return e.handleOperatorPauseWithCheckpoint(rs, currentNodeID)
			default:
			}
		}
		// Live-steering overrides (bump_loop / raise_budget) land at the
		// same safe boundary: this goroutine is the single writer of
		// loopCounters/loopOverrides, so applying here needs no locks.
		e.drainOverrides(rs)

		node, ok := e.workflow.Nodes[currentNodeID]
		if !ok {
			return e.failRunWithCheckpoint(rs, currentNodeID,
				fmt.Sprintf("node %q not found", currentNodeID))
		}

		// A specially-dispatched node (fan-out, round-robin, LLM router,
		// compute, review gate, subbot) is bracketed here rather than in
		// execLoopAfterExec, which it never reaches. Two distinct needs:
		// the node gets its OWN pre-marker so a rewind promoted to a
		// router has an anchor at all, and the remembered anchor is
		// invalidated afterwards because these paths DO mutate the tree —
		// fan-out branches write, a non-isolated subbot child writes into
		// the parent worktree, and the review gate squash-merges — while
		// none of them refreshes lastSnapshotCommit. Aliasing across one
		// makes a later rewind revert past that work while its outputs
		// stay in the checkpoint.
		if isSpecialDispatch(node) {
			e.markPreNodeBoundary(rs, currentNodeID)
		}
		handled, terminate, next, err := e.execLoopDispatchSpecial(ctx, rs, currentNodeID, node)
		if handled {
			if terminate {
				return err
			}
			// A specially-dispatched node (fan-out, round-robin, LLM
			// router, compute, review gate, subbot) never reaches
			// execLoopAfterExec, so no post-boundary capture ran — yet its
			// branches may have written to the workspace. Invalidating the
			// remembered boundary forces the NEXT node's pre-marker to
			// take a real capture instead of aliasing a state that predates
			// that work. A wrong anchor is worse than an extra walk: it
			// makes a later rewind delete files whose outputs it keeps.
			rs.lastSnapshotCommit = ""
			rs.lastWorkspaceSnapshot = ""
			currentNodeID = next
			continue
		}

		output, retry, err := e.execLoopRunNode(ctx, rs, currentNodeID, node)
		if errors.Is(err, errInteractionHandledInline) {
			// The interaction was auto-answered and the rest of the workflow
			// already ran inline (see errInteractionHandledInline). Stop here;
			// the run is complete (or already failed/paused via that path).
			return nil
		}
		if err != nil {
			return err
		}
		if retry {
			continue
		}

		nextNodeID, err := e.execLoopAfterExec(ctx, rs, currentNodeID, node, output)
		if err != nil {
			return err
		}
		currentNodeID = nextNodeID
	}
}

// execLoopDispatchSpecial handles terminal (Done/Fail), Human, Router,
// and Compute nodes that don't follow the standard
// emit-started → executor.Execute → emit-finished pipeline.
//
// Return tuple (handled, terminate, next, err):
//   - handled=false: caller falls through to standard execution
//     (LLM-mode human, RouterCondition, or genuinely non-special node).
//   - handled=true && terminate=true: caller returns `err` from execLoop
//     (terminal Done/Fail, pause, or fatal dispatch error).
//   - handled=true && terminate=false: caller continues the loop with
//     currentNodeID = `next` (router/compute advance, LLM-or-Human
//     auto-answered then advanced).
func (e *Engine) execLoopDispatchSpecial(ctx context.Context, rs *runState, currentNodeID string, node ir.Node) (handled, terminate bool, next string, err error) {
	switch n := node.(type) {
	case *ir.DoneNode:
		if emErr := e.emitTerminalNodeEvents(rs, currentNodeID); emErr != nil {
			return true, true, "", emErr
		}
		// Best-effort status flip — the run logically succeeded the
		// moment we reached DoneNode, so a transient store-side
		// failure on the final status write must not flip a
		// successful run to "failed" (which would also skip
		// worktree finalize and orphan any commits the run
		// produced). Log and continue; run_finished still fires
		// below so observers see the terminal event.
		if usErr := e.store.UpdateRunStatus(rs.ctx, rs.runID, store.RunStatusFinished, ""); usErr != nil && e.logger != nil {
			e.logger.Warn("runtime: failed to persist run %s as finished: %v (run reached DoneNode — treating as success)", rs.runID, usErr)
		}
		return true, true, "", e.emit(rs.ctx, rs.runID, store.EventRunFinished, "", nil)

	case *ir.FailNode:
		if emErr := e.emitTerminalNodeEvents(rs, currentNodeID); emErr != nil {
			return true, true, "", emErr
		}
		return true, true, "", e.failRunDeliberate(rs, currentNodeID, "workflow reached fail node")

	case *ir.HumanNode:
		switch n.Interaction {
		case ir.InteractionLLM:
			// LLM interaction human nodes execute via the standard
			// pipeline below (executeHumanLLM handles model + schema).
			return false, false, "", nil
		case ir.InteractionReview:
			// Guided review-&-merge gate: run the companion for the first
			// turn, then either pause for the human or (posture:
			// agent_verdict_ok) auto-merge on a favorable verdict and
			// advance to the next node.
			next, terminal, gErr := e.execReviewGate(ctx, rs, currentNodeID, n)
			if gErr != nil {
				return true, true, "", gErr
			}
			if terminal {
				return true, true, "", nil
			}
			if next != "" {
				return true, false, next, nil
			}
			return true, true, "", ErrRunPaused
		case ir.InteractionLLMOrHuman:
			paused, autoErr := e.execAutoOrPauseHuman(ctx, rs, currentNodeID, node)
			if autoErr != nil {
				return true, true, "", autoErr
			}
			if paused {
				return true, true, "", ErrRunPaused
			}
			// LLM decided no human needed — continue to edge selection.
			nextNodeID, edgeErr := e.selectEdgeRS(rs, currentNodeID, rs.outputs[currentNodeID])
			if edgeErr != nil {
				return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, edgeErr)
			}
			return true, false, nextNodeID, nil
		default:
			// InteractionHuman (default) and InteractionNone both pause.
			return true, true, "", e.pauseAtHuman(rs, currentNodeID, node)
		}

	case *ir.RouterNode:
		switch n.RouterMode {
		case ir.RouterFanOutAll:
			nextNodeID, fErr := e.execFanOut(ctx, rs, currentNodeID)
			if fErr != nil {
				if errors.Is(fErr, ErrRunPaused) {
					return true, true, "", ErrRunPaused
				}
				return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, fErr)
			}
			return true, false, nextNodeID, nil
		case ir.RouterFanOutEach:
			nextNodeID, fErr := e.execFanOutEach(ctx, rs, currentNodeID)
			if fErr != nil {
				if errors.Is(fErr, ErrRunPaused) {
					return true, true, "", ErrRunPaused
				}
				return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, fErr)
			}
			return true, false, nextNodeID, nil
		case ir.RouterRoundRobin:
			nextNodeID, rrErr := e.execRoundRobin(ctx, rs, currentNodeID)
			if rrErr != nil {
				return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, rrErr)
			}
			return true, false, nextNodeID, nil
		case ir.RouterLLM:
			nextNodeID, lErr := e.execLLMRouter(ctx, rs, currentNodeID)
			if lErr != nil {
				if errors.Is(lErr, ErrRunPaused) {
					return true, true, "", ErrRunPaused
				}
				return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, lErr)
			}
			return true, false, nextNodeID, nil
		}
		// RouterCondition falls through to standard execution.
		return false, false, "", nil

	case *ir.ComputeNode:
		nextNodeID, cErr := e.execCompute(rs, currentNodeID, n)
		if cErr != nil {
			return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, cErr)
		}
		return true, false, nextNodeID, nil

	case *ir.SubbotNode:
		nextNodeID, sErr := e.execSubbot(ctx, rs, currentNodeID, n)
		if sErr != nil {
			return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, sErr)
		}
		return true, false, nextNodeID, nil

	case *ir.EmitNode:
		nextNodeID, emErr := e.execEmit(rs, currentNodeID, n)
		if emErr != nil {
			return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, emErr)
		}
		return true, false, nextNodeID, nil

	case *ir.WaitNode:
		nextNodeID, wErr := e.execWait(ctx, rs, currentNodeID, n)
		if wErr != nil {
			return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, wErr)
		}
		return true, false, nextNodeID, nil

	case *ir.AwaitAnswersNode:
		nextNodeID, aErr := e.execAwaitAnswers(ctx, rs, currentNodeID, n)
		if aErr != nil {
			return true, true, "", e.failRunErrWithCheckpoint(rs, currentNodeID, aErr)
		}
		return true, false, nextNodeID, nil
	}

	return false, false, "", nil
}

// execLoopRunNode runs the standard non-special node pipeline: emit
// node_started, check budget, build node input (with fork-rehydration
// when applicable), invoke executor.Execute under a per-node span, and
// route any error through ErrNeedsInteraction / handleNodeFailure. On
// retry=true the caller `continue`s the loop without advancing; on
// retry=false + err==nil the output is returned for downstream
// persistence in execLoopAfterExec.
func (e *Engine) execLoopRunNode(ctx context.Context, rs *runState, currentNodeID string, node ir.Node) (map[string]any, bool, error) {
	// Compute the loop iteration once so the event payload and the
	// executor's Task.Iteration agree. The frontend uses
	// data.iteration as the source of truth for the pip-strip UI,
	// but the reducer keys exec_id on data.iteration_path because a
	// single int collapses nested-loop executions onto the same id
	// (observed live: solo body nodes were stuck on the family_loop
	// counter so every package's validate_upgrade collided on
	// iter=5 → canvas showed nothing as running across 5+ pkgs).
	iter := e.currentLoopIteration(currentNodeID, rs.loopCounters)
	iterPath := e.currentLoopIterationPath(currentNodeID, rs.loopCounters)
	payload := map[string]any{
		"kind":      node.NodeKind().String(),
		"iteration": iter,
	}
	if iterPath != "" {
		payload["iteration_path"] = iterPath
	}
	// Bracket the node: record the workspace it starts from, so a rewind
	// can restore its prior conditions (files included), not just its
	// declared output.
	e.markPreNodeBoundary(rs, currentNodeID)
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, currentNodeID, payload); err != nil {
		return nil, false, err
	}

	if err := e.checkBudgetBeforeExec(rs, currentNodeID); err != nil {
		return nil, false, err
	}

	nodeInput := e.buildNodeInputRS(currentNodeID, rs.scope())

	// Acquire any resources this node declares (`needs:`) — blocks until a
	// slot is free. Placed before the per-node wall-clock deadline below so
	// waiting on a busy resource doesn't eat the node's execution budget.
	// Released on return (defer) so a failed node still frees its slot.
	releaseResources, leases, aerr := e.acquireResources(ctx, rs, ir.NodeNeeds(node))
	if aerr != nil {
		return nil, false, aerr
	}
	defer releaseResources()
	if rem, bounded := rs.budget.RemainingDuration(); bounded && rem <= 0 {
		// Same grace as every other axis: checkBudgetBeforeExec above
		// already graced (or failed) a pre-existing duration overrun,
		// and this re-check only exists for time spent WAITING on a
		// busy resource slot. Killing here unconditionally made the
		// grace inert on the one axis a long campaign trips most — the
		// run emitted budget_exit_grace and then died on the very next
		// statement. graceOrFailBudget dedupes the event.
		used, limit, _ := rs.budget.DurationStatus()
		if err := e.graceOrFailBudget(rs, currentNodeID, &budgetCheckResult{
			exceeded: true, dimension: "duration", used: used, limit: limit,
		}); err != nil {
			return nil, false, err
		}
	}
	if len(leases) > 0 {
		nodeInput[leaseInputKey] = leases // surface leased instance ids to the node
	}

	// Fork rehydration: when resumeFromFailure pinned a backend
	// conversation / session id at currentNodeID, inject the matching
	// keys into the input map for THIS first execution only. Cleared
	// after injection so a loop iteration of the same node doesn't keep
	// replaying the parent's conversation. session_id flows via the
	// same key SessionInherit nodes consume, so an inherit-mode forked
	// node picks it up transparently; independent-mode nodes ignore.
	e.injectPersistAndResume(ctx, rs, node, nodeInput)

	// Fork / resumeFromFailure rehydration overlays the checkpoint's
	// conversation and session id AFTER persist inject so a stripped
	// inbound id cannot clobber the restart node's pinned session.
	if rs.resumeBackend.nodeID == currentNodeID {
		if len(rs.resumeBackend.conversation) > 0 {
			nodeInput[delegate.ResumeConversationKey] = rs.resumeBackend.conversation
		}
		if rs.resumeBackend.sessionID != "" {
			nodeInput[delegate.SessionIDKey] = rs.resumeBackend.sessionID
		}
		rs.resumeBackend = resumeBackendState{}
	}

	// Thread the run ID into ctx so the executor can locate per-node
	// session state (used by Compactor implementations to find the
	// right messages list to compact + retry). Also attach a
	// template-data snapshot so the executor can resolve `outputs.*`,
	// `loop.*`, `artifacts.*`, and `run.*` refs in prompt bodies.
	execCtx := model.WithRunID(ctx, rs.runID)
	execCtx = model.WithNodeID(execCtx, currentNodeID)
	execCtx = model.WithTemplateData(execCtx, e.buildTemplateData(rs))
	// Per-node span: inherits the runner-side or server-side root
	// span via ctx (W3C trace propagated through NATS in cloud mode).
	spanCtx, span := otel.Tracer(tracerName).Start(execCtx, "iterion.node.execute",
		trace.WithAttributes(
			attribute.String("iterion.run_id", rs.runID),
			attribute.String("iterion.node_id", currentNodeID),
			attribute.String("iterion.node_kind", node.NodeKind().String()),
		),
	)
	spanCtx = model.WithLoopIteration(spanCtx, iter)

	// Hard wall-clock deadline: bound this node's execution to the run's
	// remaining max_duration budget. checkBudgetBeforeExec only blocks NEW
	// node starts at 90%; without a deadline a single long or hung node — a
	// stuck delegate subprocess, an over-eager survey, a runaway scanner —
	// runs unbounded past max_duration (observed killing whole dogfood runs:
	// a deepsec scanner ran 81m on a 90m budget, a survey node ran 100m on a
	// 50m budget, a claude_code stream stalled 43m after a timeout). The
	// deadline propagates into claude_code's exec.CommandContext and claw's
	// streaming ctx, force-terminating the node; expiry is surfaced below as
	// a resumable BUDGET_EXCEEDED(duration) failure. Recompute after resource
	// acquisition so time spent waiting for a busy slot cannot exhaust the
	// run's max_duration and then start execution with no deadline.
	if rem, bounded := rs.budget.RemainingDuration(); bounded {
		if rem <= 0 {
			// Inside the duration grace (the gate above let this node
			// through): bound it by the GRACED ceiling instead of
			// running deadline-less — an unbounded stall is exactly
			// what the deadline exists to terminate, grace or not.
			rem, _ = rs.budget.GracedRemainingDuration(budgetExitGraceRatio())
			if rem <= 0 {
				// The sliver of graced room the gate saw was consumed
				// between there and here (span start, template build):
				// fail on the budget rather than run with NO deadline.
				used, limit, _ := rs.budget.DurationStatus()
				span.End()
				return nil, false, e.failBudgetExceeded(rs, currentNodeID, &budgetCheckResult{
					exceeded: true, dimension: "duration", used: used, limit: limit,
				})
			}
		}
		var cancel context.CancelFunc
		spanCtx, cancel = context.WithDeadline(spanCtx, time.Now().Add(rem))
		defer cancel()
	}

	output, execErr := e.executor.Execute(spanCtx, node, nodeInput)
	if execErr != nil {
		span.RecordError(execErr)
		span.SetStatus(codes.Error, execErr.Error())
	}
	span.End()
	if execErr != nil {
		// If the RUN's own context is done, the run is being torn down —
		// cancelled by a drain/operator/heartbeat, or past its wall-clock
		// deadline — WHILE this node was executing. Route through the
		// cause-aware handler, not recovery/retry (a cancelled run must not
		// retry) and not a generic failRunWithCheckpoint (which stringifies
		// the error, losing the ErrRunInterrupted sentinel — so a drain would
		// surface as a spurious "run failed" notification instead of a silent
		// auto-resume, and an operator cancel would wrongly land
		// failed_resumable and get redelivered-resumed). This mirrors the
		// top-of-loop select for the far more common MID-node interruption
		// (a deploy almost always drains a run inside an LLM call, not in the
		// gap between nodes). Gated on the RUN ctx being done, so a node's
		// own internal Canceled error with a live run ctx still takes the
		// normal recovery path below.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, e.handleContextDoneWithCheckpoint(rs, currentNodeID, ctxErr)
		}
		// Check if the delegate needs user interaction.
		var needsInput *model.ErrNeedsInteraction
		if errors.As(execErr, &needsInput) {
			ierr := e.handleNeedsInteraction(ctx, rs, currentNodeID, node, needsInput, 0)
			if ierr == nil {
				// interaction: llm / llm_or_human auto-answered and
				// reInvokeBackend already drove the rest of the workflow to
				// completion (its own execLoop ran node post-processing and
				// every downstream node). Signal execLoop to stop instead of
				// re-running this node via execLoopAfterExec with a nil output.
				ierr = errInteractionHandledInline
			}
			return nil, false, ierr
		}
		// Per-node duration deadline expiry: when the node was cut off by
		// the wall-clock deadline derived from max_duration, surface it as a
		// resumable BUDGET_EXCEEDED(duration) failure rather than routing
		// through retry/recovery — retrying a node that already exhausted the
		// run's duration budget would just hang or burn the budget again. The
		// budgetHardThreshold guard ensures an unrelated DeadlineExceeded that
		// originated inside the node (some shorter internal timeout) is NOT
		// misclassified as a budget stop.
		if errors.Is(execErr, context.DeadlineExceeded) {
			if used, limit, bounded := rs.budget.DurationStatus(); bounded && limit > 0 && used >= limit*budgetHardThreshold {
				return nil, false, e.failBudgetExceeded(rs, currentNodeID, &budgetCheckResult{
					exceeded: true, dimension: "duration", used: used, limit: limit,
				})
			}
		}
		// Recovery dispatch (when wired via WithRecoveryDispatch):
		// classify the error, look up a recipe, and either retry,
		// pause, or fail terminally. Without a dispatcher, every
		// failure produces failed_resumable as before. The run-ID-
		// enriched ctx is passed so Compact() can locate the per-
		// node session.
		retry, code, recoveryErr := e.handleNodeFailure(execCtx, rs, currentNodeID, execErr)
		if recoveryErr != nil {
			return nil, false, recoveryErr
		}
		if retry {
			return nil, true, nil
		}
		// Fail terminally carrying BOTH the classified code and the
		// original error as Cause: a host deciding how to recover needs
		// the typed cause (e.g. *delegate.ErrRateLimited and its
		// ResetAt), and flattening it into the message loses that for
		// good. The Message text stays byte-identical — the DLQ reason,
		// the run doc's error field and operator greps all key on it.
		if code == "" {
			code = ErrCodeExecutionFailed
		}
		return nil, false, e.failRunErrWithCheckpoint(rs, currentNodeID, &RuntimeError{
			Code:    code,
			NodeID:  currentNodeID,
			Cause:   execErr,
			Message: fmt.Sprintf("node %q execution failed: %v", currentNodeID, execErr),
		})
	}

	// Reset per-node retry counters on success so a future failure
	// starts fresh.
	delete(rs.nodeAttempts, currentNodeID)
	return output, false, nil
}

// emitVerifiedActionIfPresent surfaces a Verified Action node's escalation
// outcome (ADR-044) as a node_verified_action event, then removes the
// private `_verified_action` key from the output map in place so it does
// not leak into schema validation, downstream {{outputs.*}} refs, or the
// persisted artifact. No-op for nodes that did not escalate.
func (e *Engine) emitVerifiedActionIfPresent(rs *runState, nodeID string, output map[string]any) {
	if output == nil {
		return
	}
	meta, ok := output["_verified_action"].(map[string]any)
	// Strip unconditionally (no-op when absent) so the private control key
	// never reaches schema validation, downstream refs, or the store; bail
	// when it wasn't the expected map shape (only verifiedOutput writes it).
	delete(output, "_verified_action")
	if !ok {
		return
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeVerifiedAction, nodeID, meta); err != nil {
		// Best-effort observability — the run proceeds even if the event
		// store is down; log so the gap is debuggable.
		e.logger.Warn("verified-action: failed to emit event for node %q: %v", nodeID, err)
	}
}

// persistArtifactIfPublished writes the node's `publish:` artifact when it
// declares one: it versions the artifact, exposes the output under
// rs.artifacts[name] for downstream {{artifacts.name}} refs, and emits
// EventArtifactWritten. No-op for nodes without publish. Shared by every
// node-completion path so the behaviour stays identical — including
// compute, whose bespoke execCompute path calls this directly.
func (e *Engine) persistArtifactIfPublished(ctx context.Context, rs *runState, nodeID string, node ir.Node, output map[string]any) error {
	pub := nodePublish(node)
	if pub == "" {
		return nil
	}
	version := rs.artifactVersions[nodeID]
	// Labels categorise the artifact for the studio's grouped view. Union
	// the node's DSL artifact_labels with shape-derived labels (plan/
	// verdict), deduped — so explicit and heuristic labels coexist.
	labels := dedupeLabels(append(nodePublishLabels(node), artifactlabels.Classify(output)...))
	if err := e.store.WriteArtifact(ctx, &store.Artifact{
		RunID:   rs.runID,
		NodeID:  nodeID,
		Version: version,
		Data:    output,
		Labels:  labels,
	}); err != nil {
		return fmt.Errorf("runtime: write artifact: %w", err)
	}
	rs.artifactVersions[nodeID] = version + 1
	rs.artifacts[pub] = output

	evtData := map[string]any{
		"publish": pub,
		"version": version,
	}
	if len(labels) > 0 {
		evtData["labels"] = labels
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventArtifactWritten, nodeID, evtData); err != nil {
		return fmt.Errorf("runtime: artifact written but event emission failed (state inconsistency): %w", err)
	}
	return nil
}

// execLoopAfterExec runs the post-execution pipeline for a node:
// stores output in runState, validates against the declared schema,
// records budget usage, persists any `publish:` artifact, emits
// node_finished + onNodeFinished hook, saves a checkpoint (best-
// effort), snapshots the worktree at the node boundary, and selects
// the outgoing edge. Returns the next node ID.
func (e *Engine) execLoopAfterExec(ctx context.Context, rs *runState, currentNodeID string, node ir.Node, output map[string]any) (string, error) {
	// Verified Action (ADR-044): a tool node that escalated through the
	// recovery ladder stamps a private `_verified_action` key. Emit the
	// node_verified_action event for observability, then strip the key so
	// it never reaches schema validation, downstream refs, or the store.
	e.emitVerifiedActionIfPresent(rs, currentNodeID, output)

	if err := e.commitPersistSlot(ctx, rs, node, output); err != nil {
		return "", err
	}

	rs.outputs[currentNodeID] = output

	// Validate output against declared schema (optional).
	if err := e.validateNodeOutput(currentNodeID, node, output); err != nil {
		return "", e.failRunErrWithCheckpoint(rs, currentNodeID, err)
	}

	// Record budget usage and check limits.
	if err := e.recordAndDeferBudget(rs, currentNodeID, output); err != nil {
		return "", err
	}

	// Persist artifact if node has publish.
	if err := e.persistArtifactIfPublished(ctx, rs, currentNodeID, node, output); err != nil {
		return "", err
	}

	// Emit node_finished with usage data.
	nodeFinishedData := buildNodeFinishedData(e.sanitizeOutputForEvent(node, output))
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, currentNodeID, nodeFinishedData); err != nil {
		return "", err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(rs.runID, currentNodeID, output)
	}

	// Best-effort checkpoint for resume-from-failed.
	if err := e.store.SaveCheckpoint(rs.ctx, rs.runID, buildCheckpoint(rs, currentNodeID)); err != nil {
		e.logger.Error("failed to save checkpoint after node %q: %v", currentNodeID, err)
	}

	// Phase 2: snapshot the worktree at this node boundary so the
	// Fork API's rewind_code=true mode has an anchor to git reset
	// back to. Best-effort — a failure logs at warn and continues
	// (the rest of the run is unaffected, the only loss is the
	// fork-rewind capability for THIS node).
	e.snapshotAtNodeBoundary(rs, currentNodeID)

	nextNodeID, edgeErr := e.selectEdgeRS(rs, currentNodeID, output)

	// A hard overrun measured after THIS node ends the run here, one edge
	// later than the measurement — anchored on the node that has NOT run,
	// so a resume with a raised cap continues from there instead of
	// re-executing the node whose output is already stored.
	//
	// Taken BEFORE any edge error is surfaced: a node that blew the cap
	// and then found no matching edge must still die as BUDGET_EXCEEDED.
	// That error is what the cloud runner's terminal-ack carve-out
	// matches; a naked NO_OUTGOING_EDGE goes back to JetStream and is
	// redelivered onto the same spent budget. With no successor the run
	// has nowhere to go, so it stops where it stands.
	if rs.budget != nil {
		if exc := rs.budget.takeExceeded(); exc != nil {
			anchor := nextNodeID
			if edgeErr != nil || anchor == "" {
				anchor = currentNodeID
			}
			// An edge error leaves no successor to walk to: the grace
			// has nothing to deliver, and letting it swallow the
			// overrun would surface the edge error naked below — a
			// NO_OUTGOING_EDGE carries neither ErrBudgetExceeded nor
			// its code, so the cloud runner naks the message back onto
			// the same spent budget instead of acking it terminal.
			if edgeErr != nil {
				return "", e.failBudgetExceeded(rs, anchor, exc)
			}
			if err := e.graceOrFailBudget(rs, anchor, exc); err != nil {
				return "", err
			}
		}
	}
	if edgeErr != nil {
		return "", e.failRunErrWithCheckpoint(rs, currentNodeID, edgeErr)
	}
	return nextNodeID, nil
}

// snapshotAtNodeBoundary records a per-node git snapshot when the run
// is using a worktree. No-op outside worktree mode (the snapshot ref
// machinery is only meaningful when there's a dedicated worktree to
// reset later). The ref name is deterministic from
// (runID, nodeID, loopIter) so the Fork API can locate it later
// without consulting the engine.
func (e *Engine) snapshotAtNodeBoundary(rs *runState, nodeID string) {
	if e.workDir == "" {
		return
	}
	if !rs.isWorktree {
		// No isolated worktree: git is not merely unavailable here, it is
		// the wrong tool — the workspace is the operator's live checkout
		// and `git add -A` would stage their own work. iterion's own
		// versioning covers this shape.
		e.captureWorkspace(rs, nodeID, workspacetrack.PhasePost)
		return
	}
	loopIter := e.currentLoopIteration(nodeID, rs.loopCounters)
	ref := nodeSnapshotRef(rs.runID, nodeID, loopIter)
	commit, err := snapshotWorktree(e.workDir, ref)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("snapshot: node %q iter %d: %v", nodeID, loopIter, err)
		}
		return
	}
	if commit == "" {
		// The node left the tree clean — which is what a well-behaved bot
		// that commits in stride does. snapshotWorktree writes no ref in
		// that case, so without this the node would have a `pre` boundary
		// and no `post`, and `pre..post` — the range a reviewer sees —
		// would not resolve for exactly the nodes that behaved best. The
		// review panel enumerates only this namespace, so every file such
		// a node wrote would land in the *Other changes* catch-all and the
		// per-node grouping would collapse to one anonymous bucket.
		if out, uerr := runGit(e.workDir, "update-ref", ref, "HEAD"); uerr != nil && e.logger != nil {
			e.logger.Warn("snapshot: node %q iter %d: anchor clean tree at HEAD: %v\noutput: %s", nodeID, loopIter, uerr, out)
		}
	}
	// Remember the workspace as it stands now so the NEXT node's
	// pre-boundary marker is a free alias of this commit. An empty SHA
	// means the tree matched HEAD, so "" is the correct sentinel for
	// "current state is HEAD" — HEAD may itself have moved if this node
	// committed.
	rs.lastSnapshotCommit = commit
}

// markPreNodeBoundary records the workspace a node is ABOUT to execute
// against, under NodePreSnapshotRef. This is the anchor an in-place
// rewind reverts to: NodeSnapshotRef is written after the node ran and
// therefore holds that node's own production — a bot that writes docs or
// code would be "rewound" onto the very files the rewind means to
// discard.
//
// Nothing modifies the tree between the previous node's post-snapshot and
// this call, so the state is already captured by rs.lastSnapshotCommit:
// this is one `update-ref`, not a second O(filecount) index walk per node.
func (e *Engine) markPreNodeBoundary(rs *runState, nodeID string) {
	if e.workDir == "" {
		return
	}
	if !rs.isWorktree {
		e.aliasWorkspacePre(rs, nodeID)
		return
	}
	loopIter := e.currentLoopIteration(nodeID, rs.loopCounters)
	// Several dispatch paths bracket the same node: execLoop brackets every
	// isSpecialDispatch kind, then execSpecialNode brackets the compute /
	// subbot / emit / wait / await_answers kinds again, and the human path
	// does the same. The repeat lands on the same tree, so it is pure
	// duplicated work — but with lastSnapshotCommit UNKNOWN (its state
	// after every special dispatch) each call pays a full `git add -A`
	// index walk and leaves an orphan commit.
	if rs.preMarked == nil {
		rs.preMarked = make(map[string]bool)
	}
	key := fmt.Sprintf("%s\x00%d", nodeID, loopIter)
	if rs.preMarked[key] {
		return
	}
	rs.preMarked[key] = true
	ref := store.NodePreSnapshotRef(rs.runID, nodeID, loopIter)
	target := rs.lastSnapshotCommit
	if target == "" {
		// "" means UNKNOWN, not "the tree equals HEAD" — the run just
		// started, just resumed, or just came out of a special dispatch
		// that mutated the tree. Only the first of those implies HEAD; in
		// the other two the worktree carries uncommitted work HEAD does
		// not, because the engine never commits the tree (snapshotWorktree
		// only writes a ref). Aliasing HEAD there makes a later rewind
		// `read-tree --reset` + `clean -fd` back to HEAD, deleting
		// everything every fan-out branch and every earlier node produced
		// — strictly worse than the stale alias this replaced, which would
		// at least have restored the pre-fan-out tree.
		//
		// So capture the real state. snapshotWorktree returns "" and
		// writes no ref only when the tree genuinely matches HEAD, which
		// is the one case where aliasing is right.
		commit, err := snapshotWorktree(e.workDir, ref)
		if err != nil {
			if e.logger != nil {
				e.logger.Warn("pre-snapshot: node %q iter %d: %v", nodeID, loopIter, err)
			}
			return
		}
		if commit != "" {
			return // snapshotWorktree already pointed the ref at it
		}
		target = "HEAD"
	}
	if out, err := runGit(e.workDir, "update-ref", ref, target); err != nil && e.logger != nil {
		e.logger.Warn("pre-snapshot: node %q iter %d: %v\noutput: %s", nodeID, loopIter, err, out)
	}
}

// ---------------------------------------------------------------------------
// Compute nodes
// ---------------------------------------------------------------------------

// execCompute evaluates a ComputeNode's expressions deterministically and
// stores the result as the node's output, persisting its published artifact
// (post-validation) if declared. The shared lifecycle envelope (node_started →
// body → validate → node_finished → checkpoint → edge select) lives in
// execSpecialNode; only the expr-eval body and the publish hook are
// compute-specific.
func (e *Engine) execCompute(rs *runState, nodeID string, cn *ir.ComputeNode) (string, error) {
	return e.execSpecialNode(rs, nodeID, "compute", cn, nil,
		func() (map[string]any, error) { return e.computeOutput(rs, nodeID, cn, rs.scope()) },
		func(output map[string]any) error {
			// Publish is compute-specific (only ComputeNode has a Publish
			// field) and gated on validation: persist only known-valid output.
			return e.persistArtifactIfPublished(rs.ctx, rs, nodeID, cn, output)
		},
	)
}

// execSubbot runs a SubbotNode: it resolves the `with:` mappings into the
// child's input vars, acquires any `needs:` resource leases (passing the leased
// instance id to the child as `_lease_<resource>`), invokes the host-supplied
// SubbotRunner, and maps the child's terminal output to outputs.<subbot>.<field>.
// The child is a full nested run, so it may contain loops (unlike a fan-out
// branch). The run body lives in runSubbotNode (shared with the branch path);
// the lifecycle envelope is execSpecialNode.
func (e *Engine) execSubbot(ctx context.Context, rs *runState, nodeID string, sn *ir.SubbotNode) (string, error) {
	return e.execSpecialNode(rs, nodeID, "subbot", sn,
		map[string]any{"source": sn.Source},
		func() (map[string]any, error) { return e.runSubbotNode(ctx, rs, nodeID, sn, rs.scope(), "") },
		nil,
	)
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
func (e *Engine) selectEdgeRS(rs *runState, fromNodeID string, output map[string]any) (string, error) {
	selected := e.evaluateEdgesWithLoopsRS(fromNodeID, "main", output, rs)
	if selected == nil {
		return "", &RuntimeError{
			Code:    ErrCodeNoOutgoingEdge,
			Message: fmt.Sprintf("no outgoing edge from node %q", fromNodeID),
			NodeID:  fromNodeID,
			Hint:    "ensure the node's output matches at least one edge condition, or add an unconditional fallback edge",
		}
	}

	// Reset loop counters when we re-enter the loop at its TOP — i.e.
	// when a non-loop edge targets one of the loop's entry nodes
	// (target of a loop-bearing back-edge) from a source that isn't
	// part of the loop body. That signals a fresh outer iteration
	// driving a fresh loop instance (e.g. package_loop pushes into
	// Phase 1, which lands on validate_upgrade — the fix_loop entry —
	// from outside the body via align_code).
	//
	// Earlier this fired on ANY body-node target, which over-reset
	// when computeLoopBodies couldn't intersect forward+reverse BFS
	// past intermediate loop edges and yielded a minimal-endpoints
	// body. Concrete case: recovery_loop's body was just {alt_review,
	// review_commit_auto} because the cycle goes through review_loop's
	// back-edge; the non-loop edge fix_X → review_commit_auto then
	// reset the counter every cycle and review_commit_auto's
	// iteration_path stuck at recovery_loop=0, collapsing every
	// invocation into one snapshot row. Scoping the reset to the
	// loop's entries fixes the false positive while still resetting
	// when a parent iteration legitimately re-enters.
	if selected.LoopName == "" {
		for loopName, loop := range e.workflow.Loops {
			if loop == nil || len(loop.Entries) == 0 {
				continue
			}
			if !loop.Entries[selected.To] || loop.Body[selected.From] {
				continue
			}
			// Entering the body from outside re-bases the loop's price:
			// its first back-edge crossing must cost one iteration, not
			// everything the run spent before this loop existed. Outside
			// the re-entry branch below — a FIRST entry needs the
			// baseline just as much, and leaves the counter at 0.
			markLoopBudget(rs, loopName)
			if prior, ok := rs.loopCounters[loopName]; ok && prior > 0 {
				e.logger.Debug("loop %q: re-entered via edge %s→%s — counter reset from %d", loopName, selected.From, selected.To, prior)
				rs.loopCounters[loopName] = 0
				if rs.loopPreviousOutput != nil {
					delete(rs.loopPreviousOutput, loopName)
					delete(rs.loopCurrentOutput, loopName)
				}
				delete(rs.loopProgressSig, loopName)
				delete(rs.loopStaleness, loopName)
			}
		}
	}

	if selected.ForeachName != "" {
		// Advancing to the next element: bump the foreach index (shares the
		// loopCounters map under the foreach name; distinct namespaces).
		k := foreachCounterKey(selected.ForeachName)
		rs.loopCounters[k] = rs.loopCounters[k] + 1
	}

	if selected.LoopName != "" {
		rs.loopCounters[selected.LoopName] = rs.loopCounters[selected.LoopName] + 1
		// The crossing is committed: price the iteration that starts here
		// from this point. Done on the SELECTED edge only, so evaluating a
		// sibling back-edge cannot move the mark or leave it a zero price.
		markLoopBudget(rs, selected.LoopName)
		// Rotate snapshots so {{loop.<name>.previous_output}} reads the
		// snapshot from the PRIOR traversal (one iteration behind), not the
		// current one. The current iteration's source output is staged in
		// loopCurrentOutput and only promoted to loopPreviousOutput at the
		// NEXT loop-edge crossing for the same loop name.
		if rs.loopPreviousOutput != nil {
			if staged, ok := rs.loopCurrentOutput[selected.LoopName]; ok {
				rs.loopPreviousOutput[selected.LoopName] = staged
			}
			snap := make(map[string]any, len(output))
			for k, v := range output {
				snap[k] = v
			}
			rs.loopCurrentOutput[selected.LoopName] = snap
		}
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
	if err := e.emit(rs.ctx, rs.runID, store.EventEdgeSelected, "", data); err != nil {
		e.logger.Warn("failed to emit edge_selected: %v", err)
	}

	rs.setIncoming(selected)
	return selected.To, nil
}

// captureWorkspace records the workspace through iterion's own tracker.
// Best-effort, like its git twin: a capture failure costs the ability to
// rewind that boundary's files, never the run.
func (e *Engine) captureWorkspace(rs *runState, nodeID, phase string) {
	if e.workspaceTracker == nil {
		return
	}
	loopIter := e.currentLoopIteration(nodeID, rs.loopCounters)
	snap, err := e.workspaceTracker.Capture(rs.runID, e.workDir, workspacetrack.Label(phase, nodeID, loopIter))
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("workspace capture: node %q iter %d: %v", nodeID, loopIter, err)
		}
		// Invalidate the remembered anchor. It now predates whatever this
		// node wrote, so leaving it would have the NEXT node's pre-marker
		// alias a state from before this node ran — and a rewind there
		// would delete this node's files while the checkpoint still keeps
		// its outputs. Same failure the special-dispatch paths already
		// close by clearing it; this was the remaining hole, and capture
		// failures are not hypothetical (an unreadable file, a MaxFiles
		// overflow). An extra walk costs time; a wrong anchor costs data.
		rs.lastWorkspaceSnapshot = ""
		return
	}
	rs.lastWorkspaceSnapshot = snap.ID
	if len(snap.Skipped) > 0 && e.logger != nil {
		e.logger.Warn("workspace capture at %q: %d path(s) too large to version — a rewind cannot restore them",
			nodeID, len(snap.Skipped))
	}
}

// captureFailureBoundary records the workspace a node left behind when
// its execution did NOT complete.
//
// snapshotAtNodeBoundary only runs on the success path, and
// aliasWorkspacePre writes the pre-marker as an Alias — which does not
// advance the chain head. So a run that stops INSIDE a node ends with its
// most recent recorded boundary being the state that node started from,
// and everything the node wrote before dying is on disk with nothing
// recording that the run put it there.
//
// That is the common shape, not an edge case: the whole point of a rewind
// is usually "this node misbehaved, back up and replay it". Without this
// capture a scoped restore cannot distinguish the failed node's debris
// from the operator's own files, so it must leave both — and the replayed
// node builds on top of its own production, the one failure workspace
// versioning exists to prevent.
//
// Best-effort, exactly like its success-path twin: the run has already
// failed, and losing a boundary costs rewind fidelity, never the run.
func (e *Engine) captureFailureBoundary(rs *runState, nodeID string) {
	e.captureStopBoundary(rs, nodeID, workspacetrack.PhaseFail)
}

// capturePauseBoundary is the same thing for a node that PARKED rather
// than died — a human gate, an ask_user question, a recovery pause.
//
// `paused_waiting_human` is a rewindable status, so the gap is identical:
// an agent that writes ten files and then asks a question has left them
// on disk with nothing recording that the run put them there. It also
// opens the pause INTERVAL, which is the one window a scoped restore can
// prove is not the run's — nothing of the run executes inside it.
func (e *Engine) capturePauseBoundary(rs *runState, nodeID string) {
	e.captureStopBoundary(rs, nodeID, workspacetrack.PhasePause)
}

// captureStopBoundary records the workspace when a node's execution ends
// WITHOUT completing.
//
// Called inline, before the durable status write, never deferred: once
// the run is persisted in a rewindable status an operator's `iterion
// rewind` may claim it and start its own Capture on the same run, and
// two concurrent captures of one run is not a contract this tracker
// offers. The walk itself is bounded work on a local filesystem, and it
// no-ops outright wherever versioning is off — cloud runners included,
// which is where a termination grace period would otherwise argue for
// the opposite order.
func (e *Engine) captureStopBoundary(rs *runState, nodeID, phase string) {
	if e.workDir == "" || rs == nil || nodeID == "" || e.workspaceTracker == nil {
		return
	}
	if rs.isWorktree {
		// git owns that shape; snapshotAtNodeBoundary's refs are its
		// boundaries and the tracker is not consulted.
		return
	}
	label := workspacetrack.Label(phase, nodeID, e.currentLoopIteration(nodeID, rs.loopCounters))
	if rs.stopCaptured == nil {
		rs.stopCaptured = make(map[string]bool)
	}
	if rs.stopCaptured[label] {
		return
	}
	rs.stopCaptured[label] = true
	e.captureWorkspace(rs, nodeID, phase)
}

// aliasWorkspacePre labels the state a node is about to execute against.
// Nothing touches the workspace between the previous node's capture and
// this call, so it is a label write rather than a second walk — the same
// trick the git path uses, and what keeps versioning affordable enough to
// leave on by default.
func (e *Engine) aliasWorkspacePre(rs *runState, nodeID string) {
	if e.workspaceTracker == nil {
		return
	}
	loopIter := e.currentLoopIteration(nodeID, rs.loopCounters)
	label := workspacetrack.Label(workspacetrack.PhasePre, nodeID, loopIter)
	head := rs.lastWorkspaceSnapshot
	_, hasPre := e.workspaceTracker.Resolve(rs.runID, label)
	if rs.resumed {
		// The run is picking back up, and THIS is the boundary that
		// closes the interval it was stopped in — the only interval a
		// scoped rewind can prove is not the run's own work. Keyed on the
		// explicit resume flag, never on `head == ""`: two mid-run paths
		// clear that too, and with a colliding loop-iteration label
		// either would mint a spurious `resume:` and launder a fan-out's
		// real production out of the scope.
		rs.resumed = false
		e.captureWorkspace(rs, nodeID, workspacetrack.PhaseResume)
		if !hasPre && rs.lastWorkspaceSnapshot != "" {
			// The run stopped BEFORE this node was bracketed — an
			// operator pause is checked at the top of the iteration, so
			// it lands between two nodes. Point the node's pre-boundary
			// at the state it is actually about to execute against;
			// without it the node has none at all and a rewind to it has
			// nothing to restore to.
			if err := e.workspaceTracker.Alias(rs.runID, label, rs.lastWorkspaceSnapshot); err != nil && e.logger != nil {
				e.logger.Warn("workspace pre-marker after resume: node %q iter %d: %v", nodeID, loopIter, err)
			}
		}
		return
	}
	if hasPre {
		// A pre-execution boundary for this (node, iteration) already
		// exists. NEVER overwrite it — that is the state a rewind to this
		// node restores, and by now the workspace may have moved, so
		// re-pointing the label would silently redefine "what this node
		// started from" as "whatever is on disk now".
		return
	}
	if head == "" {
		// First node of a fresh run: capture, since there is no earlier
		// boundary to point at.
		e.captureWorkspace(rs, nodeID, workspacetrack.PhasePre)
		return
	}
	if err := e.workspaceTracker.Alias(rs.runID, label, head); err != nil && e.logger != nil {
		e.logger.Warn("workspace pre-marker: node %q iter %d: %v", nodeID, loopIter, err)
	}
}

// isSpecialDispatch reports whether a node is handled by
// execLoopDispatchSpecial rather than the standard executor path.
//
// Those nodes never reach execLoopAfterExec, so nothing else brackets
// them. Routers matter most: a rewind promotes any pivot inside a fan-out
// body to its router, so without a marker here the single case where the
// most files were written by the most nodes is the one that can never
// restore them.
func isSpecialDispatch(node ir.Node) bool {
	switch n := node.(type) {
	case *ir.RouterNode:
		return n.RouterMode != ir.RouterCondition
	case *ir.HumanNode:
		return true
	case *ir.ComputeNode, *ir.SubbotNode, *ir.EmitNode, *ir.WaitNode, *ir.AwaitAnswersNode:
		return true
	}
	return false
}
