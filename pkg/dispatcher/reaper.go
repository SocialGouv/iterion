package dispatcher

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The claim reaper: the PERIODIC, cross-host watchdog the boot-time
// same-host pid-probe sweep never was. A claimed card whose lease ran
// out with nobody renewing is, by construction, a card whose owner died
// (live owners heartbeat at lease/3): the reaper resolves its run,
// consults the ONE decision table (DecideStuckCard — liveness-first,
// conserve on any doubt), and for actionable rows CAS-TRANSFERS the
// claim to a recovery owner before touching anything. Never
// clear-then-decide: an eligible-state card freed before its
// disposition is decided is instantly re-dispatchable by the very tick
// this reaper cleans up after.
const (
	// claimReaperEnv is the rollout gate (expand/contract): release N
	// ships the lease fields + heartbeats with the reaper OFF, so a
	// mixed fleet can never reap a claim an old binary silently
	// un-leased; release N+1 (old binaries drained) turns it on.
	claimReaperEnv = "ITERION_BOARD_CLAIM_REAPER"

	claimReaperInterval = time.Minute
	claimReaperBatch    = 100
)

// ClaimReaperEnabled reads the fleet gate (shared with the cloud board
// dispatcher's reaper — one switch, both surfaces).
func ClaimReaperEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(claimReaperEnv)), "on")
}

// startClaimReaper launches the periodic reaper when the gate is on and
// the tracker carries both halves of the capability. The refusals are
// LOGGED — a declared watchdog that silently isn't running is the
// failure mode this whole chantier exists to end.
func (c *Dispatcher) startClaimReaper() {
	if !ClaimReaperEnabled() {
		return
	}
	reaper, ok := c.tracker.(tracker.ClaimReaper)
	if !ok || c.leaser == nil {
		c.logger.Warn("dispatcher: %s=on but tracker %T has no claim-lease/reaper capability — the claim watchdog stays off for this tracker", claimReaperEnv, c.tracker)
		return
	}
	c.logger.Info("dispatcher: claim watchdog active (every %s — expired leases are reclaimed and routed by the decision table)", claimReaperInterval)
	errtrack.Go("dispatcher.claimReaper", func() {
		t := time.NewTicker(claimReaperInterval)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				c.reapExpiredClaims(context.Background(), reaper, time.Now().UTC())
			}
		}
	})
}

// reapExpiredClaims performs one pass. Exported to the test via the
// direct call with an injected `now` — the lease is time-based and the
// pass must be provable without waiting one out.
func (c *Dispatcher) reapExpiredClaims(ctx context.Context, reaper tracker.ClaimReaper, now time.Time) {
	cands, err := reaper.ListExpiredClaimCandidates(ctx, now, claimReaperBatch)
	if err != nil {
		c.logger.Warn("dispatcher: claim watchdog list: %v", err)
		return
	}
	if len(cands) == 0 {
		return
	}
	// The reaper runs OFF the actor, so it opens its own run store and
	// never touches the actor-owned degraded-episode bookkeeping the
	// shared openRunStore/loadRunForDecision helpers maintain (an
	// unsynchronized map — using them here was a data race, caught by
	// the reaper's own first test).
	var runs *store.FilesystemRunStore
	if c.storeDir != "" {
		if s, serr := store.New(c.storeDir, store.WithLogger(c.logger)); serr != nil {
			c.logger.Warn("dispatcher: claim watchdog cannot open the run store: %v — every candidate conserves this pass", serr)
		} else {
			runs = s
		}
	}
	for _, cand := range cands {
		c.reapOne(ctx, reaper, runs, cand, now)
	}
}

func (c *Dispatcher) reapOne(ctx context.Context, reaper tracker.ClaimReaper, runs *store.FilesystemRunStore, cand tracker.ExpiredClaim, now time.Time) {
	run, runErr := c.loadRunForReap(ctx, runs, cand.LastRunID)
	dec := DecideStuckCard(run, runErr)
	if dec.Action == StuckKeep {
		c.logger.Debug("dispatcher: claim watchdog keeps %s: %s", cand.Identifier, dec.Reason)
		return
	}
	// TRANSFER first (the F9 order): the CAS re-verifies the claim is
	// still exactly what we listed AND still expired — anything that
	// moved on (a renewal, an operator, another replica's reaper) makes
	// this a clean skip.
	tok, err := reaper.ReclaimExpired(ctx, cand.IssueID, cand.Prev, "reaper:"+c.hostMarker, now)
	if err != nil {
		if !errors.Is(err, tracker.ErrClaimConflict) {
			c.logger.Warn("dispatcher: claim watchdog reclaim %s: %v", cand.Identifier, err)
		}
		return
	}
	cfg := c.cfg.Load()
	switch dec.Action {
	case StuckComplete:
		if cfg.Agent.CompletedState != "" {
			if err := c.leaser.UpdateStateOwned(ctx, cand.IssueID, cfg.Agent.CompletedState, tok); err != nil {
				c.logger.Warn("dispatcher: claim watchdog file %s → %s: %v", cand.Identifier, cfg.Agent.CompletedState, err)
			}
		}
	case StuckFail:
		if cfg.Agent.FailedState != "" {
			if err := c.leaser.UpdateStateOwned(ctx, cand.IssueID, cfg.Agent.FailedState, tok); err != nil {
				c.logger.Warn("dispatcher: claim watchdog file %s → %s: %v", cand.Identifier, cfg.Agent.FailedState, err)
			}
		}
	case StuckRepark, StuckReleaseOnly:
		// The release below is the whole action: the card re-enters the
		// eligible pool, and for Repark the retry machinery resumes the
		// RECORDED run (resolveRunID + lastRunHoldBeforeClaim), never a
		// fresh sibling.
	}
	if err := c.leaser.ReleaseOwned(ctx, cand.IssueID, tok); err != nil {
		c.logger.Warn("dispatcher: claim watchdog release %s: %v", cand.Identifier, err)
	}
	// Warn on purpose: a reclaim is an incident's closing bracket — the
	// owner died holding this card — and must be visible at production
	// log levels (the Debug-decline lesson).
	c.logger.Warn("dispatcher: claim watchdog reclaimed %s from %q (%s → %s): %s",
		cand.Identifier, cand.Prev.Marker, cand.State, dec.Action, dec.Reason)
}

// loadRunForReap resolves the card's recorded run for the decision
// table, running the orphan-promotion oracle first so a dead `running`
// run is classified by what it truly is (promoteIfOrphaned only ever
// promotes when it can PROVE no live owner holds the run lock).
func (c *Dispatcher) loadRunForReap(ctx context.Context, runs *store.FilesystemRunStore, runID string) (*store.Run, error) {
	if runID == "" {
		return nil, nil
	}
	if runs == nil {
		return nil, errors.New("run store unavailable")
	}
	r, err := runs.LoadRun(ctx, runID)
	if err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			// A recorded run whose document is gone (pruned) proves
			// nothing is alive — same disposition as "no run".
			return nil, nil
		}
		return nil, err
	}
	if r.Status == store.RunStatusRunning {
		if promoted := c.promoteIfOrphaned(ctx, runs, r); promoted != r.Status {
			return runs.LoadRun(ctx, runID)
		}
	}
	return r, nil
}
