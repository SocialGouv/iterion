package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

// review.go — the guided review-&-merge gate (interaction: review).
//
// A review gate walks a human through testing a change via a continuous
// companion↔human dialogue, then squash-merges the run's worktree DURING
// the pause. The companion is an LLM that produces precise test
// instructions, reads the human's replies, and emits a verdict. Three exits:
//
//   1. approve_merge   — human approves → squash-merge → advance (decision=approved)
//   2. force_merge     — human merges without finishing the dialogue (skips the
//                        verdict requirement; git safety guards still apply)
//   3. agent verdict   — posture: agent_verdict_ok + a high-confidence companion
//                        "approved" auto-merges without a human click
//
// request_changes routes the gate to a downstream `when decision ==
// 'changes_requested'` edge (typically back to an implementer).
//
// The dialogue lives on a single Interaction (stable ID, no loop suffix);
// each turn appends to Interaction.Turns and re-pauses. max_turns bounds it
// so the gate always converges to an asymptote.

// Reserved answers keys carrying the human's review-gate action on resume.
const (
	reviewActionKey        = "__review_action"         // reply | approve_merge | force_merge | request_changes
	reviewReplyKey         = "__review_reply"          // free-text reply for action=reply
	reviewMessageKey       = "__review_message"        // optional squash-commit message override
	reviewMergeStrategyKey = "__review_merge_strategy" // optional strategy override (squash|merge)
)

// Review-gate actions.
const (
	reviewActionReply          = "reply"
	reviewActionApproveMerge   = "approve_merge"
	reviewActionForceMerge     = "force_merge"
	reviewActionRequestChanges = "request_changes"
)

const reviewCompanionMessageContract = `

# Human-facing message contract (mandatory)

The first sentence must tell the operator exactly what action to take now.
Keep the complete message under 120 words and 800 characters. After that first
sentence, give at most three short numbered or bulleted checks. Use plain,
non-technical language. Do not mention implementation jargon, internal field
names, statuses or identifiers, file paths, URLs, commit hashes, or raw diff
content. Refer to an attached file only by its human caption; keep its path in
media_refs. Do not ask the operator to repeat checks the automated review has
already settled.
`

// ReviewCompanion is implemented by executors that can drive a review gate's
// companion LLM. The production ClawExecutor implements it; the e2e scenario
// stub may implement it to script companion turns without a real model.
type ReviewCompanion interface {
	ExecuteReviewCompanion(ctx context.Context, node *ir.HumanNode, systemText, userMessage string) (map[string]any, error)
}

// execReviewGate handles the FIRST entry to a review gate (the Run path).
// It runs the companion for turn 0, then either auto-merges (posture:
// agent_verdict_ok + favorable verdict) and returns the next node, or pauses
// for the human. Returns (nextNodeID, terminal, err): nextNodeID != "" means
// advance; otherwise the run has paused (ErrRunPaused handled by the caller).
func (e *Engine) execReviewGate(ctx context.Context, rs *runState, nodeID string, hn *ir.HumanNode) (string, bool, error) {
	if err := e.emit(ctx, rs.runID, store.EventNodeStarted, nodeID, map[string]any{
		"kind":      hn.NodeKind().String(),
		"iteration": e.currentLoopIteration(nodeID, rs.loopCounters),
	}); err != nil {
		return "", false, err
	}

	companion := e.runReviewCompanion(ctx, rs, hn, nodeID, nil)
	turns := []store.InteractionTurn{companionTurn(companion)}
	e.emitReviewTurn(ctx, rs.runID, nodeID, "companion", len(turns), companion)

	// Auto-merge on a favorable verdict when the operator allowed it.
	if hn.Posture == ir.PostureAgentVerdictOK && companionApproves(companion) {
		next, err := e.gateMergeAndSelectEdge(ctx, rs, hn, nodeID, approvedVerdict(companion), nil)
		if err != nil {
			return "", true, err
		}
		return next, false, nil
	}

	// Pause for the human.
	if err := e.pauseReviewGate(rs, nodeID, hn, companion, turns); err != nil {
		return "", true, err
	}
	return "", false, nil
}

