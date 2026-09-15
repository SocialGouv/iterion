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
	// Where the last deep pass ran out of page budget. Only the DEEP pass
	// carries one: the fast pass's contract is "answer a dropped event within
	// the minute", which means always starting at the head.
	var deepResume time.Time
	// Deep passes spent on the traversal currently in progress. It is what
	// makes the give-up warning's band a MEASUREMENT instead of a guess: with
	// a resuming cursor a run is revisited once per full traversal, not once
	// per deep pass, and how long that is depends on the backlog.
	deepThisCycle := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().UTC()
			if gateSweepIsDeep(pass) {
				deepThisCycle++
				deepResume = s.sweepGates(ctx, lister, now, gateSweepHorizon, deepResume)
				if deepResume.IsZero() { // the traversal reached the end of the window
					s.noteGateDeepCycle(deepThisCycle, true)
					deepThisCycle = 0
				} else {
					s.noteGateDeepCycle(deepThisCycle, false)
				}
			} else {
				s.sweepGates(ctx, lister, now, gateSweepLookback, time.Time{})
			}
			pass++
		}
	}
}

// noteGateDeepCycle records how long a traversal of the horizon is taking.
// `complete` says whether the walk just finished, which is what lets the
// measure SHRINK again: a high-water mark would keep a band wide forever
// after one busy day.
func (s *Server) noteGateDeepCycle(passes int, complete bool) {
	if s == nil {
		return
	}
	if complete {
		s.gateDeepCycleLen.Store(int64(passes))
		return
	}
	// Mid-traversal: the cycle is already at least this long, so widen the
	// band now rather than after the run it needs to warn about has aged out.
	if int64(passes) > s.gateDeepCycleLen.Load() {
		s.gateDeepCycleLen.Store(int64(passes))
	}
}

// gateSweepIsDeep reports whether pass number `pass` reaches the full horizon.
// Pass 0 — the first after a start or a rollout — is deep on purpose: a
// replica that has just come up is precisely the one with no idea what died
// while nothing was watching.
func gateSweepIsDeep(pass int) bool { return pass%gateDeepSweepEvery == 0 }

// gateSweepWindowFor picks how far back pass number `pass` reaches.
func gateSweepWindowFor(pass int) time.Duration {
	if gateSweepIsDeep(pass) {
		return gateSweepHorizon
	}
	return gateSweepLookback
}

