package dispatcher

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/store"
)

// reconcileParked sweeps cards parked in the awaiting-input column whose
// runs have since reached a terminal status. Every resume surface — CLI
// `iterion resume`, the studio run console, answer-from-board — completes
// the run OUTSIDE the dispatcher (the pause arm's worker returned when the
// run parked), so without this sweep the card strands in awaiting_input
// with its claim retained forever.
//
// finished → CompletedState; hard-failed (non-resumable) → FailedState.
// Operator-owned statuses (paused_*, failed_resumable, cancelled) and in-flight
// ones (running, queued) stay parked — the card genuinely still awaits the
// operator. Native-tracker only (optional-interface seam, same as
// stampLastRun/setAwaitingInput); every call is local disk I/O, safe on
// the actor per the ADR-028 Step 3 boundary.
func (c *Dispatcher) reconcileParked(ctx context.Context) {
	lister, ok := c.tracker.(interface {
		ListAwaitingInput(marker string) ([]tracker.Issue, error)
	})
	if !ok {
		return
	}
	look, ok := c.tracker.(interface {
		LastRunForIssue(id string) (string, error)
	})
	if !ok {
		return
	}
	parked, err := lister.ListAwaitingInput(c.hostMarker)
	if err != nil {
		c.logger.Warn("dispatcher: parked sweep list: %v", err)
		return
	}
	if len(parked) == 0 {
		return
	}
	cfg := c.cfg.Load()
	rs, err := c.openRunStore()
	if err != nil {
		c.logger.Debug("dispatcher: open store for parked sweep: %v", err)
		return
	}
	moved := false
	for _, iss := range parked {
		if _, running := c.state.running[iss.ID]; running {
			continue // a live worker owns it (e.g. mid re-dispatch)
		}
		runID, err := look.LastRunForIssue(iss.ID)
		if err != nil || runID == "" {
			continue
		}
		var target string
		switch c.runStatusFrom(rs, runID) {
		case store.RunStatusFinished:
			target = cfg.Agent.CompletedState
			c.logger.Info("dispatcher: %s resumed out-of-band and finished (run=%s) — moving awaiting-input card to %q", iss.Identifier, runID, target)
		case store.RunStatusFailed:
			target = cfg.Agent.FailedState
			c.logger.Warn("dispatcher: %s resumed out-of-band and failed hard (run=%s) — moving awaiting-input card to %q", iss.Identifier, runID, target)
		default:
			continue // still paused / resumable / in flight — genuinely awaiting the operator
		}
		if target == "" || target == native.StateAwaitingInput {
			continue
		}
		c.setAwaitingInput(iss.ID, false)
		if err := c.tracker.UpdateState(ctx, iss.ID, target); err != nil {
			c.logger.Warn("dispatcher: parked sweep move %s → %q: %v", iss.Identifier, target, err)
			continue
		}
		// Transition FIRST, Release LAST (same ordering as the finish
		// worker): the claim keeps ListCandidates away until the card is
		// in its final, mostly-non-eligible state.
		if err := c.tracker.Release(ctx, iss.ID, c.hostMarker); err != nil {
			c.logger.Warn("dispatcher: parked sweep release %s: %v", iss.Identifier, err)
		}
		c.claims.Remove(iss.ID)
		moved = true
	}
	if moved {
		c.fireSnapshot()
	}
}

// lastRunID is the tracker's persisted last_run pointer, or "" when
// the adapter has no lookup (github/forgejo) or the card has never
// been dispatched. Native-only today, same seam as stampLastRun.
func (c *Dispatcher) lastRunID(issueID string) string {
	look, ok := c.tracker.(interface {
		LastRunForIssue(id string) (string, error)
	})
	if !ok {
		return ""
	}
	runID, err := look.LastRunForIssue(issueID)
	if err != nil || runID == "" {
		return ""
	}
	return runID
}

// reparkToAwaitingInput moves a card whose last_run is still paused
// back to the awaiting-input column, same run id. Caller owns the
// claim (so ListCandidates stays away even if the column is missing).
func (c *Dispatcher) reparkToAwaitingInput(iss tracker.Issue, runID string) bool {
	// A retry can coexist with the persisted pause when the run was
	// resumed out-of-band while its dispatcher retry timer was pending.
	// Re-parking supersedes that stale retry: leaving it behind would keep
	// a phantom row in the dashboard and reuse its attempt on a later run.
	if retry, ok := c.state.retries[iss.ID]; ok {
		if retry.Timer != nil {
			retry.Timer.Stop()
		}
		delete(c.state.retries, iss.ID)
	}
	c.setAwaitingInput(iss.ID, true)
	moved := c.moveToAwaitingInput(iss.ID, iss.Identifier)
	if !moved {
		return false
	}
	c.logger.Info(
		"dispatcher: %s last run %s is still paused — re-parked in awaiting-input, refusing a fresh run",
		iss.Identifier, runID,
	)
	return true
}