// resumeReviewGate handles resuming a paused review gate. The answers carry a
// __review_action that decides whether to continue the dialogue (reply),
// squash-merge (approve_merge / force_merge), or request changes.
func (e *Engine) resumeReviewGate(ctx context.Context, r *store.Run, cp *store.Checkpoint, hn *ir.HumanNode, answers map[string]any) error {
	runID := r.ID
	nodeID := cp.NodeID
	action := reviewActionOf(answers)

	// Load the existing dialogue so we can append to it.
	var priorTurns []store.InteractionTurn
	if it, err := e.store.LoadInteraction(ctx, runID, cp.InteractionID); err == nil && it != nil {
		priorTurns = it.Turns
	}

	// Claim the run (paused → running) so a duplicate concurrent resume
	// can't spawn a second execution.
	claimed, claimErr := e.store.UpdateRunStatusIf(ctx, runID, store.RunStatusRunning, "",
		[]store.RunStatus{store.RunStatusPausedWaitingHuman})
	if claimErr != nil {
		return fmt.Errorf("runtime: claim run for review resume: %w", claimErr)
	}
	if !claimed {
		return fmt.Errorf("runtime: run %q is already being executed; refusing duplicate review resume", runID)
	}
	if err := e.emit(ctx, runID, store.EventRunResumed, "", nil); err != nil {
		return err
	}

	outputs := copyOutputs(cp.Outputs)
	if outputs == nil {
		outputs = make(map[string]map[string]any)
	}
	artifactVersions := cp.ArtifactVersions
	if artifactVersions == nil {
		artifactVersions = make(map[string]int)
	}

	rs, sandboxCleanup, rbErr := e.resumeRebuildState(ctx, r, cp, outputs, artifactVersions)
	if rbErr != nil {
		return rbErr
	}
	defer sandboxCleanup()

	switch action {
	case reviewActionApproveMerge, reviewActionForceMerge:
		// Record the verdict, squash-merge during the pause, advance.
		verdict := map[string]any{"decision": "approved"}
		mergeFromPrior(verdict, priorTurns)
		return e.gateResolveAndRun(ctx, r, rs, hn, nodeID, verdict, answers, true /* merge */)

	case reviewActionRequestChanges:
		verdict := map[string]any{"decision": "changes_requested"}
		mergeFromPrior(verdict, priorTurns)
		return e.gateResolveAndRun(ctx, r, rs, hn, nodeID, verdict, answers, false /* no merge */)

	default: // reviewActionReply (and any unknown action → safe re-pause)
		replyText := stringAnswer(answers, reviewReplyKey)
		turns := append(append([]store.InteractionTurn{}, priorTurns...),
			store.InteractionTurn{Role: "human", Content: replyText, At: time.Now().UTC()})
		e.emitReviewTurn(ctx, runID, nodeID, "human", len(turns), nil)

		// Asymptote backstop: once the companion has spoken max_turns times,
		// stop re-invoking it. The human must conclude (approve / force-merge /
		// request changes); further replies re-pause without growing the
		// dialogue, so the gate always converges.
		var companion map[string]any
		if countCompanionTurns(priorTurns) >= hn.MaxTurns {
			companion = map[string]any{
				"message":           "Maximum review turns reached. Please approve, force-merge, or request changes.",
				"needs_human_input": true,
			}
		} else {
			companion = e.runReviewCompanion(ctx, rs, hn, nodeID, turns)
		}
		turns = append(turns, companionTurn(companion))
		e.emitReviewTurn(ctx, runID, nodeID, "companion", len(turns), companion)

		// Auto-conclude when the operator allowed the agent's verdict to stand.
		if hn.Posture == ir.PostureAgentVerdictOK && companionApproves(companion) {
			return e.gateResolveAndRun(ctx, r, rs, hn, nodeID, approvedVerdict(companion), answers, true)
		}

		// Otherwise keep the dialogue going — re-pause with the full thread.
		return e.pauseReviewGate(rs, nodeID, hn, companion, turns)
	}
}

