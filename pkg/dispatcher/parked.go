package dispatcher

import (
	"context"

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
