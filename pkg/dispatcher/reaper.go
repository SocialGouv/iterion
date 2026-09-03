package dispatcher

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
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

	// reaperMarkerPrefix tags a recovery claim so it reads as the
	// watchdog's in logs and events. isStaleLocalMarker strips it: a
	// reaper that dies holding a card must be sweepable like any other
	// dead owner, or disabling the gate would strand its cards forever.
	reaperMarkerPrefix = tracker.ReaperMarkerPrefix
)

// ClaimReaperInterval is the watchdog's cadence, exported so the cloud
// board dispatcher paces its own pass by the SAME constant instead of
// inheriting whatever its dispatch tick happens to be.
func ClaimReaperInterval() time.Duration { return claimReaperInterval }

// ClaimReaperEnvName is the fleet-gate env var, exported so the cloud
// board dispatcher references the one constant instead of re-literalling
// the name in its startup log.
func ClaimReaperEnvName() string { return claimReaperEnv }

// ReaperMarkerPrefix is the marker namespace a watchdog claims under,
// exported so a store can SELECT that population rather than filter it
// out of a capped batch — a recovery claim carries a FRESH lease, so it
// sorts last among expired claims and a post-hoc filter never sees one.
func ReaperMarkerPrefix() string { return reaperMarkerPrefix }

// IsReaperMarker reports whether a claim marker was minted by a
// watchdog. It is the persisted record that a card was already conserved
// once — the only bound available on a decision that must otherwise be
// re-taken from scratch every lease.
func IsReaperMarker(marker string) bool { return tracker.IsReaperMarker(marker) }

// ReaperMarker builds the watchdog's recovery-claim marker for a host.
// Exported so the cloud reaper uses the SAME shape the local boot sweep
// knows how to strip — a recopied literal is how the two drift apart.
func ReaperMarker(host string) string { return reaperMarkerPrefix + host }

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
	// The latch is fed ONCE, with the whole pass's verdict. Feeding it at
	// two sites (store-open clears, per-card read sets) made it FLAP on a
	// board with one unreadable run — two warns per pass for ever, one of
	// them announcing a recovery nothing observed: the very storm the
	// latch exists to prevent.
	var runs *store.FilesystemRunStore
	var passErr error // the pass's run-read verdict
	observed := false // did anything consult the store this pass?
	if c.storeDir != "" {
		if s, serr := store.New(c.storeDir, store.WithLogger(c.logger)); serr != nil {
			passErr, observed = serr, true
		} else {
			runs = s
		}
	}
	for _, cand := range cands {
		obs, rerr := c.reapOne(ctx, reaper, runs, cand, now)
		if obs {
			observed = true
			if rerr != nil && passErr == nil {
				passErr = rerr
			}
		}
	}
	// Only a pass that touched the store reports on its health: a board
	// whose claimed cards carry no run observed nothing, so it neither
	// clears nor sets the latch (a clear there would announce a recovery
	// nothing measured).
	if observed {
		c.noteRunReadFailure(passErr)
	}
}