// gateResolveAndRun records the gate's resolution (verdict as the node
// output + node_finished), optionally squash-merges during the pause, then
// drives execLoop from the selected edge to the terminal node and finalizes
// the worktree. Used by the resume path (which owns its own execLoop).
func (e *Engine) gateResolveAndRun(ctx context.Context, r *store.Run, rs *runState, hn *ir.HumanNode, nodeID string, verdict, answers map[string]any, merge bool) error {
	var next string
	var err error
	if merge {
		next, err = e.gateMergeAndSelectEdge(ctx, rs, hn, nodeID, verdict, answers)
	} else {
		next, err = e.gateSelectEdge(ctx, rs, hn, nodeID, verdict)
	}
	if err != nil {
		return err
	}
	loopErr := e.execLoop(ctx, rs, next)
	e.evictRunSessions(rs.runID, loopErr)
	// Run-end finalize: the gate-merge already created the storage branch +
	// recorded merge_status, so finalizeOnExit skips the re-merge (see the
	// idempotency guard) and just tears down the worktree when present.
	e.finalizeReconstructedWorktree(ctx, rs.runID, r, loopErr)
	return loopErr
}

// gateMergeAndSelectEdge performs the squash-merge during the pause, records
// the verdict as the node output + node_finished, and returns the next node.
func (e *Engine) gateMergeAndSelectEdge(ctx context.Context, rs *runState, hn *ir.HumanNode, nodeID string, verdict, answers map[string]any) (string, error) {
	if err := e.performGateMerge(ctx, rs, hn, nodeID, answers); err != nil {
		// A merge failure is resumable: preserve the checkpoint so the
		// operator can fix the tree and re-approve / force.
		return "", e.failRunWithCheckpoint(rs, nodeID,
			fmt.Sprintf("review gate %q merge failed: %v", nodeID, err))
	}
	return e.gateSelectEdge(ctx, rs, hn, nodeID, verdict)
}

// gateSelectEdge records the verdict as the node output, emits the answered
// interaction + node_finished, persists the publish artifact, and selects
// the outgoing edge. Shared by the merge and request-changes paths.
func (e *Engine) gateSelectEdge(ctx context.Context, rs *runState, hn *ir.HumanNode, nodeID string, verdict map[string]any) (string, error) {
	rs.outputs[nodeID] = verdict

	// Record the verdict on the interaction as the answer + emit events,
	// mirroring the single-shot human-resume bookkeeping.
	interactionID := fmt.Sprintf("%s_%s", rs.runID, nodeID)
	if it, err := e.store.LoadInteraction(ctx, rs.runID, interactionID); err == nil && it != nil {
		now := time.Now().UTC()
		it.AnsweredAt = &now
		it.Answers = verdict
		if werr := e.store.WriteInteraction(ctx, it); werr != nil && e.logger != nil {
			e.logger.Warn("runtime: review gate %q: persist answered interaction: %v", nodeID, werr)
		}
	}
	if err := e.emit(ctx, rs.runID, store.EventHumanAnswersRecorded, nodeID, map[string]any{
		"interaction_id": interactionID,
		"answers":        verdict,
	}); err != nil {
		return "", err
	}

	if pub := nodePublish(hn); pub != "" {
		version := rs.artifactVersions[nodeID]
		if werr := e.store.WriteArtifact(ctx, &store.Artifact{
			RunID: rs.runID, NodeID: nodeID, Version: version, Data: verdict,
		}); werr != nil {
			return "", fmt.Errorf("runtime: review gate %q: write artifact: %w", nodeID, werr)
		}
		rs.artifactVersions[nodeID] = version + 1
		if err := e.emit(ctx, rs.runID, store.EventArtifactWritten, nodeID, map[string]any{
			"publish": pub, "version": version,
		}); err != nil && e.logger != nil {
			e.logger.Warn("runtime: review gate %q: emit artifact_written: %v", nodeID, err)
		}
	}

	if err := e.emit(ctx, rs.runID, store.EventNodeFinished, nodeID, nil); err != nil {
		return "", err
	}

	next, err := e.selectEdgeRS(rs, nodeID, verdict)
	if err != nil {
		return "", e.failRunErrWithCheckpoint(rs, nodeID, err)
	}
	return next, nil
}

