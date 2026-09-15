package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The gate reconciler's net.
//
// The repair next door is driven by ONE run-outcome event on the internal bus.
// That bus is lossy by design and the repo says so in every other place it is
// consumed: usernotify carries a 2-minute reconciliation sweep for exactly the
// episodes it drops, the dispatcher's 30s poll backs its board fast path, and
// the retry sweeper exists because no in-pod timer survives a rollout. The
// merge gate — the one consumer whose miss BLOCKS A PULL REQUEST — had no such
// net: a dropped event, a replica that took the queue-group delivery mid-
// shutdown, or a handler that returned early meant the required check stayed
// absent forever, with the run showing `failed_resumable` and nothing anywhere
// saying a PR was waiting on it.
//
// Observed 2026-08-10: four review runs died on one provider weekly cap within
// 90 seconds; all four PRs kept an absent required check for hours, and the
// reconciler had left no trace of having considered any of them.
//
// So the same run is offered to the same repair twice, from two independent
// paths. reconcileGateForRunID re-reads the live status before it writes, so
// the second offer costs one API read and changes nothing when the first
// worked.

const (
	// gateSweepInterval is the latency an operator waits, worst case, between
	// a review dying and its check turning red. A minute matches the retry and
	// orphan sweepers; the scan is one indexed query over a bounded window.
	gateSweepInterval = 60 * time.Second

	// gateSweepGrace keeps the sweep off runs the event path is still handling.
	// The handler does forge round-trips (get PR, list statuses, post), so a
	// run that ended seconds ago is not yet evidence of a miss — without this
	// the two paths would race on every single run instead of only on the
	// dropped ones.
	gateSweepGrace = 3 * time.Minute

	// gateSweepLookback bounds how far back an ORDINARY pass reaches. It has to
	// exceed the interval by enough to cover a replica restart or a slow pass
	// without a run slipping through the gap between two windows. It is the
	// fast lane only — the horizon past which a run is abandoned is
	// gateSweepHorizon, and gateDeepSweepEvery is what reaches it.
	gateSweepLookback = 60 * time.Minute

	// gateSweepHorizon is how long a dead gating run stays reachable by this
	// net — the single figure every other bound here derives from, including
	// the publish grant's own post-run life (forgePublishPostRunGrace).
	//
	// It is anchored on the longest wait the retry policy itself permits,
	// because the outage class this net exists for is a provider usage window,
	// and a weekly window shuts for DAYS. An hour-long horizon survives a
	// dropped event; it does not survive the thing that kills reviews in
	// batches. Measured 2026-09-15: seven runs died on one weekly cap, two of
	// them gating runs holding valid grants, and their required checks sat
	// `pending` for 81 hours with nothing left to answer them — the hour-long
	// net had closed 80 hours earlier.
	//
	// Unbounded is still wrong, but the danger an earlier bound was written
	// against — posting a synthetic failure onto a pull request that merged or
	// moved on — is not what holds it back: reconcileGateForRunID stands down
	// on a closed/merged pull request, on a head that moved, and on a check
	// that already carries a real verdict, each read live from the forge.
	// What bounds it is the grant: past its life there is nothing to post
	// with.
	gateSweepHorizon = retrypolicy.DefaultMaxWait

	// gateDeepSweepEvery is how many ordinary passes separate two horizon-wide
	// ones. The deep pass is what makes gateSweepHorizon real; the fast pass
	// keeps the ordinary dropped event answered within the minute. Splitting
	// them is what keeps the per-minute scan the size of an hour rather than
	// the size of the horizon.
	gateDeepSweepEvery = 30

	// gateSweepBatch bounds one PAGE. Each candidate can cost a few forge
	// round-trips, and the overwhelming majority are runs that gate nothing
	// and exit on a local field read.
	gateSweepBatch = 200

	// gateSweepMaxPages bounds a pass. The window is paged with the `before`
	// cursor the query was built for, because a single page is not a bound —
	// it is a silent truncation that repeats: the rows come back newest-first,
	// so a dead gating run pushed off the page would be pushed off again on
	// every subsequent pass and never examined at all. (The reused query also
	// returns every currently-paused run with no time bound, which consumes
	// rows without ever being a candidate.) The cap keeps one slow pass from
	// outliving its own ticker.
	gateSweepMaxPages = 10
)

// gateSweepLister is the store capability the sweep scans with — the same
// bounded-window terminal-run query the usernotify sweep uses, reused rather
// than duplicated so both nets share one index. Implemented by the Mongo
// store; the local store has no cloud gate to reconcile and no sweeper.
type gateSweepLister interface {
	ListNotifiableRuns(ctx context.Context, since, before time.Time, limit int) ([]mongostore.NotifiableRunRef, error)
}

