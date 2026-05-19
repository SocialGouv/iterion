package runview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	switch r.Status {
	case store.RunStatusPausedWaitingHuman, store.RunStatusFailedResumable:
		// flippable
	default:
		return false, nil // already terminal — no-op
	}
	if err := s.store.UpdateRunStatus(ctx, runID, store.RunStatusCancelled, "cancelled by operator (was "+string(r.Status)+")"); err != nil {
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
		ID:        newQueuedMessageID(),
		Text:      text,
		TenantID:  r.TenantID,
		SkillRefs: cfg.skillRefs,
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
	skillRefs []string
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
// Time-prefix gives FIFO-friendly ordering at the filesystem level
// even when wall-clock collides; the random suffix avoids ID reuse
// within the same nanosecond.
func newQueuedMessageID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// rand.Read effectively never fails on Linux; on the off
		// chance, fall back to the timestamp alone — collisions are
		// caught at AppendQueuedMessage (would clobber the FS row).
		return fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("msg_%d_%s", time.Now().UnixNano(), hex.EncodeToString(buf[:]))
}

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
	repoRoot := r.RepoRoot
	if repoRoot == "" {
		// Mid-vintage runs may lack RepoRoot; fall back through the
		// same chain runs_files.go uses.
		repoRoot = gitlib.FindRepoRoot(r.WorkDir)
	}
	if repoRoot == "" {
		return nil, fmt.Errorf("run %q has no resolvable repo root", runID)
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
		// Persist the failure so the studio can show "Retry merge".
		r.MergeStatus = store.MergeStatusFailed
		if saveErr := s.store.SaveRun(ctx, r); saveErr != nil && s.logger != nil {
			s.logger.Warn("runview: persist merge failure for %s: %v", runID, saveErr)
		}
		return nil, mergeErr
	}

	// Success: persist the new state.
	r.MergedCommit = res.MergedCommit
	r.MergedInto = res.MergedInto
	r.MergeStrategy = store.MergeStrategy(res.Strategy)
	r.MergeStatus = store.MergeStatusMerged
	if err := s.store.SaveRun(ctx, r); err != nil {
		return nil, fmt.Errorf("runview: persist merge result: %w", err)
	}

	return &MergeResponse{
		MergedCommit:  r.MergedCommit,
		MergedInto:    r.MergedInto,
		MergeStrategy: r.MergeStrategy,
		MergeStatus:   r.MergeStatus,
	}, nil
}