// performGateMerge squash-merges the run's worktree into the target branch
// while the run is paused at the gate. It reuses the same primitives as the
// post-run deferred merge: createBranchSafely (the GC-guard storage branch)
// + PerformDeferredMerge (the guarded squash). On success it records
// final_commit / final_branch / merged_into / merge_status on run.json so the
// run-end finalize is a no-op. answers may carry message/strategy/target
// overrides from the studio merge form.
func (e *Engine) performGateMerge(ctx context.Context, rs *runState, hn *ir.HumanNode, nodeID string, answers map[string]any) error {
	r, err := e.store.LoadRun(ctx, rs.runID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	if ownershipErr := verifyFinalizationWorktreeOwnership(e.store, r); ownershipErr != nil {
		return fmt.Errorf("review gate refuses worktree mutation: %w", ownershipErr)
	}
	wtCtx := e.reconstructWorktreeContext(r)
	if wtCtx == nil {
		return fmt.Errorf("no worktree to merge (review gates require worktree: auto)")
	}
	finalSHA := readHEAD(wtCtx.wtPath)
	if finalSHA == "" {
		return fmt.Errorf("cannot read worktree HEAD at %s", wtCtx.wtPath)
	}

	branchName := e.branchName
	if branchName == "" {
		label := e.runName
		if label == "" {
			label = rs.runID
		}
		branchName = "iterion/run/" + label
	}

	// Precedence: studio merge-form override > launch flag (--merge-strategy /
	// --merge-into) > the bot's DSL default. The launch flag wins over the bot
	// so an operator can redirect a dogfood merge to a scratch branch.
	strategy := strutil.FirstNonBlank(stringAnswer(answers, reviewMergeStrategyKey), e.mergeStrategy, hn.MergeStrategy)
	mergeInto := strutil.FirstNonBlank(e.mergeInto, hn.MergeInto)

	// No commits → nothing to merge. Record skipped so the run-end finalize
	// doesn't try again, and so the studio shows "no commits".
	if finalSHA == wtCtx.originalTip {
		return e.persistGateMerge(ctx, rs.runID, "", "", "", "", string(store.MergeStatusSkipped), strategy)
	}

	created, finalName := createBranchSafely(wtCtx.repoRoot, branchName, finalSHA, e.logger)
	if !created {
		return fmt.Errorf("could not create storage branch %q for %s", branchName, shortSHA(finalSHA))
	}

	// merge_into: none (or detached HEAD) → branch only, no merge.
	if resolveMergeTarget(mergeInto, wtCtx.originalBranch) == "" {
		return e.persistGateMerge(ctx, rs.runID, finalSHA, finalName, "", "", string(store.MergeStatusSkipped), strategy)
	}

	message := strutil.FirstNonBlank(stringAnswer(answers, reviewMessageKey),
		buildSquashMessage(wtCtx.repoRoot, wtCtx.originalTip, finalSHA, e.runName))

	res, mErr := PerformDeferredMerge(DeferredMergeRequest{
		RepoRoot:      wtCtx.repoRoot,
		Target:        mergeInto,
		BranchToMerge: finalName,
		FinalSHA:      finalSHA,
		Strategy:      strategy,
		Message:       message,
	}, e.logger)
	if mErr != nil {
		// The storage branch is preserved; surface the guard/conflict error.
		return fmt.Errorf("%w", mErr)
	}

	if err := e.persistGateMerge(ctx, rs.runID, finalSHA, finalName, res.MergedInto, res.MergedCommit, string(store.MergeStatusMerged), res.Strategy); err != nil {
		return err
	}
	return e.emit(ctx, rs.runID, store.EventReviewMerged, nodeID, map[string]any{
		"final_commit": finalSHA,
		"merged_into":  res.MergedInto,
		"strategy":     res.Strategy,
	})
}

// persistGateMerge records the gate-merge outcome on run.json. final_branch
// being set is the marker the run-end finalize uses to skip re-finalization.
func (e *Engine) persistGateMerge(ctx context.Context, runID, finalCommit, finalBranch, mergedInto, mergedCommit, status, strategy string) error {
	r, err := e.store.LoadRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run for merge persist: %w", err)
	}
	if finalCommit != "" {
		r.FinalCommit = finalCommit
	}
	if finalBranch != "" {
		r.FinalBranch = finalBranch
	}
	r.MergedInto = mergedInto
	r.MergedCommit = mergedCommit
	r.MergeStatus = store.MergeStatus(status)
	if strategy != "" {
		r.MergeStrategy = store.MergeStrategy(strategy)
	}
	if err := e.store.SaveRun(ctx, r); err != nil {
		return fmt.Errorf("persist gate merge: %w", err)
	}
	return nil
}

