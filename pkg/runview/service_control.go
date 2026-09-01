package runview

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ---------------------------------------------------------------------------
// Write-side API: lifecycle
// ---------------------------------------------------------------------------

// Cancel signals an active run to stop. Returns ErrRunNotActive if the
// run is not held by this process — cross-process cancel is not
// supported in the current design.
func (s *Service) Cancel(runID string) error {
	if s.publisher != nil {
		// Cloud-mode: the runner pool owns the lifecycle. The
		// publisher flips the Mongo doc to cancelled so the
		// runner's cooperative-cancel check (pkg/runner/loop.go)
		// acks the next delivery without executing; if a runner
		// is currently holding the lease, the cancel subject
		// `iterion.cancel.<run_id>` unwinds engine.Run via
		// handleContextDoneWithCheckpoint.
		return s.publisher.CancelRun(context.Background(), runID)
	}
	return s.manager.Cancel(runID)
}

// CancelWithReason mirrors Cancel but records WHY on the run (run.Error) —
// what the run list, board cards and merge-gate synthetic statuses read. An
// automated caller (the webhook supersede lane) must not sign its cancel
// "cancelled by user". The in-process manager path has no reason plumbing —
// the engine stamps its own cancellation semantics — so the reason applies
// on the cloud path, where the bare status flip IS the record.
func (s *Service) CancelWithReason(runID, reason string) error {
	if s.publisher != nil {
		return s.publisher.CancelRunWithReason(context.Background(), runID, reason)
	}
	return s.manager.Cancel(runID)
}

// CancelInactive flips a persisted-but-not-active run to cancelled status
// when the operator clicked Cancel on a paused_waiting_human or
// failed_resumable run. Returns (cancelled, error): cancelled=true means
// the status was actually flipped; false+nil means the run was already
// terminal (no-op). Cross-process cancel of a held run is still not
// supported — this only handles the case where no goroutine owns it.
//
// After flipping, RecoverFinalize fires so the studio's merge UI can act
// on whatever commits the run produced before it stalled (counterpart to
// the post-cancel finalize in spawnRun).
func (s *Service) CancelInactive(runID string) (bool, error) {
	return s.CancelInactiveCtx(context.Background(), runID)
}

// Pause requests an in-process operator pause for runID — the engine
// observes the closed pauseCh at the next safe boundary (top of
// execLoop, between LLM turns inside an agent's loop), saves a
// checkpoint, flips status to paused_operator, and returns
// ErrRunPausedOperator. Idempotent.
//
// Returns ErrRunNotActive when no goroutine owns runID. Cloud-mode
// cross-process pause is out of scope for Phase 1 — the publisher
// path falls back to ErrRunNotActive (a NATS pause subject is the
// follow-up).
func (s *Service) Pause(runID string) error {
	if s.publisher != nil {
		// Cross-process pause via NATS is not implemented yet — for
		// cloud-mode runs, surface ErrRunNotActive so the HTTP layer
		// returns 409 and the studio can hide the Pause button when
		// the run is not held in this process. Follow-up: add
		// `iterion.pause.<run_id>` analogous to the cancel subject.
		return ErrRunNotActive
	}
	return s.manager.RequestPause(runID)
}