// reapOne judges one candidate. Returns whether the run store was
// consulted and what the read said — the CALLER folds those into the
// pass-level latch verdict; noting per card is what made the latch flap.
func (c *Dispatcher) reapOne(ctx context.Context, reaper tracker.ClaimReaper, runs *store.FilesystemRunStore, cand tracker.ExpiredClaim, now time.Time) (runObserved bool, runReadErr error) {
	run, runErr := c.loadRunForReap(ctx, runs, cand.LastRunID)
	runObserved, runReadErr = cand.LastRunID != "", runErr
	cfg := c.cfg.Load()
	card := StuckCard{
		State: cand.State, RunningState: cfg.Agent.RunningState, LaunchStates: c.launchStates(),
		StampWindowOpen: StampWindowOpen(cand.ClaimedAt, now),
	}
	// PRE-transfer: only the rows that protect a live owner. The parked
	// row is deliberately not consulted here — refusing the transfer would
	// make its own bound unreachable and leave the card held for ever.
	if pre := DecideTransfer(run, runErr, card); pre.Action == StuckKeep {
		c.logger.Debug("dispatcher: claim watchdog keeps %s: %s", cand.Identifier, pre.Reason)
		return
	}
	var dec StuckDecision
	// TRANSFER first (the F9 order): the CAS re-verifies the claim is
	// still exactly what we listed AND still expired — anything that
	// moved on (a renewal, an operator, another replica's reaper) makes
	// this a clean skip.
	tok, liveState, err := reaper.ReclaimExpired(ctx, cand.IssueID, cand.Prev, reaperMarkerPrefix+c.hostMarker, now)
	if err != nil {
		if !errors.Is(err, tracker.ErrClaimConflict) {
			c.logger.Warn("dispatcher: claim watchdog reclaim %s: %v", cand.Identifier, err)
		}
		return
	}
	// The transfer is the first moment state and ownership are known
	// TOGETHER, so the decision is re-taken on what it saw. The listing's
	// copy only ever selected a candidate; every rule that reads the card
	// (the anti-double-launch one, the parked-out-of-pool one) must judge
	// this value or it is judging a card that no longer exists.
	card.State = liveState
	if dec = DecideStuckCard(run, runErr, card); dec.Action == StuckKeep {
		c.keepAfterTransfer(ctx, cand, tok, dec)
		return
	}
	switch dec.Action {
	case StuckComplete:
		c.fileStuckCard(ctx, cand, card, cfg.Agent.CompletedState, tok, tracker.ReasonRunFinished)
	case StuckFail:
		c.fileStuckCard(ctx, cand, card, cfg.Agent.FailedState, tok, tracker.ReasonRunFailed)
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
	return
}

// fileStuckCard performs the terminal filing half of a disposition,
// under the recovery token, gated by the shared ShouldFileStuckCard
// predicate (the cloud reaper reads the same one). Failures are logged,
// never fatal: the claim is released either way, so a card is never left
// owned by a dead worker's ghost.
func (c *Dispatcher) fileStuckCard(ctx context.Context, cand tracker.ExpiredClaim, card StuckCard, target string, tok tracker.ClaimToken, reason string) {
	if !ShouldFileStuckCard(card.State, card.RunningState, target, card.LaunchStates) {
		if target != "" && target != card.RunningState && card.State != target {
			c.logger.Info("dispatcher: claim watchdog leaves %s in %q (moved out of %q deliberately — not overwriting it with %q)",
				cand.Identifier, card.State, card.RunningState, target)
		}
		return
	}
	// A TERMINAL filing carries the run's own verdict (run_finished /
	// run_failed — descriptive, non-machine): the card's downstream chain
	// must fire exactly as it would have for the living owner. Only the
	// reparks stay under the machine watchdog reason. A leaser without
	// the reasoned form falls back to the marker-derived one — the
	// conformance canary is what keeps both twins honest.
	if rr, ok := c.leaser.(interface {
		UpdateStateOwnedReason(ctx context.Context, id, newState string, tok tracker.ClaimToken, reason string) error
	}); ok && reason != "" {
		if err := rr.UpdateStateOwnedReason(ctx, cand.IssueID, target, tok, reason); err != nil {
			c.logger.Warn("dispatcher: claim watchdog file %s → %s: %v", cand.Identifier, target, err)
		}
		return
	}
	if err := c.leaser.UpdateStateOwned(ctx, cand.IssueID, target, tok); err != nil {
		c.logger.Warn("dispatcher: claim watchdog file %s → %s: %v", cand.Identifier, target, err)
	}
}

// keepAfterTransfer handles a decision that flipped to Keep once the
// transfer showed the card's real state. The claim is already ours, so
// "keep" has to be enacted rather than assumed — and it must not be
// enacted forever: holding a card no one can dispatch is the same
// outcome, for the operator, as the stuck card the watchdog exists to
// clear. So conservation is granted ONCE. The recovery marker on the
// claim is the record of that grant: finding it means this card was
// already conserved a full lease ago and the reason has not gone away,
// so the claim is released and said out loud.
func (c *Dispatcher) keepAfterTransfer(ctx context.Context, cand tracker.ExpiredClaim, tok tracker.ClaimToken, dec StuckDecision) {
	if !IsReaperMarker(cand.Prev.Marker) {
		c.logger.Warn("dispatcher: claim watchdog holds %s under a recovery claim: %s — re-judged at the next lease",
			cand.Identifier, dec.Reason)
		return
	}
	c.logger.Warn("dispatcher: claim watchdog releases %s after conserving it for a full lease (%s) — "+
		"the reason has not cleared, and holding it any longer only hides the card",
		cand.Identifier, dec.Reason)
	if err := c.leaser.ReleaseOwned(ctx, cand.IssueID, tok); err != nil {
		c.logger.Warn("dispatcher: claim watchdog release %s: %v", cand.Identifier, err)
	}
}

// StampWindowOpen reports whether a run stamp could still plausibly be
// in flight for a claim taken at claimedAt. The stamp is written
// immediately after the launch, so the real window is seconds; a whole
// lease is already generous, and two is the point past which "the stamp
// is late" stops being a credible explanation. A zero claimedAt (a store
// that never recorded one) reads as CLOSED: an unknown age must not
// grant an unbounded hold.
func StampWindowOpen(claimedAt, now time.Time) bool {
	if claimedAt.IsZero() {
		return false
	}
	return now.Sub(claimedAt) < 2*native.ClaimLeaseDuration
}

// noteRunReadFailure reports the run store's health on its EDGES, like
// the cloud twin: one line when reads start failing (every card is
// conserved from there) and one when they recover, never one per card.
func (c *Dispatcher) noteRunReadFailure(err error) {
	if err == nil {
		if c.runReadFailure.Swap(false) {
			c.logger.Warn("dispatcher: claim watchdog can read runs again — cards are being judged rather than conserved")
		}
		return
	}
	if !c.runReadFailure.Swap(true) {
		c.logger.Warn("dispatcher: claim watchdog cannot read runs (%v) — every card is conserved until this clears", err)
	}
}

// launchStates asks the tracker which columns a card is dispatched from
// (tracker.LaunchStateLister). A tracker without the capability returns
// nothing, which keeps the watchdog conservative: it then honours every
// state it did not expect rather than guessing.
func (c *Dispatcher) launchStates() []string {
	if l, ok := c.tracker.(tracker.LaunchStateLister); ok {
		return l.LaunchStates()
	}
	return nil
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