// reparkClaimedIfLastRunWaiting stops a fresh dispatch when last_run is
// already paused for a human or the operator. The ticket must already
// be claimed by this tick (so ListCandidates stays away). Returns true
// when the caller must abort — the card is parked again, same run.
// Only dispatcher-owned runs are re-parked (see isDispatcherPausedRun);
// other paused runs still block the mint via lastRunForbidsFresh in
// resolveRunID, just without touching the card.
func (c *Dispatcher) reparkClaimedIfLastRunWaiting(iss tracker.Issue) bool {
	runID := c.lastRunID(iss.ID)
	if runID == "" {
		return false
	}
	rs, err := c.openRunStore()
	if err != nil {
		c.logger.Debug("dispatcher: open store for re-park check: %v", err)
		return false
	}
	r, err := c.loadRunForDecision(rs, runID, "re-park check")
	if err != nil {
		return false
	}
	if !isDispatcherPausedRun(r) {
		return false
	}
	if !c.reparkToAwaitingInput(iss, runID) {
		// No awaiting-input column: the retained claim keeps the paused card
		// safe, while the skip explains why it is held in its current lane.
		c.recordDispatchSkip(iss, "last run "+runID+" is still paused — held in place because the awaiting-input move failed")
	}
	c.fireSnapshot()
	return true
}

// reconcileStrandedPaused re-parks eligible cards whose last_run is
// still paused for a human or the operator. The usual case is a
// studio reboot: SweepStaleClaims dropped the park-claim, in_progress
// is eligible, and without this sweep the next dispatch would mint a
// sibling from entry. Also catches an out-of-band `iterion resume`
// that re-paused on a later human node — the dispatcher worker had
// already returned, so finishRun never moved the card.
//
// Only dispatcher-owned runs are re-parked (see isDispatcherPausedRun):
// a pipelines-launched run belongs to the admission sweep's state
// machine, which only reconciles in_progress tickets — exiling its
// card to awaiting_input would strand it there after the answer.
//
// Runs on every tick, including while paused: reclaim is re-park,
// never a fresh run.
func (c *Dispatcher) reconcileStrandedPaused(ctx context.Context) {
	lister, ok := c.tracker.(interface {
		ListForRepark(marker string) ([]tracker.Issue, error)
	})
	if !ok {
		return
	}
	cards, err := lister.ListForRepark(c.hostMarker)
	if err != nil {
		c.logger.Warn("dispatcher: stranded-pause sweep list: %v", err)
		return
	}
	if len(cards) == 0 {
		return
	}
	rs, err := c.openRunStore()
	if err != nil {
		c.logger.Debug("dispatcher: open store for stranded-pause sweep: %v", err)
		return
	}
	moved := false
	for _, iss := range cards {
		if _, running := c.state.running[iss.ID]; running {
			continue
		}
		runID := c.lastRunID(iss.ID)
		if runID == "" {
			continue
		}
		r, err := rs.LoadRun(ctx, runID)
		if err != nil || !isDispatcherPausedRun(r) {
			continue
		}
		// Claim before the column move so a concurrent tick cannot
		// dispatch if awaiting_input is missing on a custom board.
		c.claims.Record(claimEntry{
			IssueID: iss.ID, Identifier: iss.Identifier,
			Marker: c.hostMarker, ClaimedAt: time.Now().UTC(),
		})
		if err := c.tracker.Claim(ctx, iss.ID, c.hostMarker); err != nil {
			c.claims.Remove(iss.ID)
			if !errors.Is(err, tracker.ErrClaimConflict) {
				c.logger.Warn("dispatcher: stranded-pause claim %s: %v", iss.Identifier, err)
			}
			continue
		}
		if c.reparkToAwaitingInput(iss, runID) {
			moved = true
			continue
		}
		// The claim is load-bearing when the board cannot represent the
		// awaiting-input column: releasing it would make the paused card a
		// dispatch candidate again. Keep it, but surface the in-place park
		// in the dashboard instead of leaving an invisible claimed card.
		c.recordDispatchSkip(iss, "last run "+runID+" is still paused — held in place because the awaiting-input move failed")
	}
	if moved {
		c.fireSnapshot()
	}
}