// CancelInactiveCtx is the tenant-aware variant of CancelInactive.
func (s *Service) CancelInactiveCtx(ctx context.Context, runID string) (bool, error) {
	if runID == "" {
		return false, errors.New("runview: run_id is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("load run: %w", err)
	}
	if r.Status == store.RunStatusQueued {
		// A queued root has no engine to signal — Cancel and the dispatcher
		// both miss it — so dropping it out of the FIFO is what actually
		// stops it. Do that BEFORE the status write so the scheduler cannot
		// dequeue it in between; startQueuedRun re-checks the status anyway.
		s.pipelineQueue.dropQueued(runID)
	}
	switch r.Status {
	case store.RunStatusQueued:
		// Never started, so there is no checkpoint to lose and no partial
		// work to strand — `cancelled` is exactly what happened.
	case store.RunStatusPausedWaitingHuman, store.RunStatusFailedResumable, store.RunStatusPausedOperator:
		// flippable — paused_operator included so an orphaned operator-paused
		// run can still be cancelled after a daemon restart
	// `failed` is deliberately NOT flippable. It looks like a harmless
	// dismissal — the run is dead either way — but `cancelled` is a RESUMABLE
	// status everywhere in the engine (runtime/resume.go, cli/resume.go, the
	// studio's Resume affordance) while applyStatusTransition clears the
	// checkpoint on `failed`. The flip would therefore advertise Resume on a
	// checkpoint-less run, and Engine.Resume routes that to resumeFromFailure,
	// which restarts from the WORKFLOW ENTRY — re-burning the whole budget on
	// a run whose failure may have been an intentional FailNode termination.
	// Standalone failed runs need no dismissal anyway: they reserve nothing
	// (only ticket-backed cards reserve) and the board files them in Closed.
	default:
		return false, nil // already terminal — no-op
	}
	// UpdateRunStatus REPLACES run.Error (applyStatusTransition does a bare
	// r.Error = runErr), so the original failure text has to be carried
	// forward explicitly. It is the only record in run.json of WHY the run
	// died — the board card and the REST payload both read it from there —
	// and dismissing a failure must not erase its cause. events.jsonl still
	// has it, but nothing in the UI reads that.
	reason := "cancelled by operator (was " + string(r.Status) + ")"
	if r.Error != "" {
		reason += ": " + r.Error
	}
	if err := s.store.UpdateRunStatusCoded(ctx, runID, store.RunStatusCancelled, reason, store.FailureCancelled); err != nil {
		return false, fmt.Errorf("update status: %w", err)
	}
	// Re-load post-flip so RecoverFinalize sees the new status.
	r, err = s.store.LoadRun(ctx, runID)
	if err == nil {
		if recErr := runtime.RecoverFinalize(ctx, s.store, r, s.logger); recErr != nil && s.logger != nil {
			s.logger.Warn("runview: post-cancel-inactive finalize for %s: %v", runID, recErr)
		}
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// User-message inbox (chatbox queued messages)
// ---------------------------------------------------------------------------

// QueueMessage appends a new operator chat message to the run's
// inbox in "queued" status, emits user_message_queued so WS
// subscribers can update their UI, and returns the persisted record.
// The engine drains pending messages cooperatively at safe boundaries
// (between agent-loop iterations for claw, at the next human pause
// for claude_code / codex) — there is no preemption of the running
// agent.
func (s *Service) QueueMessage(ctx context.Context, runID, text string, opts ...QueueMessageOption) (*store.QueuedUserMessage, error) {
	if runID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	if text == "" {
		return nil, errors.New("runview: message text is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run: %w", err)
	}
	switch r.Status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusCancelled:
		return nil, fmt.Errorf("run %s is terminal (%s); cannot queue message", runID, r.Status)
	}
	cfg := queueMessageConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	msg := store.QueuedUserMessage{
		ID:            newQueuedMessageID(),
		Text:          text,
		TenantID:      r.TenantID,
		NodeID:        cfg.nodeID,
		SkillRefs:     cfg.skillRefs,
		InteractionID: cfg.interactionID,
	}
	if err := s.store.AppendQueuedMessage(ctx, runID, msg); err != nil {
		return nil, fmt.Errorf("append queued message: %w", err)
	}
	if err := store.NormalizeQueuedForAppend(&msg, runID); err != nil {
		return nil, err
	}
	store.PublishInboxEvent(ctx, s.store, s.brokerPublish(), store.EventUserMessageQueued, runID, msg)
	return &msg, nil
}

// queueMessageConfig accumulates the optional knobs callers can pass
// to QueueMessage via the QueueMessageOption family. Kept as a
// private struct so the public surface stays narrow (callers see
// only the option-builder helpers below).
type queueMessageConfig struct {
	skillRefs     []string
	nodeID        string
	interactionID string
}

// QueueMessageOption is the functional-option form of QueueMessage's
// extras. Today only WithMessageSkills exists; adding more knobs
// (e.g. attachments, source attribution) won't break existing
// callers.
type QueueMessageOption func(*queueMessageConfig)

// WithMessageSkills attaches bundle skill names to the queued
// message. Before the engine injects the message into the agent's
// conversation, each skill's SKILL.md is mirrored into the
// workspace's .claude/skills/ directory. Sticky — the skill stays
// loaded for the rest of the run. Empty/nil slice is a no-op.
func WithMessageSkills(skills []string) QueueMessageOption {
	return func(c *queueMessageConfig) { c.skillRefs = skills }
}

// WithMessageNode scopes the queued message to a single workflow node:
// the engine's drain only releases it while that node is the active
// executing node (see store.QueuedUserMessage.NodeID). A supervisor
// watching one node tags its steering messages this way so a late
// message can't leak into the next node. Empty nodeID = run-scoped
// (the default for operator-typed chatbox messages).
func WithMessageNode(nodeID string) QueueMessageOption {
	return func(c *queueMessageConfig) { c.nodeID = nodeID }
}

// WithMessageInteraction marks the queued message as the delivery of an
// async question's answer (ADR-081), referencing the answered
// interaction. The await-resume path cancels superseded deliveries by
// this typed field.
func WithMessageInteraction(interactionID string) QueueMessageOption {
	return func(c *queueMessageConfig) { c.interactionID = interactionID }
}

// CancelQueuedMessage marks a queued (not-yet-delivered) message as
// cancelled. Returns store.ErrQueuedMessageNotFound or
// store.ErrQueuedMessageStatusConflict (already-delivered) so the
// HTTP handler can map them to 404 / 409 respectively.
func (s *Service) CancelQueuedMessage(ctx context.Context, runID, msgID string) error {
	if runID == "" || msgID == "" {
		return errors.New("runview: run_id and message_id are required")
	}
	if err := s.store.UpdateQueuedMessageStatus(ctx, runID, msgID, store.QueuedMessageStatusCancelled, store.QueuedMessageStatusQueued); err != nil {
		return err
	}
	msg := store.QueuedUserMessage{ID: msgID}
	store.StampQueuedTransition(&msg, store.QueuedMessageStatusCancelled, time.Now().UTC())
	store.PublishInboxEvent(ctx, s.store, s.brokerPublish(), store.EventUserMessageCancelled, runID, msg)
	return nil
}

// ListQueuedMessages returns every message recorded for the run in
// FIFO order, regardless of current status. Used by the studio for
// initial hydration alongside the run snapshot.
func (s *Service) ListQueuedMessages(ctx context.Context, runID string) ([]store.QueuedUserMessage, error) {
	if runID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	return s.store.ListQueuedMessages(ctx, runID)
}

// brokerPublish returns broker.Publish as a free function, or nil
// when no broker is wired. Shape matches store.PublishInboxEvent.
func (s *Service) brokerPublish() func(store.Event) {
	if s.broker == nil {
		return nil
	}
	return s.broker.Publish
}

// newQueuedMessageID returns a short opaque ID for inbox messages.
// The format is owned by the store so every producer (service, CLI)
// mints the same shape.
func newQueuedMessageID() string { return store.NewQueuedMessageID() }

// ---------------------------------------------------------------------------
// Deferred merge
// ---------------------------------------------------------------------------

// MergeRequest carries the parameters of a UI-driven merge action. The
// HTTP handler builds it from the request body; the Service translates
// it into a runtime.PerformDeferredMerge call and persists the outcome.
type MergeRequest struct {
	// Strategy is "squash" (default when empty) or "merge".
	Strategy store.MergeStrategy
	// MergeInto is the target branch override:
	//   ""        → currently-checked-out branch (default)
	//   "current" → same as default
	//   <branch>  → that branch (must equal currently-checked-out)
	MergeInto string
	// CommitMessage overrides the squash commit message. Ignored for
	// "merge" strategy. Empty falls back to a generated message that
	// lists each squashed commit.
	CommitMessage string
}

// MergeResponse mirrors the persisted Run fields after a successful
// merge so the HTTP handler can return them without re-loading.
type MergeResponse struct {
	MergedCommit  string              `json:"merged_commit"`
	MergedInto    string              `json:"merged_into"`
	MergeStrategy store.MergeStrategy `json:"merge_strategy"`
	MergeStatus   store.MergeStatus   `json:"merge_status"`
	// SourceIssueID is set when the run was dispatcher-spawned (i.e.
	// Run.Source is non-nil). The HTTP handler reads it to fire the
	// post-merge auto-transition without a second LoadRun round-trip.
	// Internal-only — omitted from the JSON wire.
	SourceIssueID string `json:"-"`
}

// mergeClaimStaleAfter bounds how long a merge claim protects its
// holder: a "merging" older than this is up for grabs (the claimant
// crashed mid-merge — a wedged claim must not block the run forever).
// Generous vs the per-command git timeouts (120s) plus a server-side
// clone of a large repo.
const mergeClaimStaleAfter = 15 * time.Minute

// mergeUpdateFromRun snapshots r's current merge bookkeeping into a
// RunMergeUpdate, so a transition writes the fields it changes and
// carries the rest unchanged (the same shape a full SaveRun of the
// mutated r would have produced).
func mergeUpdateFromRun(r *store.Run) store.RunMergeUpdate {
	return store.RunMergeUpdate{
		Status:              r.MergeStatus,
		MergedCommit:        r.MergedCommit,
		MergedInto:          r.MergedInto,
		MergeStrategy:       r.MergeStrategy,
		PendingMergeMessage: r.PendingMergeMessage,
		PendingMergeInto:    r.PendingMergeInto,
	}
}

// PerformMerge runs the deferred merge for runID. Preconditions:
//   - run.FinalCommit and run.FinalBranch must be set (the engine must
//     have created the storage branch — runs without commits cannot be
//     merged).
//   - run.MergeStatus must not already be "merged" (idempotence; clients
//     that want to redo a merge should explicitly reset state first).
//
// On success, the run.json is updated with the merge outcome and the
// new state is returned.
func (s *Service) PerformMerge(runID string, req MergeRequest) (*MergeResponse, error) {
	return s.PerformMergeCtx(context.Background(), runID, req)
}

// PerformMergeCtx is the tenant-aware variant of PerformMerge.
func (s *Service) PerformMergeCtx(ctx context.Context, runID string, req MergeRequest) (*MergeResponse, error) {
	if runID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.FinalCommit == "" || r.FinalBranch == "" {
		return nil, fmt.Errorf("run %q has no storage branch — nothing to merge (FinalCommit=%q, FinalBranch=%q)", runID, r.FinalCommit, r.FinalBranch)
	}
	if r.MergeStatus == store.MergeStatusMerged {
		return nil, fmt.Errorf("run %q is already merged into %q at %s", runID, r.MergedInto, r.MergedCommit)
	}

	// Claim the merge before touching git: a compare-and-set to
	// "merging", so two replicas (or two operator clicks) cannot both
	// build a squash for the same run — the loser stops HERE, before
	// any clone or push. The claim is released on every early exit and
	// consumed by exactly one conditional persist below.
	claimed, prior, claimToken, err := s.store.ClaimMerge(ctx, runID, time.Now().Add(-mergeClaimStaleAfter))
	if err != nil {
		return nil, err
	}
	if !claimed {
		switch prior {
		case store.MergeStatusMerged:
			return nil, fmt.Errorf("run %q is already merged", runID)
		case store.MergeStatusMerging:
			return nil, fmt.Errorf("run %q has a merge in progress (claimed by another worker) — retry after it finishes", runID)
		default:
			return nil, fmt.Errorf("run %q is not mergeable from merge_status=%q", runID, prior)
		}
	}
	// Merge-state writes ride a context detached from the request: once
	// the claim is held (and a fortiori once git has run), a client
	// disconnect or ingress timeout must not be able to leak the claim
	// or lose the outcome — a cancelled release would wedge the run in
	// "merging" for the whole staleness window.
	wctx := context.WithoutCancel(ctx)
	r.MergeStatus = store.MergeStatusMerging
	// Until an outcome is persisted, every exit restores the pre-claim
	// state so an aborted attempt (bad token, unreachable repo) leaves
	// the run exactly as found. A stale claim we stole restores to
	// "failed", not back to "merging" — re-wedging it would hide the
	// crash for another staleness window.
	restoreTo := prior
	if restoreTo == store.MergeStatusMerging {
		restoreTo = store.MergeStatusFailed
	}
	persisted := false
	defer func() {
		if persisted {
			return
		}
		upd := mergeUpdateFromRun(r)
		upd.Status = restoreTo
		upd.ExpectClaimedAt = claimToken
		if changed, rerr := s.store.UpdateRunMergeIf(wctx, runID, upd, []store.MergeStatus{store.MergeStatusMerging}); rerr != nil || !changed {
			if s.logger != nil {
				s.logger.Warn("runview: release merge claim for %s (restore to %q): changed=%v err=%v", runID, restoreTo, changed, rerr)
			}
		}
	}()

	repoRoot := mergeRepoRoot(r)
	remote := repoRoot == "" && r.RepoURL != ""
	token := ""
	if remote {
		// Repo-targeted run: the runner workspace is gone, the storage
		// branch lives on the forge. Materialise the server-side merge
		// clone and run the exact same pipeline a local run gets.
		if s.forgeToken != nil {
			if token, err = s.forgeToken(ctx, r); err != nil {
				return nil, fmt.Errorf("forge token for the merge of %s: %w", runID, err)
			}
		}
		if repoRoot, err = s.ensureRepoTargetedMergeRoot(ctx, r, token, req.MergeInto); err != nil {
			return nil, err
		}
	}
	if repoRoot == "" {
		return nil, fmt.Errorf("run %q has no resolvable repo root", runID)
	}

	// Out-of-band merge: if the run's HEAD is already an ancestor of the
	// resolved target (someone merged the storage branch via git CLI / CI
	// outside the studio), there's nothing to squash — a redundant squash
	// would just build a confusing empty commit. Reconcile to "merged" and
	// return a no-op success instead.
	if ok, rcErr := s.reconcileOutOfBandMergeClaimed(wctx, r, repoRoot, resolveMergeTargetForPersistence(req.MergeInto, repoRoot), claimToken); rcErr != nil {
		return nil, rcErr
	} else if ok {
		persisted = true
		return &MergeResponse{
			MergedCommit:  r.MergedCommit,
			MergedInto:    r.MergedInto,
			MergeStrategy: r.MergeStrategy,
			MergeStatus:   r.MergeStatus,
			SourceIssueID: sourceIssueID(r),
		}, nil
	}

	strategy := req.Strategy
	if strategy == "" {
		strategy = store.MergeStrategySquash
	}

	message := req.CommitMessage
	if message == "" && strategy == store.MergeStrategySquash {
		message = runtime.BuildSquashMessage(repoRoot, r.BaseCommit, r.FinalCommit, runtime.RunDisplayName(r))
	}

	res, mergeErr := runtime.PerformDeferredMerge(runtime.DeferredMergeRequest{
		RepoRoot:      repoRoot,
		Target:        req.MergeInto,
		BranchToMerge: r.FinalBranch,
		FinalSHA:      r.FinalCommit,
		Strategy:      string(strategy),
		Message:       message,
	}, s.logger)
	if mergeErr != nil {
		// Content conflicts produce a typed error and leave the
		// worktree in the conflicted state. Persist
		// MergeStatusConflicted (not "failed") so the studio drops
		// into the conflict resolver instead of the retry path.
		// Also stash the squash message in MergedInto's sibling
		// fields so FinalizeMergeAfterConflict can recover it
		// without recomputing — strategy is already known to be
		// squash at this point (conflict can't arise from FF).
		var conflictErr *runtime.MergeConflictError
		if errors.As(mergeErr, &conflictErr) {
			r.MergeStatus = store.MergeStatusConflicted
			r.MergeStrategy = store.MergeStrategySquash
			r.PendingMergeMessage = message
			r.PendingMergeInto = resolveMergeTargetForPersistence(req.MergeInto, repoRoot)
			upd := mergeUpdateFromRun(r)
			upd.ExpectClaimedAt = claimToken
			changed, saveErr := s.store.UpdateRunMergeIf(wctx, runID, upd, []store.MergeStatus{store.MergeStatusMerging})
			if saveErr != nil || !changed {
				if s.logger != nil {
					s.logger.Warn("runview: persist merge conflict for %s: changed=%v err=%v", runID, changed, saveErr)
				}
			} else {
				persisted = true
			}
			return nil, mergeErr
		}
		// Persist the failure so the studio can show "Retry merge".
		r.MergeStatus = store.MergeStatusFailed
		upd := mergeUpdateFromRun(r)
		upd.ExpectClaimedAt = claimToken
		changed, saveErr := s.store.UpdateRunMergeIf(wctx, runID, upd, []store.MergeStatus{store.MergeStatusMerging})
		if saveErr != nil || !changed {
			if s.logger != nil {
				s.logger.Warn("runview: persist merge failure for %s: changed=%v err=%v", runID, changed, saveErr)
			}
		} else {
			persisted = true
		}
		return nil, mergeErr
	}

	// Success. A repo-targeted merge only exists once the forge has it:
	// push first, persist after, then drop the re-creatable clone.
	if remote {
		if pushErr := s.pushRepoTargetedMerge(ctx, repoRoot, token, res.MergedInto); pushErr != nil {
			r.MergeStatus = store.MergeStatusFailed
			upd := mergeUpdateFromRun(r)
			upd.ExpectClaimedAt = claimToken
			changed, saveErr := s.store.UpdateRunMergeIf(wctx, runID, upd, []store.MergeStatus{store.MergeStatusMerging})
			if saveErr != nil || !changed {
				if s.logger != nil {
					s.logger.Warn("runview: persist merge failure for %s: changed=%v err=%v", runID, changed, saveErr)
				}
			} else {
				persisted = true
			}
			return nil, fmt.Errorf("merged in the server-side clone but the push failed — %s does NOT carry the merge: %w", res.MergedInto, pushErr)
		}
	}
	persisted = true
	resp, err := s.persistMergeSuccess(wctx, r, res.MergedCommit, res.MergedInto, store.MergeStrategy(res.Strategy), []store.MergeStatus{store.MergeStatusMerging}, claimToken)
	if err == nil && remote {
		s.removeRepoTargetedMergeRoot(r.ID)
	}
	return resp, err
}

// reconcileOutOfBandMerge marks r as merged when its FinalCommit is
// already an ancestor of `target` — i.e. the run's storage branch was
// merged into the target outside the studio (git CLI, CI, cherry-pick).
// It mutates r in place and persists the corrected record so the
// run-view stops offering a redundant "Squash and merge". A "" target
// resolves against the repo's current branch. Returns (true, nil) when
// it reconciled, (false, nil) when there's nothing to do, and
// (false, err) when the persist failed — the caller decides whether
// that is fatal (PerformMerge) or best-effort (snapshot read).
// reconcileOutOfBandMergeClaimed is the under-claim variant: the exit
// is scoped to the caller's claim token.
func (s *Service) reconcileOutOfBandMergeClaimed(ctx context.Context, r *store.Run, repoRoot, target string, claimToken time.Time) (bool, error) {
	return s.reconcileOutOfBand(ctx, r, repoRoot, target, []store.MergeStatus{store.MergeStatusMerging}, claimToken)
}

func (s *Service) reconcileOutOfBandMerge(ctx context.Context, r *store.Run, repoRoot, target string, expectedFrom []store.MergeStatus) (bool, error) {
	return s.reconcileOutOfBand(ctx, r, repoRoot, target, expectedFrom, time.Time{})
}

func (s *Service) reconcileOutOfBand(ctx context.Context, r *store.Run, repoRoot, target string, expectedFrom []store.MergeStatus, claimToken time.Time) (bool, error) {
	if r == nil || r.FinalCommit == "" {
		return false, nil
	}
	if repoRoot == "" {
		return false, nil
	}
	if target == "" {
		target = resolveMergeTargetForPersistence("", repoRoot)
	}
	if target == "" || !gitlib.IsAncestor(repoRoot, r.FinalCommit, target) {
		return false, nil
	}
	r.MergedCommit = r.FinalCommit
	r.MergedInto = target
	r.MergeStatus = store.MergeStatusMerged
	r.PendingMergeMessage = ""
	r.PendingMergeInto = ""
	upd := mergeUpdateFromRun(r)
	upd.ExpectClaimedAt = claimToken
	changed, err := s.store.UpdateRunMergeIf(ctx, r.ID, upd, expectedFrom)
	if err != nil {
		return false, fmt.Errorf("runview: persist out-of-band merge reconcile for %s: %w", r.ID, err)
	}
	if !changed {
		// Someone else moved the merge state while we were looking —
		// their transition wins; report "nothing reconciled".
		return false, nil
	}
	if s.logger != nil {
		s.logger.Info("runview: run %s already merged out-of-band into %s (FinalCommit %s is an ancestor) — reconciled merge_status=merged", r.ID, target, r.FinalCommit)
	}
	return true, nil
}

// CommitAndFinalizeResponse echoes the persisted state after a
// successful commit-and-finalize, so the studio can update its
// snapshot and pivot to the standard /merge UX without an extra GET.
type CommitAndFinalizeResponse struct {
	RunID         string              `json:"run_id"`
	FinalCommit   string              `json:"final_commit"`
	FinalBranch   string              `json:"final_branch"`
	MergeStatus   store.MergeStatus   `json:"merge_status"`
	MergedInto    string              `json:"merged_into,omitempty"`
	MergedCommit  string              `json:"merged_commit,omitempty"`
	MergeStrategy store.MergeStrategy `json:"merge_strategy,omitempty"`
	// SourceIssueID is set when the run was dispatcher-spawned. The HTTP
	// handler reads it to fire the merged-issue auto-transition when an
	// auto-FF landed the merge inside finalize (MergeStatus="merged"),
	// without a second LoadRun round-trip. Internal-only — omitted from
	// the JSON wire.
	SourceIssueID string `json:"-"`
}

// CommitAndFinalizeCtx commits a run's uncommitted workdir changes
// with the operator-supplied message, then promotes the new HEAD
// onto a persistent branch via the standard finalize path. The
// resulting state is identical to a clean bot-side commit + run
// completion, so the existing /merge endpoint takes over from there.
//
// Preconditions:
//   - run must be a worktree run (worktree=true on run.json).
//   - run.FinalBranch must be empty (already-finalized runs go
//     through /merge directly).
//   - workdir must be dirty (no-op rejected; the operator should
//     use /merge if the run finalized cleanly).
//
// Errors propagate as-is — handlers translate them to 409 for the
// expected guard-rejection cases.
func (s *Service) CommitAndFinalizeCtx(ctx context.Context, runID, message string) (*CommitAndFinalizeResponse, error) {
	if runID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := runtime.CommitUncommittedAndFinalize(ctx, s.store, r, message, s.logger); err != nil {
		return nil, err
	}
	// Re-read so we return the just-persisted state (RecoverFinalize
	// writes via SaveRun under its own context; we want the canonical
	// post-write snapshot to send back).
	r, err = s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	// RecoverFinalize promotes the worktree HEAD with mergeInto:"none",
	// which lands MergeStatus="skipped". For a commit-and-finalize the
	// operator's intent is explicitly "I want to merge this" — but the
	// studio reads finished+skipped as "operator opted out of the merge
	// at launch" and renders a dead-end notice with no merge button.
	// Flip skipped → pending here so the standard squash/merge footer
	// surfaces. (Only when not already merged — defensive against an
	// auto-FF path having landed it.)
	if r.MergeStatus == store.MergeStatusSkipped && r.MergedInto == "" {
		r.MergeStatus = store.MergeStatusPending
		if changed, err := s.store.UpdateRunMergeIf(ctx, r.ID, mergeUpdateFromRun(r), []store.MergeStatus{store.MergeStatusSkipped}); err != nil {
			return nil, fmt.Errorf("runview: persist pending merge status: %w", err)
		} else if !changed && s.logger != nil {
			s.logger.Warn("runview: skipped→pending flip for %s lost a race — leaving the concurrent state", r.ID)
		}
	}
	return &CommitAndFinalizeResponse{
		RunID:         r.ID,
		FinalCommit:   r.FinalCommit,
		FinalBranch:   r.FinalBranch,
		MergeStatus:   r.MergeStatus,
		MergedInto:    r.MergedInto,
		MergedCommit:  r.MergedCommit,
		MergeStrategy: r.MergeStrategy,
		SourceIssueID: sourceIssueID(r),
	}, nil
}

// sourceIssueID returns r.Source.IssueID without the nil-pointer dance
// every caller would otherwise have to write. Empty for non-dispatcher
// runs.
func sourceIssueID(r *store.Run) string {
	if r == nil || r.Source == nil {
		return ""
	}
	return r.Source.IssueID
}

// resolveMergeTargetForPersistence resolves the merge target into a
// branch name so PendingMergeInto can be recorded for the finalize
// path. "" / "current" → currently-checked-out branch; anything else
// passes through. Returns "" if the resolution fails; callers should
// fall back to current-branch lookup at finalize time.
func resolveMergeTargetForPersistence(target, repoRoot string) string {
	if target != "" && target != "current" {
		return target
	}
	out, err := runtime.GitSymbolicRef(repoRoot)
	if err != nil {
		return ""
	}
	return out
}

// ---------------------------------------------------------------------------
// Merge conflict resolution
// ---------------------------------------------------------------------------

// MergeConflictsResponse is the payload returned by GetMergeConflicts.
// Files lists each conflicted path with its current worktree content
// + parsed hunks; Merging signals whether `MERGE_HEAD` / `SQUASH_MSG`
// indicates an in-progress merge (vs. a stale conflicted file the
// operator partially resolved before crashing).
type MergeConflictsResponse struct {
	Files []runtime.ConflictFile `json:"files"`
	// PendingMessage is the squash commit message that was passed to
	// the original merge attempt — preserved so the finalize path can
	// reuse it without recomputing (the user can still override).
	PendingMessage string `json:"pending_message,omitempty"`
	// PendingMergeInto is the target branch the original merge
	// targeted; finalize must run with the same target.
	PendingMergeInto string `json:"pending_merge_into,omitempty"`
}

// GetMergeConflicts inspects the worktree associated with runID and
// returns the current conflict state. Returns (nil, nil) when the
// run's merge_status is not "conflicted" — callers should treat that
// as "no conflicts pending".
func (s *Service) GetMergeConflicts(ctx context.Context, runID string) (*MergeConflictsResponse, error) {
	r, repoRoot, err := s.loadRunForMerge(ctx, runID)
	if err != nil {
		return nil, err
	}
	det, err := runtime.ParseConflicts(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("parse conflicts: %w", err)
	}
	// When the persisted status disagrees with the live worktree — e.g.
	// r.MergeStatus == store.MergeStatusConflicted but det.Files is now
	// empty because the operator resolved manually via the CLI — we
	// deliberately don't auto-finalize run.json: the operator may not
	// have run `git commit` yet. Surface the empty list below and let
	// the UI drive the finalize.
	return &MergeConflictsResponse{
		Files:            det.Files,
		PendingMessage:   r.PendingMergeMessage,
		PendingMergeInto: r.PendingMergeInto,
	}, nil
}

// ResolveMergeConflictFile writes resolved content for one conflicted
// file and stages it via `git add`. Validates that path is currently
// in the unmerged set (no arbitrary writes), but tolerates the file
// being already-staged (idempotent re-resolve).
func (s *Service) ResolveMergeConflictFile(ctx context.Context, runID, path, content string) error {
	if runID == "" {
		return errors.New("runview: run_id is required")
	}
	if path == "" {
		return errors.New("runview: path is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	if err := requireConflict(r, runID); err != nil {
		return err
	}
	repoRoot := mergeRepoRoot(r)
	if repoRoot == "" {
		return fmt.Errorf("run %q has no resolvable repo root", runID)
	}
	paths, err := runtime.UnmergedPaths(repoRoot)
	if err != nil {
		return fmt.Errorf("list unmerged: %w", err)
	}
	if !slices.Contains(paths, path) {
		return fmt.Errorf("path %q is not in the conflict set", path)
	}
	return runtime.StageResolvedFile(repoRoot, path, content)
}

// FinalizeMergeAfterConflict commits the squash merge once every
// conflicted file has been staged. Reuses the pending message stored
// on the run unless the caller supplies an override. On success the
// run.json is updated the same way the conflict-free path would.
func (s *Service) FinalizeMergeAfterConflict(ctx context.Context, runID, messageOverride string) (*MergeResponse, error) {
	r, repoRoot, err := s.loadRunForMerge(ctx, runID)
	if err != nil {
		return nil, err
	}
	if err := requireConflict(r, runID); err != nil {
		return nil, err
	}
	remaining, err := runtime.UnmergedPaths(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("list unmerged: %w", err)
	}
	if len(remaining) > 0 {
		return nil, fmt.Errorf("still unresolved: %v", remaining)
	}
	message := messageOverride
	if message == "" {
		message = r.PendingMergeMessage
	}
	if message == "" {
		message = runtime.BuildSquashMessage(repoRoot, r.BaseCommit, r.FinalCommit, runtime.RunDisplayName(r))
	}

	sha, commitErr := runtime.FinalizeConflictMerge(repoRoot, message)
	if commitErr != nil {
		return nil, commitErr
	}
	target := r.PendingMergeInto
	if target == "" {
		target = resolveMergeTargetForPersistence("current", repoRoot)
	}
	if remote := mergeRepoRoot(r) == "" && r.RepoURL != ""; remote {
		token := ""
		if s.forgeToken != nil {
			if token, err = s.forgeToken(ctx, r); err != nil {
				return nil, fmt.Errorf("forge token for the merge of %s: %w", runID, err)
			}
		}
		if pushErr := s.pushRepoTargetedMerge(ctx, repoRoot, token, target); pushErr != nil {
			return nil, fmt.Errorf("conflict resolved and committed in the server-side clone but the push failed — %s does NOT carry the merge: %w", target, pushErr)
		}
		resp, perr := s.persistMergeSuccess(ctx, r, sha, target, store.MergeStrategySquash, []store.MergeStatus{store.MergeStatusConflicted}, time.Time{})
		if perr == nil {
			s.removeRepoTargetedMergeRoot(r.ID)
		}
		return resp, perr
	}
	return s.persistMergeSuccess(ctx, r, sha, target, store.MergeStrategySquash, []store.MergeStatus{store.MergeStatusConflicted}, time.Time{})
}

// AbortMergeConflict discards the in-progress squash merge: runs
// `git reset --merge` on the repo root and flips merge_status back to
// "failed" so the operator can decide what to do next.
func (s *Service) AbortMergeConflict(ctx context.Context, runID string) error {
	r, repoRoot, err := s.loadRunForMerge(ctx, runID)
	if err != nil {
		return err
	}
	if err := requireConflict(r, runID); err != nil {
		return err
	}
	if err := runtime.AbortConflictMerge(repoRoot); err != nil {
		return err
	}
	r.MergeStatus = store.MergeStatusFailed
	r.PendingMergeMessage = ""
	r.PendingMergeInto = ""
	changed, err := s.store.UpdateRunMergeIf(ctx, r.ID, mergeUpdateFromRun(r), []store.MergeStatus{store.MergeStatusConflicted})
	if mergeRepoRoot(r) == "" && r.RepoURL != "" {
		// Repo-targeted run: the server-side clone is disposable — drop
		// it so a retried merge starts from a fresh checkout. Done even
		// when the CAS below lost: `git reset --merge` already ran, the
		// clone no longer matches any persisted state.
		s.removeRepoTargetedMergeRoot(r.ID)
	}
	if err != nil {
		return fmt.Errorf("runview: persist abort: %w", err)
	}
	if !changed {
		return fmt.Errorf("runview: abort merge for %s: merge state changed concurrently — nothing aborted", r.ID)
	}
	return nil
}

// mergeRepoRoot picks the right repo root for merge operations: the
// persisted RepoRoot when set, otherwise the legacy resolution chain
// the /commits handler uses. Centralised so future moves to a
// dedicated MergeRepoRoot field are a one-line change.
func mergeRepoRoot(r *store.Run) string {
	if r.RepoRoot != "" {
		return r.RepoRoot
	}
	return gitlib.FindRepoRoot(r.WorkDir)
}

// loadRunForMerge fuses the three-step preamble shared by every
// merge-side method: empty-runID rejection, LoadRun, and repo-root
// resolution (with the empty-root rejection). Returns the resolved
// repo root alongside the run so callers don't re-derive it. Error
// values match what each call site previously returned inline.
func (s *Service) loadRunForMerge(ctx context.Context, runID string) (*store.Run, string, error) {
	if runID == "" {
		return nil, "", errors.New("runview: run_id is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, "", err
	}
	repoRoot := mergeRepoRoot(r)
	if repoRoot == "" && r.RepoURL != "" && s.hasRepoTargetedMergeRoot(r.ID) {
		// Repo-targeted run mid-conflict: the server-side merge clone
		// (materialised by PerformMergeCtx) is the working tree.
		repoRoot = s.repoTargetedMergeRoot(r.ID)
	}
	if repoRoot == "" {
		return nil, "", fmt.Errorf("run %q has no resolvable repo root", runID)
	}
	return r, repoRoot, nil
}

// requireConflict guards the three conflict-resolution methods
// (ResolveMergeConflictFile, FinalizeMergeAfterConflict,
// AbortMergeConflict): they only make sense when the run is sitting
// in MergeStatusConflicted. Error message is the exact string the
// inline guards previously produced.
func requireConflict(r *store.Run, runID string) error {
	if r.MergeStatus != store.MergeStatusConflicted {
		return fmt.Errorf("run %q has no pending conflict (merge_status=%q)", runID, r.MergeStatus)
	}
	return nil
}

// persistMergeSuccess is the shared success-tail of PerformMergeCtx
// and FinalizeMergeAfterConflict: stamp the merged fields, clear the
// pending-merge bookkeeping, SaveRun, and build the response. Target
// must already be resolved (callers default differently — Perform
// uses res.MergedInto from the runtime call; Finalize defaults
// PendingMergeInto / current-branch). Strategy is supplied as a
// pre-typed MergeStatus so this helper does no coercion.
func (s *Service) persistMergeSuccess(ctx context.Context, r *store.Run, sha, target string, strategy store.MergeStrategy, expectedFrom []store.MergeStatus, claimToken time.Time) (*MergeResponse, error) {
	r.MergedCommit = sha
	r.MergedInto = target
	r.MergeStrategy = strategy
	r.MergeStatus = store.MergeStatusMerged
	r.PendingMergeMessage = ""
	r.PendingMergeInto = ""
	upd := mergeUpdateFromRun(r)
	upd.ExpectClaimedAt = claimToken
	changed, err := s.store.UpdateRunMergeIf(ctx, r.ID, upd, expectedFrom)
	if err != nil {
		return nil, fmt.Errorf("runview: persist merge result: %w", err)
	}
	if !changed {
		// The git side of the merge landed but the run record moved
		// concurrently (our claim was stolen, or another writer raced).
		// Do NOT overwrite: the next merge attempt reconciles via the
		// out-of-band ancestry check. Surface the split-brain instead
		// of hiding it.
		return nil, fmt.Errorf("runview: merge of %s landed on %s at %s but the run record changed concurrently — record NOT updated (a retried merge heals the record — re-squashing an already-squashed branch is a no-op)", r.ID, target, sha)
	}
	return &MergeResponse{
		MergedCommit:  r.MergedCommit,
		MergedInto:    r.MergedInto,
		MergeStrategy: r.MergeStrategy,
		MergeStatus:   r.MergeStatus,
		SourceIssueID: sourceIssueID(r),
	}, nil
}

// ResolveAllConflictsWithAgent invokes the merge-conflict resolver
// to produce resolved content for every conflicted file at once via
// a direct claw LLM call. The model parameter, when non-empty,
// overrides the detector's pick; format follows claw's
// "<provider>/<model>" spec.
//
// The actual LLM call lives in conflict_agent.go (separate file so
// service_control.go doesn't drag in pkg/backend/model). This stub
// dispatches through resolveAllConflictsWithAgentImpl, which the
// agent file installs on package init.
func (s *Service) ResolveAllConflictsWithAgent(ctx context.Context, runID, model string) (*MergeConflictsResponse, error) {
	if runID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	if resolveAllConflictsWithAgentImpl == nil {
		return nil, ErrAgentResolverNotWired
	}
	return resolveAllConflictsWithAgentImpl(ctx, s, runID, model)
}

// resolveAllConflictsWithAgentImpl is the dispatchable hook. nil
// means the implementation hasn't been installed (e.g. in tests that
// strip out the agent file's init); the stub returns
// ErrAgentResolverNotWired in that case.
var resolveAllConflictsWithAgentImpl func(ctx context.Context, s *Service, runID, model string) (*MergeConflictsResponse, error)

// ErrAgentResolverNotWired signals that no provider credential is
// reachable for the resolver. Detect with errors.Is so future
// generations of this code surface a stable "no creds" signal even
// if the message changes.
var ErrAgentResolverNotWired = errors.New("agent resolver unavailable: no LLM credential detected (sign in via `claude` or `codex` and retry)")
