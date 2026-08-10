package server

import (
	"context"
	"time"

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

	// gateSweepLookback bounds how far back a pass reaches. It has to exceed
	// the interval by enough to cover a replica restart or a slow pass without
	// a run slipping through the gap between two windows — but it is NOT
	// unbounded: re-offering week-old runs would mean posting a synthetic
	// failure onto pull requests that have long since merged or moved on.
	gateSweepLookback = 60 * time.Minute

	// gateSweepBatch bounds one pass. Each candidate can cost a few forge
	// round-trips, and the overwhelming majority are runs that gate nothing
	// and exit on a local field read.
	gateSweepBatch = 200
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
	s.infof("merge-gate sweeper: re-offering dead gating runs to the reconciler (every %s, %s grace, %s lookback) — the net under the lossy outcome event",
		gateSweepInterval, gateSweepGrace, gateSweepLookback)
	t := time.NewTicker(gateSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepGates(ctx, lister, time.Now().UTC())
		}
	}
}

// sweepGates performs one pass. Extracted (with an injectable clock) for tests.
func (s *Server) sweepGates(ctx context.Context, lister gateSweepLister, now time.Time) {
	if lister == nil || s.cfg.Store == nil {
		return
	}
	refs, err := lister.ListNotifiableRuns(
		store.WithoutTenantFilter(ctx),
		now.Add(-gateSweepLookback),
		now.Add(-gateSweepGrace),
		gateSweepBatch,
	)
	if err != nil {
		s.warnf("merge-gate sweeper: scan: %v", err)
		return
	}
	if len(refs) == gateSweepBatch {
		// The window is bounded and so is the batch, so a full page means the
		// oldest candidates of this window were not examined. Saying so beats
		// a silent truncation that reads as "everything was covered".
		s.warnf("merge-gate sweeper: batch full at %d runs — the oldest candidates in the %s window were not examined this pass",
			gateSweepBatch, gateSweepLookback)
	}
	for _, ref := range refs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// Every guard that decides whether this run owes anything lives in the
		// reconciler; the sweep's only job is to offer the run again. Runs that
		// gate nothing — the vast majority of any window — exit on a local
		// field read with no forge traffic.
		_ = s.reconcileGateForRunID(ctx, ref.ID, "sweep")
	}
}