// isDispatcherPausedRun reports whether the run sits on a human/operator
// pause AND belongs to the dispatcher (RunSource stamps every dispatch).
// A run launched from the pipelines control center or the studio console
// (Source nil or another kind) is left to its owner's state machine —
// re-parking its card would only move the problem.
func isDispatcherPausedRun(r *store.Run) bool {
	if r == nil {
		return false
	}
	if r.Status != store.RunStatusPausedWaitingHuman && r.Status != store.RunStatusPausedOperator {
		return false
	}
	return r.Source != nil && r.Source.Kind == store.RunSourceKindDispatcher
}

// openRunStore builds the filesystem run store once for a sweep —
// store.New does MkdirAll + gitignore housekeeping, too expensive to
// repeat per card on the actor goroutine every tick.
func (c *Dispatcher) openRunStore() (*store.FilesystemRunStore, error) {
	if c.storeDir == "" {
		return nil, errors.New("no store dir")
	}
	s, err := store.New(c.storeDir, store.WithLogger(c.logger))
	// The choke point every resume/park/status decision reads through: an
	// unreachable store silently degrades all of them to "no information",
	// so the episode must be visible at production log levels.
	if err != nil {
		c.warnDegraded("run-store", "dispatcher: cannot open the run store at %s: %v — resume/park/status decisions degraded to 'no information' until it recovers", c.storeDir, err)
		return nil, err
	}
	c.clearDegraded("run-store")
	return s, nil
}

// loadRunForDecision reads a run whose status is about to change a
// dispatch decision. A MISSING record stays Debug — a pruned run is
// normal and self-describing; any other read error means the decision
// is being taken blind, which warns once per run until it reads again.
func (c *Dispatcher) loadRunForDecision(s *store.FilesystemRunStore, runID, what string) (*store.Run, error) {
	r, err := s.LoadRun(context.Background(), runID)
	key := "run-read:" + runID
	switch {
	case err == nil:
		c.clearDegraded(key)
		return r, nil
	case errors.Is(err, store.ErrRunNotFound):
		c.logger.Debug("dispatcher: run %s not found (%s)", runID, what)
		return nil, err
	default:
		c.warnDegraded(key, "dispatcher: cannot read run %s (%s): %v — deciding as if it held no information", runID, what, err)
		return nil, err
	}
}

// warnDegraded logs a decision-changing degradation at Warn once per
// episode (keyed); repeats stay Debug until clearDegraded closes the
// episode. Actor-goroutine-owned state, like every other c.state map.
func (c *Dispatcher) warnDegraded(key, format string, args ...any) {
	if c.state.degradedWarned[key] {
		c.logger.Debug(format, args...)
		return
	}
	c.state.degradedWarned[key] = true
	c.logger.Warn(format, args...)
}

// clearDegraded closes a degradation episode opened by warnDegraded,
// logging the recovery once so the two log lines bracket the episode.
func (c *Dispatcher) clearDegraded(key string) {
	if !c.state.degradedWarned[key] {
		return
	}
	delete(c.state.degradedWarned, key)
	c.logger.Info("dispatcher: %s degradation recovered", key)
}

// runStatusFrom reads a run's persisted status from an already-open
// store. Best-effort — any read error returns the empty status, which
// the sweeps treat as "leave the card alone". A missing record stays
// Debug (a pruned run is normal); any other read error is a real
// degradation and warns once per run.
func (c *Dispatcher) runStatusFrom(s *store.FilesystemRunStore, runID string) store.RunStatus {
	r, err := c.loadRunForDecision(s, runID, "status check")
	if err != nil {
		return ""
	}
	return r.Status
}

// runStatusOnDisk reads a run's persisted status straight from the store.
// Best-effort — any read error returns the empty status, which the guard
// treats as "leave the card alone". One-shot callers only; sweeps open
// the store once and use runStatusFrom.
func (c *Dispatcher) runStatusOnDisk(runID string) store.RunStatus {
	if runID == "" || c.storeDir == "" {
		return ""
	}
	s, err := c.openRunStore()
	if err != nil {
		c.logger.Debug("dispatcher: open store for status check: %v", err)
		return ""
	}
	return c.runStatusFrom(s, runID)
}
