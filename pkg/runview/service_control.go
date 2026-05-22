package runview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	repoRoot := mergeRepoRoot(r)
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
			if saveErr := s.store.SaveRun(ctx, r); saveErr != nil && s.logger != nil {
				s.logger.Warn("runview: persist merge conflict for %s: %v", runID, saveErr)
			}
			return nil, mergeErr
		}
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
	r.PendingMergeMessage = ""
	r.PendingMergeInto = ""
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
	if runID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	repoRoot := mergeRepoRoot(r)
	if repoRoot == "" {
		return nil, fmt.Errorf("run %q has no resolvable repo root", runID)
	}
	det, err := runtime.ParseConflicts(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("parse conflicts: %w", err)
	}
	// When the persisted status disagrees with the live worktree
	// (e.g. the operator resolved manually via the CLI), refresh
	// run.json so the UI sees the right state on the next refresh.
	if r.MergeStatus == store.MergeStatusConflicted && len(det.Files) == 0 {
		// All conflicts resolved out-of-band. Don't auto-finalize:
		// the operator may not have run `git commit` yet. Surface
		// the empty list and let the UI drive the finalize.
	}
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
	if r.MergeStatus != store.MergeStatusConflicted {
		return fmt.Errorf("run %q has no pending conflict (merge_status=%q)", runID, r.MergeStatus)
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
	if runID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.MergeStatus != store.MergeStatusConflicted {
		return nil, fmt.Errorf("run %q has no pending conflict (merge_status=%q)", runID, r.MergeStatus)
	}
	repoRoot := mergeRepoRoot(r)
	if repoRoot == "" {
		return nil, fmt.Errorf("run %q has no resolvable repo root", runID)
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
	r.MergedCommit = sha
	r.MergedInto = target
	r.MergeStrategy = store.MergeStrategySquash
	r.MergeStatus = store.MergeStatusMerged
	r.PendingMergeMessage = ""
	r.PendingMergeInto = ""
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

// AbortMergeConflict discards the in-progress squash merge: runs
// `git reset --merge` on the repo root and flips merge_status back to
// "failed" so the operator can decide what to do next.
func (s *Service) AbortMergeConflict(ctx context.Context, runID string) error {
	if runID == "" {
		return errors.New("runview: run_id is required")
	}
	r, err := s.store.LoadRun(ctx, runID)
	if err != nil {
		return err
	}
	if r.MergeStatus != store.MergeStatusConflicted {
		return fmt.Errorf("run %q has no pending conflict (merge_status=%q)", runID, r.MergeStatus)
	}
	repoRoot := mergeRepoRoot(r)
	if repoRoot == "" {
		return fmt.Errorf("run %q has no resolvable repo root", runID)
	}
	if err := runtime.AbortConflictMerge(repoRoot); err != nil {
		return err
	}
	r.MergeStatus = store.MergeStatusFailed
	r.PendingMergeMessage = ""
	r.PendingMergeInto = ""
	if err := s.store.SaveRun(ctx, r); err != nil {
		return fmt.Errorf("runview: persist abort: %w", err)
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
