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
// Resumable statuses (paused_*, failed_resumable, cancelled) and in-flight
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
		switch c.runStatusOnDisk(runID) {
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
func (c *Dispatcher) reparkToAwaitingInput(iss tracker.Issue, runID string) {
	c.stampLastRun(iss.ID, &runningEntry{
		IssueID: iss.ID, Identifier: iss.Identifier, RunID: runID,
	})
	c.setAwaitingInput(iss.ID, true)
	c.moveToAwaitingInput(iss.ID, iss.Identifier)
	c.logger.Info(
		"dispatcher: %s last run %s is still paused — re-parked in awaiting-input, refusing a fresh run",
		iss.Identifier, runID,
	)
}

// reparkClaimedIfLastRunWaiting stops a fresh dispatch when last_run is
// already paused for a human or the operator. The ticket must already
// be claimed by this tick (so ListCandidates stays away). Returns true
// when the caller must abort — the card is parked again, same run.
func (c *Dispatcher) reparkClaimedIfLastRunWaiting(iss tracker.Issue) bool {
	runID := c.lastRunID(iss.ID)
	if runID == "" {
		return false
	}
	switch c.runStatusOnDisk(runID) {
	case store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
	default:
		return false
	}
	c.reparkToAwaitingInput(iss, runID)
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
	moved := false
	for _, iss := range cards {
		if _, running := c.state.running[iss.ID]; running {
			continue
		}
		runID := c.lastRunID(iss.ID)
		if runID == "" {
			continue
		}
		switch c.runStatusOnDisk(runID) {
		case store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator:
		default:
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
		c.reparkToAwaitingInput(iss, runID)
		moved = true
	}
	if moved {
		c.fireSnapshot()
	}
}

// runStatusOnDisk reads a run's persisted status straight from the store.
// Best-effort — any read error returns the empty status, which the parked
// sweep treats as "leave the card alone" (mirrors resumableRunID).
func (c *Dispatcher) runStatusOnDisk(runID string) store.RunStatus {
	if runID == "" || c.storeDir == "" {
		return ""
	}
	s, err := store.New(c.storeDir, store.WithLogger(c.logger))
	if err != nil {
		c.logger.Debug("dispatcher: open store for parked sweep: %v", err)
		return ""
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		c.logger.Debug("dispatcher: cannot read run %s for parked sweep: %v", runID, err)
		return ""
	}
	return r.Status
}