// pauseReviewGate pauses the run at the review gate, carrying the companion's
// latest message + verdict + the merge configuration so the studio can render
// the Review-&-Merge card. The full dialogue (turns) rides on the interaction.
func (e *Engine) pauseReviewGate(rs *runState, nodeID string, hn *ir.HumanNode, companion map[string]any, turns []store.InteractionTurn) error {
	questions := e.buildNodeInputRS(nodeID, rs.scope())

	message := stringAnswer(companion, "message")
	if strings.TrimSpace(message) == "" {
		// No companion message (e.g. the stub executor) → fall back to the
		// authored instructions prompt so the human still has guidance.
		if extra := e.humanInstructionsExtra(nodeID, questions, rs); extra != nil {
			if s, ok := extra["instructions"].(string); ok {
				message = s
			}
		}
	}

	extra := map[string]any{
		"review":       true,
		"instructions": message, // reuse the studio's existing instructions rendering
		"posture":      hn.Posture,
		// Show the EFFECTIVE merge target/strategy (launch flag > bot default),
		// matching what performGateMerge will actually do.
		"merge_strategy": strutil.FirstNonBlank(e.mergeStrategy, hn.MergeStrategy),
		"merge_into":     strutil.FirstNonBlank(e.mergeInto, hn.MergeInto),
		"max_turns":      hn.MaxTurns,
		"turns":          turns, // the full companion↔human thread, for self-contained studio rendering
	}
	// Keep the key even for an empty selection: presence tells doPause that
	// the companion's choice is authoritative and prevents a fallback to any
	// media_refs supplied as node input.
	extra["media"] = companionMedia(companion)
	if url := e.resolveReviewURL(hn, questions, rs); url != "" {
		extra["review_url"] = url
	}
	if v := reviewVerdict(companion); v != nil {
		extra["verdict"] = v
	}

	return e.doPause(rs, nodeID, questions, extra, pauseInfo{Turns: turns})
}

// runReviewCompanion resolves the companion system prompt and assembles its
// user message (diff context + dialogue transcript), then calls the executor.
// When the executor doesn't implement ReviewCompanion (e.g. the e2e stub
// without it), it returns a default result that pauses for the human.
func (e *Engine) runReviewCompanion(ctx context.Context, rs *runState, hn *ir.HumanNode, nodeID string, turns []store.InteractionTurn) map[string]any {
	rc, ok := e.executor.(ReviewCompanion)
	if !ok {
		return map[string]any{"needs_human_input": true}
	}

	questions := e.buildNodeInputRS(nodeID, rs.scope())
	var systemText string
	if hn.SystemPrompt != "" {
		if p := e.workflow.Prompts[hn.SystemPrompt]; p != nil {
			systemText = e.renderHumanInstructions(p, questions, rs)
		}
	}
	availableMedia := e.availableReviewMedia(rs)
	userMessage := e.buildCompanionMessage(rs, hn, turns, availableMedia)

	out, err := rc.ExecuteReviewCompanion(ctx, hn, systemText, userMessage)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("runtime: review gate %q: companion failed: %v — pausing for human", nodeID, err)
		}
		return map[string]any{"needs_human_input": true}
	}
	// The model selects exact paths only. Replace its objects with metadata
	// from the actual run-file manifest before anything is persisted/emitted.
	out[reviewMediaRefsKey] = normalizeReviewMediaRefs(out[reviewMediaRefsKey], availableMedia)
	return out
}