// runGateSweeper ticks sweepGates until ctx is cancelled. Started by
// ListenAndServe alongside the event-driven reconciler.
//
// It announces itself for the reason the retry sweeper does: "no PR is stuck"
// and "every stuck PR is invisible" produce identical silence otherwise.
func (s *Server) runGateSweeper(ctx context.Context, lister gateSweepLister) {
	s.infof("merge-gate sweeper: re-offering dead gating runs to the reconciler (every %s, %s grace, %s lookback, %s horizon reached every %d passes) — the net under the lossy outcome event",
		gateSweepInterval, gateSweepGrace, gateSweepLookback, gateSweepHorizon, gateDeepSweepEvery)
	t := time.NewTicker(gateSweepInterval)
	defer t.Stop()
	// The first pass is a deep one: a replica that has just started is exactly
	// the one with no idea what died while nothing was watching, and waiting
	// gateDeepSweepEvery ticks to find out would make a rolling deploy the
	// longest blind window the net has.
	pass := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepGates(ctx, lister, time.Now().UTC(), gateSweepWindowFor(pass))
			pass++
		}
	}
}

// gateSweepWindowFor picks how far back pass number `pass` reaches. Pass 0 —
// the first after a start or a rollout — is deep on purpose: a replica that
// has just come up is precisely the one with no idea what died while nothing
// was watching.
func gateSweepWindowFor(pass int) time.Duration {
	if pass%gateDeepSweepEvery == 0 {
		return gateSweepHorizon
	}
	return gateSweepLookback
}

// sweepGates performs one pass over the window `lookback` reaches back to.
// Extracted (with an injectable clock and window) for tests.
func (s *Server) sweepGates(ctx context.Context, lister gateSweepLister, now time.Time, lookback time.Duration) {
	if lister == nil || s.cfg.Store == nil {
		return
	}
	since := now.Add(-lookback)
	before := now.Add(-gateSweepGrace)
	for page := 0; page < gateSweepMaxPages; page++ {
		refs, err := lister.ListNotifiableRuns(store.WithoutTenantFilter(ctx), since, before, gateSweepBatch)
		if err != nil {
			s.warnf("merge-gate sweeper: scan: %v", err)
			return
		}
		oldest := before
		for _, ref := range refs {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Every guard that decides whether this run owes anything lives in
			// the reconciler; the sweep's only job is to offer the run again.
			// Runs that gate nothing — the vast majority of any window — exit
			// on a local field read with no forge traffic.
			_ = s.reconcileGateForRunID(ctx, ref.ID, gateTriggerSweep)
			// Same net for the auto-fix lane: it consumes the same lossy bus
			// with the same miss modes, and until now had NO second path — a
			// dropped outcome event silently meant no fix pass for a repo
			// that opted in. The offer is idempotent (per-head claim + idem
			// key) and the lane's own guards exclude cancelled/paused/armed
			// runs the reconciler-oriented window also contains. Recovery
			// horizon = gateSweepHorizon, same as the gate's.
			s.autofixOffer(ctx, ref.ID)
			if !ref.UpdatedAt.IsZero() && ref.UpdatedAt.Before(oldest) {
				oldest = ref.UpdatedAt
			}
		}
		if len(refs) < gateSweepBatch {
			return // window exhausted
		}
		// Rows come back newest-first, so the next page starts at the oldest
		// row of this one. A page that fails to advance the cursor (every row
		// sharing one timestamp, or none carrying it) would otherwise re-scan
		// the same rows until the page cap.
		if !oldest.Before(before) {
			s.warnf("merge-gate sweeper: cursor stalled at %s with a full page — the remaining candidates in the %s window were not examined this pass",
				before.Format(time.RFC3339), lookback)
			return
		}
		before = oldest
	}
	s.warnf("merge-gate sweeper: stopped after %d pages of %d — the oldest candidates in the %s window were not examined this pass",
		gateSweepMaxPages, gateSweepBatch, lookback)
}

// gateSweepIsLastPass reports whether this pass is among the final ones that
// will ever offer the run to the reconciler. Candidacy is bounded by
// gateSweepHorizon on the run's own updated_at, so once that much time has
// passed the run leaves the window and NOTHING revisits it — whatever the
// reconciler abstained on becomes permanent.
//
// That instant is the only one worth a Warn out of every identical pass: a
// stuck check is not news while the net is still trying, and is news the
// moment the net gives up. The margin is two DEEP intervals, the spacing of
// the passes that actually reach the horizon, so a late or skipped one does
// not swallow the only line that names the reason (2026-08-29: a pull request
// sat behind an unanswered required check for 22h and the whole sweep history
// had been Debug, which deployments suppress at info level).
func (s *Server) gateSweepIsLastPass(run *store.Run) bool {
	if run == nil || run.UpdatedAt.IsZero() {
		return false
	}
	return s.gateNow().Sub(run.UpdatedAt) >= gateSweepHorizon-2*gateDeepSweepEvery*gateSweepInterval
}

// gateNow reads the wall clock the gate lanes measure by — the sweeper's
// lookback window and the unattended launch-failure backoff (the launch tail
// stamps Delivery.FailedAt from it so both sides of that backoff agree) —
// overridable in tests (mirrors scheduleClock).
func (s *Server) gateNow() time.Time {
	if s != nil && s.gateClock != nil {
		return s.gateClock()
	}
	return time.Now().UTC()
}