// sweepGates performs one pass over the window `lookback` reaches back to,
// starting at `resume` when a previous pass of the same depth ran out of page
// budget there (the zero time starts at the head). It returns where the next
// pass of that depth should pick up, or the zero time when the window was
// walked to its end.
//
// That return value is what makes the horizon REACHABLE rather than merely
// declared. One pass is capped at gateSweepMaxPages × gateSweepBatch rows, and
// rows arrive updated_at-descending, so a backlog bigger than that cap is
// truncated at its OLDEST end — precisely the batch-death runs the horizon
// exists to reach. Restarting at the head every pass would drop the same rows
// every time, forever, while the warning below said "not examined this pass"
// as though a later one would get to them. Resuming means each deep pass
// carries the traversal further: a backlog of P candidates is walked in
// ceil(P / (gateSweepMaxPages × gateSweepBatch)) deep passes, i.e. that many
// × gateDeepSweepEvery × gateSweepInterval of wall clock.
//
// Extracted (with an injectable clock, window and cursor) for tests.
func (s *Server) sweepGates(ctx context.Context, lister gateSweepLister, now time.Time, lookback time.Duration, resume time.Time) time.Time {
	if lister == nil || s.cfg.Store == nil {
		return time.Time{}
	}
	since := now.Add(-lookback)
	head := now.Add(-gateSweepGrace)
	before := head
	// A resume point that has aged out below the window is not a cursor any
	// more — the backlog it pointed into has left the horizon. Start over at
	// the head rather than scan an empty range for the rest of the traversal.
	if !resume.IsZero() && resume.After(since) && resume.Before(head) {
		before = resume
	}
	for page := 0; page < gateSweepMaxPages; page++ {
		refs, err := lister.ListNotifiableRuns(store.WithoutTenantFilter(ctx), since, before, gateSweepBatch)
		if err != nil {
			s.warnf("merge-gate sweeper: scan: %v", err)
			// Keep the ground already covered: a transient store error must
			// not send the next deep pass back to the head to re-walk it.
			return before
		}
		oldest := before
		for _, ref := range refs {
			select {
			case <-ctx.Done():
				return before
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
			return time.Time{} // window exhausted — the next pass starts fresh at the head
		}
		// Rows come back newest-first, so the next page starts at the oldest
		// row of this one. A page that fails to advance the cursor (every row
		// sharing one timestamp, or none carrying it) would otherwise re-scan
		// the same rows until the page cap.
		if !oldest.Before(before) {
			s.warnf("merge-gate sweeper: cursor stalled at %s with a full page — the remaining candidates in the %s window were not examined this pass",
				before.Format(time.RFC3339), lookback)
			// Deliberately NOT a resume point. The cursor is a timestamp, so
			// handing this one back would page past the tied rows and lose
			// them for the rest of the traversal; restarting at the head keeps
			// the pre-existing behaviour, which re-examines them.
			return time.Time{}
		}
		before = oldest
	}
	if lookback == gateSweepHorizon {
		s.warnf("merge-gate sweeper: stopped after %d pages of %d in the %s window — the next deep pass (one every %d, so ~%s) resumes at %s instead of restarting at the head",
			gateSweepMaxPages, gateSweepBatch, lookback, gateDeepSweepEvery,
			gateDeepSweepEvery*gateSweepInterval, before.Format(time.RFC3339))
	} else {
		s.warnf("merge-gate sweeper: stopped after %d pages of %d — the oldest candidates in the %s window were not examined this pass",
			gateSweepMaxPages, gateSweepBatch, lookback)
	}
	return before
}

// gateSweepIsLastPass reports whether this pass is among the final ones that
// will ever offer the run to the reconciler. Candidacy is bounded by
// gateSweepHorizon on the run's own updated_at, so once that much time has
// passed the run leaves the window and NOTHING revisits it — whatever the
// reconciler abstained on becomes permanent.
//
// That instant is the only one worth a Warn out of every identical pass: a
// stuck check is not news while the net is still trying, and is news the
// moment the net gives up (2026-08-29: a pull request sat behind an unanswered
// required check for 22h and the whole sweep history had been Debug, which
// deployments suppress at info level).
//
// The band has to be at least as wide as the interval at which this run is
// actually REVISITED, or the run ages out between two visits and the one line
// that names the reason is never written. See gateSweepLastPassMargin: that
// interval is a full traversal of the horizon, which the deep pass measures
// rather than assumes.
func (s *Server) gateSweepIsLastPass(run *store.Run) bool {
	if run == nil || run.UpdatedAt.IsZero() {
		return false
	}
	return s.gateNow().Sub(run.UpdatedAt) >= gateSweepHorizon-s.gateSweepLastPassMargin()
}

// gateSweepLastPassMargin is how wide the give-up band is.
//
// It used to be two deep intervals, on the assumption that a run is revisited
// every deep pass. That stopped being true when the deep pass started resuming
// its cursor across passes: a run is now revisited once per full TRAVERSAL of
// the horizon, and a backlog wider than one pass budget makes a traversal
// several deep passes long. A fixed band would then be stepped clean over —
// the run is visited before it, and never again after it.
//
// So it is derived from what the sweeper measured (gateDeepCycleLen): a
// traversal of N deep passes revisits a run every N of them, and the band
// spans N+1 so a late or skipped pass still cannot swallow the line. That is
// the same rule the historical constant encoded — it read two intervals
// because a traversal was assumed to be one pass — now stated for any N
// instead of for N=1, and floored there so the guarantee never weakens.
//
// Capped at half the horizon: a traversal longer than THAT means the net is
// not keeping up at all, which is a per-deployment fact the page-cap warning
// already states once per pass — turning it into a per-run Warn storm would
// bury the branches that carry new information, the very thing the Debug/Warn
// split exists to prevent.
func (s *Server) gateSweepLastPassMargin() time.Duration {
	passes := int64(2)
	if s != nil {
		if measured := s.gateDeepCycleLen.Load(); measured+1 > passes {
			passes = measured + 1
		}
	}
	margin := time.Duration(passes) * gateDeepSweepEvery * gateSweepInterval
	if max := gateSweepHorizon / 2; margin > max {
		margin = max
	}
	return margin
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