// buildCompanionMessage assembles the companion's user message: a bounded
// diff of the run's commits, the passive-attachment manifest, then the dialogue
// transcript so far. The manifest is the companion's entire attachment
// authority: it cannot attach arbitrary URLs or files outside this list.
func (e *Engine) buildCompanionMessage(rs *runState, hn *ir.HumanNode, turns []store.InteractionTurn, availableMedia []store.ReviewMediaRef) string {
	var b strings.Builder
	if r, err := e.store.LoadRun(rs.ctx, rs.runID); err == nil {
		if wtCtx := e.reconstructWorktreeContext(r); wtCtx != nil && wtCtx.originalTip != "" {
			if diff := reviewDiffContext(wtCtx); diff != "" {
				b.WriteString("# Change under review (git diff)\n\n")
				b.WriteString(diff)
				b.WriteString("\n\n")
			}
		}
	}
	writeReviewMediaInventory(&b, availableMedia)
	if len(turns) == 0 {
		b.WriteString("Begin the review with only the consequential checks that still require human judgment. " +
			"Then set needs_human_input=true to wait for their report. Attach only relevant items from " +
			"the available-review-attachments list in media_refs; use [] when none are relevant.")
		b.WriteString(reviewCompanionMessageContract)
		return b.String()
	}
	b.WriteString("# Conversation so far\n\n")
	for _, t := range turns {
		role := t.Role
		if role == "" {
			role = "human"
		}
		b.WriteString(strings.ToUpper(role[:1]) + role[1:] + ": " + t.Content + "\n\n")
		for _, media := range t.Media {
			fmt.Fprintf(&b, "Attached media: %q", media.Path)
			if media.Caption != "" {
				fmt.Fprintf(&b, " — %q", media.Caption)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("Respond to the human's latest message. If their testing confirms the change works, " +
		"set decision=approved with your confidence; if they report problems, set decision=changes_requested " +
		"and list the blockers. Set needs_human_input=false only when you have reached a confident verdict. " +
		"Attach only relevant items from the available-review-attachments list in media_refs; use [] when none are relevant.")
	b.WriteString(reviewCompanionMessageContract)
	return b.String()
}

func writeReviewMediaInventory(b *strings.Builder, media []store.ReviewMediaRef) {
	if b == nil {
		return
	}
	b.WriteString("# Available review attachments\n\n")
	if len(media) == 0 {
		b.WriteString("No review attachments are available. Return media_refs as an empty array.\n\n")
		return
	}
	const promptLimit = 100
	for i, ref := range media {
		if i == promptLimit {
			fmt.Fprintf(b, "- … %d more file(s) omitted; do not attach omitted paths\n", len(media)-promptLimit)
			break
		}
		fmt.Fprintf(b, "- %q (%s, %s, %d bytes)\n", ref.Path, ref.Kind, ref.MIME, ref.Size)
	}
	b.WriteString("\nSelect media_refs only from the exact paths above. Each item must contain path and a short human caption.\n\n")
}

// reviewDiffContext returns a bounded diff of the run's commits for the
// companion. Best-effort: returns "" on any git error.
func reviewDiffContext(wtCtx *worktreeContext) string {
	statCmd, statCancel := gitCmd("-C", wtCtx.wtPath, "diff", "--stat", wtCtx.originalTip+"..HEAD")
	out, err := statCmd.Output()
	statCancel()
	stat := ""
	if err == nil {
		stat = strings.TrimSpace(string(out))
	}
	fullCmd, fullCancel := gitCmd("-C", wtCtx.wtPath, "diff", wtCtx.originalTip+"..HEAD")
	full, ferr := fullCmd.Output()
	fullCancel()
	body := ""
	if ferr == nil {
		body = string(full)
	}
	const maxDiff = 8000
	if len(body) > maxDiff {
		body = body[:maxDiff] + "\n… (diff truncated)\n"
	}
	combined := strings.TrimSpace(stat + "\n\n" + body)
	return combined
}

// resolveReviewURL renders the gate's review_url template (e.g.
// "{{outputs.provision.url}}") against the run state. Returns "" when unset
// or when a referenced output isn't available yet.
func (e *Engine) resolveReviewURL(hn *ir.HumanNode, questions map[string]any, rs *runState) string {
	if hn.ReviewURL == "" {
		return ""
	}
	if len(hn.ReviewURLRefs) == 0 {
		return hn.ReviewURL
	}
	pairs := make([]string, 0, 2*len(hn.ReviewURLRefs))
	for _, ref := range hn.ReviewURLRefs {
		val := e.resolveRef(ref, resolveScope{
			vars:      rs.vars,
			outputs:   rs.outputs,
			runInputs: questions,
			artifacts: rs.artifacts,
			rs:        rs,
		})
		pairs = append(pairs, ref.Raw, renderInstructionValue(val))
	}
	return strings.TrimSpace(strings.NewReplacer(pairs...).Replace(hn.ReviewURL))
}

// emitReviewTurn emits a review_turn event (and, for companion turns with a
// verdict, a review_verdict event) for the studio dialogue thread.
func (e *Engine) emitReviewTurn(ctx context.Context, runID, nodeID, role string, turn int, companion map[string]any) {
	_ = e.emit(ctx, runID, store.EventReviewTurn, nodeID, map[string]any{
		"role": role,
		"turn": turn,
	})
	if companion == nil {
		return
	}
	if v := reviewVerdict(companion); v != nil {
		_ = e.emit(ctx, runID, store.EventReviewVerdict, nodeID, v)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// reviewActionOf extracts the review action from resume answers, defaulting
// to "reply" (the safe non-merging continuation) when unset/unknown.
func reviewActionOf(answers map[string]any) string {
	switch stringAnswer(answers, reviewActionKey) {
	case reviewActionApproveMerge:
		return reviewActionApproveMerge
	case reviewActionForceMerge:
		return reviewActionForceMerge
	case reviewActionRequestChanges:
		return reviewActionRequestChanges
	default:
		return reviewActionReply
	}
}

// countCompanionTurns returns how many turns the companion has spoken — the
// dialogue length the max_turns backstop bounds.
func countCompanionTurns(turns []store.InteractionTurn) int {
	n := 0
	for _, t := range turns {
		if t.Role == "companion" {
			n++
		}
	}
	return n
}

// companionTurn builds an InteractionTurn from a companion result.
func companionTurn(companion map[string]any) store.InteractionTurn {
	return store.InteractionTurn{
		Role:    "companion",
		Content: stringAnswer(companion, "message"),
		Verdict: reviewVerdict(companion),
		Media:   companionMedia(companion),
		At:      time.Now().UTC(),
	}
}

func companionMedia(companion map[string]any) []store.ReviewMediaRef {
	if companion == nil {
		return nil
	}
	media, _ := companion[reviewMediaRefsKey].([]store.ReviewMediaRef)
	return media
}

// checkpointReviewGateState turns the typed values assembled by
// pauseReviewGate into the checkpoint-owned read model used by projections
// that deliberately do not replay events. Nil means an ordinary human pause.
func checkpointReviewGateState(eventExtra map[string]any, turns []store.InteractionTurn) *store.ReviewGateState {
	if eventExtra == nil || eventExtra["review"] != true {
		return nil
	}
	state := &store.ReviewGateState{
		Turns: append([]store.InteractionTurn(nil), turns...),
	}
	state.Posture, _ = eventExtra["posture"].(string)
	state.MergeStrategy, _ = eventExtra["merge_strategy"].(string)
	state.MergeInto, _ = eventExtra["merge_into"].(string)
	state.MaxTurns, _ = eventExtra["max_turns"].(int)
	state.ReviewURL, _ = eventExtra["review_url"].(string)
	if verdict, ok := eventExtra["verdict"].(map[string]any); ok {
		state.Verdict = cloneMap(verdict)
	}
	return state
}

// reviewVerdict extracts the verdict fields (decision/confidence/blockers)
// from a companion result. Returns nil when no decision was produced.
func reviewVerdict(companion map[string]any) map[string]any {
	decision := stringAnswer(companion, "decision")
	if decision == "" {
		return nil
	}
	v := map[string]any{"decision": decision}
	if c := stringAnswer(companion, "confidence"); c != "" {
		v["confidence"] = c
	}
	if b, ok := companion["blockers"]; ok {
		v["blockers"] = b
	}
	return v
}

// approvedVerdict builds the node output for an agent-verdict auto-merge,
// pinning decision=approved while keeping the companion's confidence/blockers.
func approvedVerdict(companion map[string]any) map[string]any {
	v := reviewVerdict(companion)
	if v == nil {
		v = map[string]any{}
	}
	v["decision"] = "approved"
	return v
}

// mergeFromPrior copies confidence/blockers from the last companion turn into
// an explicit-action verdict, so a human-approved/-rejected gate still carries
// the agent's last assessment downstream.
func mergeFromPrior(verdict map[string]any, turns []store.InteractionTurn) {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "companion" && turns[i].Verdict != nil {
			for k, v := range turns[i].Verdict {
				if k == "decision" {
					continue // the human's action sets the decision
				}
				if _, exists := verdict[k]; !exists {
					verdict[k] = v
				}
			}
			return
		}
	}
}

// companionApproves reports whether the companion reached a high-confidence
// approval that may auto-merge under posture: agent_verdict_ok.
func companionApproves(companion map[string]any) bool {
	if needsHuman, ok := companion["needs_human_input"].(bool); ok && needsHuman {
		return false
	}
	return stringAnswer(companion, "decision") == "approved" &&
		stringAnswer(companion, "confidence") == "high"
}

// stringAnswer reads a string-valued map entry, tolerating absent/non-string.
func stringAnswer(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}
