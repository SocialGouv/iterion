package server

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
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
	// net — the figure the gating grant's own post-run life derives from
	// (forgePublishGateGrace).
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

	// gateSweepMinInterval is the shortest interval an operator may set: the
	// lease is paced by it, and below a second a pass would outlive its own
	// ticker on any real forge.
	gateSweepMinInterval = time.Second

	// gateSweepLeaseTTLFactor sizes the sweep's lease in paces — the sweep
	// interval, capped at the default one (runElectedGateSweeper). The holder
	// renews every pace and steps down 2.5 paces after its last successful
	// renewal was sent, so the term survives one lost renewal when the next
	// one, sent a pace later, is answered within half a pace. A holder that
	// dies without releasing is outlived by the TTL: a successor is elected at
	// most one retry (one pace) after that — four paces after its last renewal
	// — and its term opens with a pass at once.
	gateSweepLeaseTTLFactor = 3
)

// gateSweepLister is the store capability the sweep scans with — the same
// bounded-window terminal-run query the usernotify sweep uses, reused rather
// than duplicated so both nets share one index. Implemented by the Mongo
// store; the local store has no cloud gate to reconcile and no sweeper.
type gateSweepLister interface {
	ListNotifiableRuns(ctx context.Context, since, before time.Time, limit int) ([]mongostore.NotifiableRunRef, error)
}

// runElectedGateSweeper runs the sweep on the ONE replica holding the
// merge-gate lease, and campaigns for it on every other. Started by
// ListenAndServe alongside the event-driven reconciler.
//
// The repair is idempotent — reconcileGateForRunID re-reads the live status
// before it writes — so what every replica sweeping cost was, first, N times
// the forge reads: each offer spends the App installation's hourly budget,
// which the sweep alone outspends at the fleet's replica count. It also put N
// sweeps on every dead run at once, and two offers racing on one run are what
// the relaunch and escalation guards have to absorb. The lease makes the cost
// one replica's and keeps the net's reach: a holder that stops releases on
// its way out, one that dies is outlived by the TTL, and each term opens with
// a deep pass.
func (s *Server) runElectedGateSweeper(ctx context.Context, lister gateSweepLister) {
	err := lease.Run(ctx, s.leases, s.gateSweepLeaseSpec(), func(ctx context.Context) { s.runGateSweeper(ctx, lister) })
	if err != nil {
		s.warnf("merge-gate sweeper NOT started: %v — a review whose outcome event is dropped will leave its required check absent forever", err)
	}
}

// gateSweepLeaseSpec is the sweep's lease. It is paced by the sweep interval,
// but never slower than the default one: a longer interval an operator chose
// to spare the forge must not also stretch the time a dead holder keeps the
// net down.
func (s *Server) gateSweepLeaseSpec() lease.Spec {
	pace := min(s.gateSweepEvery(), gateSweepInterval)
	return lease.Spec{
		Name:  leaseMergeGateSweeper,
		Owner: s.replicaID,
		TTL:   gateSweepLeaseTTLFactor * pace,
		Retry: pace,
		Info:  s.infof,
		Warn:  s.warnf,
	}
}

// gateSweepSettings is the sweep's cadence: the interval between passes, how
// far back an ordinary pass reaches, and how many passes separate two deep
// ones. The defaults are the constants above; an operator overrides each from
// the environment (gateSweepSettingsFromEnv) — the lever that slows the sweep
// down without a release when the forge's budget is what runs short. The
// horizon is not one of them: it is the grant's, and a sweep reaching past it
// would find nothing to post with.
type gateSweepSettings struct {
	interval  time.Duration
	lookback  time.Duration
	deepEvery int
}

func defaultGateSweepSettings() gateSweepSettings {
	return gateSweepSettings{interval: gateSweepInterval, lookback: gateSweepLookback, deepEvery: gateDeepSweepEvery}
}

// gateSweepSettingsFromEnv reads ITERION_GATE_SWEEP_INTERVAL,
// ITERION_GATE_SWEEP_LOOKBACK (Go durations) and ITERION_GATE_SWEEP_DEEP_EVERY
// (a pass count). A value that does not parse, an interval under
// gateSweepMinInterval or of half the horizon or more, or a combination that
// breaks the net keeps the default for that setting, and warns naming the
// variable — an operator's explicit choice is never replaced in silence. The
// combinations the net cannot survive:
//   - a lookback not exceeding interval plus grace: a run could end between
//     two windows and never be examined;
//   - a lookback reaching the horizon: every pass would be a deep one, and the
//     ordinary grant would outlive the gate's;
//   - deep passes half the horizon apart or more: a run would see fewer than
//     two, and the last-pass warning would fire on every offer.
//
// Every bound is checked without multiplying the operator's values, which
// overflow: a huge pass count must be refused, not wrap into an accepted one.
func gateSweepSettingsFromEnv(getenv func(string) string, warnf func(format string, args ...any)) gateSweepSettings {
	out := defaultGateSweepSettings()
	duration := func(name string, def time.Duration) time.Duration {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return def
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			warnf("merge-gate sweeper: %s=%q is not a positive duration — keeping the default %s", name, raw, def)
			return def
		}
		return d
	}
	out.interval = duration("ITERION_GATE_SWEEP_INTERVAL", gateSweepInterval)
	switch {
	case out.interval < gateSweepMinInterval:
		warnf("merge-gate sweeper: ITERION_GATE_SWEEP_INTERVAL=%s is under the %s floor — keeping the default %s", out.interval, gateSweepMinInterval, gateSweepInterval)
		out.interval = gateSweepInterval
	case out.interval >= gateSweepHorizon/2:
		warnf("merge-gate sweeper: ITERION_GATE_SWEEP_INTERVAL=%s is half the %s horizon or more — keeping the default %s", out.interval, gateSweepHorizon, gateSweepInterval)
		out.interval = gateSweepInterval
	}
	out.lookback = duration("ITERION_GATE_SWEEP_LOOKBACK", gateSweepLookback)
	if out.lookback >= gateSweepHorizon {
		warnf("merge-gate sweeper: ITERION_GATE_SWEEP_LOOKBACK=%s reaches the %s horizon — keeping the default lookback %s", out.lookback, gateSweepHorizon, gateSweepLookback)
		out.lookback = gateSweepLookback
	}
	if raw := strings.TrimSpace(getenv("ITERION_GATE_SWEEP_DEEP_EVERY")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			warnf("merge-gate sweeper: ITERION_GATE_SWEEP_DEEP_EVERY=%q is not a pass count ≥ 1 — keeping the default %d", raw, gateDeepSweepEvery)
		} else {
			out.deepEvery = n
		}
	}
	// interval < horizon/2 from here on, so interval+grace cannot overflow.
	// Each fallback blames the variable it resets: a value already at its
	// default is left alone, and the interval answers for the gap instead.
	if out.lookback <= out.interval+gateSweepGrace {
		if out.lookback != gateSweepLookback {
			warnf("merge-gate sweeper: ITERION_GATE_SWEEP_LOOKBACK=%s does not exceed ITERION_GATE_SWEEP_INTERVAL=%s plus the %s grace, so a run could end between two windows — keeping the default lookback %s",
				out.lookback, out.interval, gateSweepGrace, gateSweepLookback)
			out.lookback = gateSweepLookback
		}
		if out.lookback <= out.interval+gateSweepGrace {
			warnf("merge-gate sweeper: ITERION_GATE_SWEEP_INTERVAL=%s is not covered by the %s lookback plus the %s grace, so a run could end between two windows — keeping the default interval %s",
				out.interval, out.lookback, gateSweepGrace, gateSweepInterval)
			out.interval = gateSweepInterval
		}
	}
	if !deepCadenceFits(out.deepEvery, out.interval) {
		if out.deepEvery != gateDeepSweepEvery {
			warnf("merge-gate sweeper: ITERION_GATE_SWEEP_DEEP_EVERY=%d × ITERION_GATE_SWEEP_INTERVAL=%s spaces deep passes half the %s horizon apart or more — keeping the default deep cadence %d",
				out.deepEvery, out.interval, gateSweepHorizon, gateDeepSweepEvery)
			out.deepEvery = gateDeepSweepEvery
		}
		if !deepCadenceFits(out.deepEvery, out.interval) {
			warnf("merge-gate sweeper: ITERION_GATE_SWEEP_INTERVAL=%s spaces the %d-pass deep cadence half the %s horizon apart or more — keeping the default interval %s",
				out.interval, out.deepEvery, gateSweepHorizon, gateSweepInterval)
			out.interval = gateSweepInterval
		}
	}
	return out
}

// deepCadenceFits reports whether deep passes every deepEvery passes of
// interval come strictly closer than half the horizon. It divides rather than
// multiplies: the product of a large pass count and an interval overflows into
// a small or negative duration that every comparison would accept.
func deepCadenceFits(deepEvery int, interval time.Duration) bool {
	return deepEvery >= 1 && interval > 0 && int64(deepEvery) <= int64((gateSweepHorizon/2-1)/interval)
}

// gateSweepEvery is the sweep cadence: the configured interval, unless a test
// shortened it.
func (s *Server) gateSweepEvery() time.Duration {
	if s != nil && s.gateSweepTick > 0 {
		return s.gateSweepTick
	}
	if s != nil && s.gateSweep.interval > 0 {
		return s.gateSweep.interval
	}
	return gateSweepInterval
}

// gateSweepCadence is the configured lookback and deep cadence, defaults
// filled in.
func (s *Server) gateSweepCadence() (lookback time.Duration, deepEvery int) {
	lookback, deepEvery = gateSweepLookback, gateDeepSweepEvery
	if s != nil && s.gateSweep.lookback > 0 {
		lookback = s.gateSweep.lookback
	}
	if s != nil && s.gateSweep.deepEvery > 0 {
		deepEvery = s.gateSweep.deepEvery
	}
	return lookback, deepEvery
}

// gateSweepWindow picks how far back pass number `pass` of a term reaches,
// and whether it is a deep pass. Pass 0 — the first of a term — is deep on
// purpose: a replica that has just been elected is precisely the one with no
// idea what died while it was not sweeping.
func (s *Server) gateSweepWindow(pass int) (window time.Duration, deep bool) {
	lookback, deepEvery := s.gateSweepCadence()
	if pass%deepEvery == 0 {
		return gateSweepHorizon, true
	}
	return lookback, false
}

// runGateSweeper ticks sweepGates until ctx is cancelled — for one term of the
// merge-gate lease (runElectedGateSweeper).
//
// It announces itself for the reason the retry sweeper does: "no PR is stuck"
// and "every stuck PR is invisible" produce identical silence otherwise. The
// line also names the replica holding the term.
func (s *Server) runGateSweeper(ctx context.Context, lister gateSweepLister) {
	every := s.gateSweepEvery()
	lookback, deepEvery := s.gateSweepCadence()
	s.infof("merge-gate sweeper: %s re-offering dead gating runs to the reconciler (every %s, %s grace, %s lookback, %s horizon reached every %d passes) — the net under the lossy outcome event",
		s.replicaID, every, gateSweepGrace, lookback, gateSweepHorizon, deepEvery)
	t := time.NewTicker(every)
	defer t.Stop()
	// The first pass of a term is a deep one, and it runs at once: a replica
	// that has just been elected — at boot, or taking over from a holder that
	// stopped or died — is exactly the one with no idea what died while it was
	// not sweeping, and waiting even one interval to find out would make every
	// hand-over a blind window, and a fleet restarted more often than the
	// interval a net that never sweeps.
	pass := 0
	// Where the deep traversal under way ran out of page budget. A pass is
	// capped at gateSweepMaxPages × gateSweepBatch rows, and rows arrive
	// newest-first, so a deep pass that restarted at now−grace every time would
	// examine the same newest rows forever and NEVER reach the old ones — which
	// are precisely the batch-death runs the horizon exists for. Resuming from
	// the cursor is what turns a capped pass into a traversal.
	//
	// The traversal resumes at the very next pass, beside that pass's fast
	// one, not at the next deep pass: with deep passes spaced out, waiting
	// would let a traversal outlast the runs it walks — a run near the
	// window's edge would leave it unvisited, and with it the re-read a
	// reversible settlement counts on. A traversal ends when the window is
	// exhausted (the cursor back to zero, see sweepGates); the next deep pass
	// starts a new one at the newest end.
	//
	// In memory, for the term: the repair is idempotent, so a term that ends
	// loses the cursor and nothing else — the successor's first pass is deep
	// and starts again at the newest end.
	var deepCursor time.Time
	// The consecutive deep sweeps that did not move the cursor. One is a blip
	// the next pass retries; a page that keeps failing — two in a row — waits
	// for the next deep pass instead of failing at every tick.
	deepMisses := 0
	sweep := func() {
		now := time.Now().UTC()
		_, deep := s.gateSweepWindow(pass)
		parked := !deepCursor.IsZero()
		if !deep || parked {
			// The fast pass always starts at the newest end, so a deep
			// traversal parked in the past never delays a fresh death.
			s.sweepGates(ctx, lister, now, lookback, time.Time{})
		}
		if deep || (parked && deepMisses < 2) {
			from := deepCursor
			deepCursor = s.sweepGates(ctx, lister, now, gateSweepHorizon, from)
			if !deepCursor.IsZero() && deepCursor.Equal(from) {
				deepMisses++
			} else {
				deepMisses = 0
			}
		}
		pass++
	}
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

// sweepGates performs one pass over the window `lookback` reaches back to,
// starting at `resumeFrom` when that is a usable point inside it.
//
// It returns where to resume NEXT time: a non-zero instant only when the pass
// stopped on its page budget with candidates still older than it. Zero means
// the window was traversed (or the cursor could not advance), and the next
// pass should start at the newest end again.
//
// Extracted (with an injectable clock, window and cursor) for tests.
func (s *Server) sweepGates(ctx context.Context, lister gateSweepLister, now time.Time, lookback time.Duration, resumeFrom time.Time) time.Time {
	if lister == nil || s.cfg.Store == nil {
		return time.Time{}
	}
	// Every forge request the pass itself sends — its reconcile, autofix and
	// relaunch offers — is counted as the sweep's (forgeRequestTally): it is
	// the lane whose cost the election exists to bound, and the hourly line
	// is how that is read. What a relaunched run later publishes is its own.
	ctx = withForgeLane(ctx, forgeLaneGateSweeper)
	since := now.Add(-lookback)
	before := now.Add(-gateSweepGrace)
	// A cursor older than the window has nothing left to offer: the window
	// slides forward with `now`, so reaching past its far edge means the whole
	// horizon was covered. Start over at the newest end.
	if !resumeFrom.IsZero() && resumeFrom.Before(before) && resumeFrom.After(since) {
		before = resumeFrom
	}
	for page := 0; page < gateSweepMaxPages; page++ {
		refs, err := lister.ListNotifiableRuns(store.WithoutTenantFilter(ctx), since, before, gateSweepBatch)
		if err != nil {
			s.warnf("merge-gate sweeper: scan: %v", err)
			// A failed scan proved nothing about what lies past `before`, so
			// keep the cursor: restarting at the newest end would re-walk
			// ground already covered and stall the descent on every error.
			return before
		}
		oldest := before
		settled := s.gateSettledIn(ctx, refs)
		for _, ref := range refs {
			select {
			case <-ctx.Done():
				return before
			default:
			}
			// Every guard that decides whether this run owes anything lives in
			// the reconciler; the sweep's only job is to offer the run again.
			// Runs that gate nothing — the vast majority of any window — exit
			// on a local field read with no forge traffic, and a run the
			// reconciler already found settled is not offered at all.
			mark, isSettled := settled[ref.ID]
			if !isSettled {
				_ = s.reconcileGateForRunID(ctx, ref.ID, gateTriggerSweep)
			}
			// Same net for the auto-fix lane: it consumes the same lossy bus
			// with the same miss modes, and until now had NO second path — a
			// dropped outcome event silently meant no fix pass for a repo
			// that opted in. The offer is idempotent (per-head claim + idem
			// key) and the lane's own guards exclude cancelled/paused/armed
			// runs the reconciler-oriented window also contains. Recovery
			// horizon = gateSweepHorizon, same as the gate's. A failure verdict
			// is that lane's own trigger, so a run settled on one is still
			// offered: the lane stands down on its per-head claim without a
			// forge read once its fix has launched.
			if !isSettled || mark.Reason == gateSettledVerdictFailure {
				s.autofixOffer(ctx, ref.ID)
			}
			if !ref.UpdatedAt.IsZero() && ref.UpdatedAt.Before(oldest) {
				oldest = ref.UpdatedAt
			}
		}
		if len(refs) < gateSweepBatch {
			return time.Time{} // window exhausted — next pass starts fresh
		}
		// ListNotifiableRuns bounds only terminal runs by `since`: a run
		// waiting on a human comes back however old it is. A page reaching
		// past `since` has therefore offered every terminal run of the window,
		// and what lies further is paused runs neither lane acts on — the
		// window is exhausted, and walking on would page through them forever.
		if !oldest.After(since) {
			return time.Time{}
		}
		// Rows come back newest-first, so the next page starts at the oldest
		// row of this one. A page that fails to advance the cursor (every row
		// sharing one timestamp, or none carrying it) would otherwise re-scan
		// the same rows until the page cap.
		if !oldest.Before(before) {
			s.warnf("merge-gate sweeper: cursor stalled at %s with a full page — the remaining candidates in the %s window were not examined this pass",
				before.Format(time.RFC3339), lookback)
			// Resuming from a point that did not advance would stall there
			// forever; start the next pass at the newest end instead.
			return time.Time{}
		}
		before = oldest
	}
	// Out of page budget with candidates still older than `before`. Saying so
	// stays useful, but the pass is no longer the end of the road: the cursor
	// is what the next deep pass resumes from, so successive passes descend
	// toward the horizon instead of re-examining the newest rows forever.
	s.warnf("merge-gate sweeper: stopped after %d pages of %d in the %s window — the next deep pass resumes at %s",
		gateSweepMaxPages, gateSweepBatch, lookback, before.Format(time.RFC3339))
	return before
}

// gateSettledIn returns the runs of a page the reconciler marked settled for
// their current episode. A store that cannot answer settles nothing — every
// run is offered, as before the marks existed — and says so once per episode.
func (s *Server) gateSettledIn(ctx context.Context, refs []mongostore.NotifiableRunRef) map[string]gateSettlement {
	if s.gateSettles == nil || len(refs) == 0 {
		return nil
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	marks, err := s.gateSettles.settled(ctx, ids)
	if err != nil {
		if !s.gateSettleReadFailing.Swap(true) {
			s.warnf("merge-gate sweeper: the settled marks are unreadable — offering every run until they are: %v", err)
		}
		return nil
	}
	if s.gateSettleReadFailing.Swap(false) {
		s.infof("merge-gate sweeper: the settled marks are readable again")
	}
	out := make(map[string]gateSettlement, len(marks))
	for _, r := range refs {
		if g, ok := marks[r.ID]; ok && g.appliesTo(r.UpdatedAt) {
			out[r.ID] = g
		}
	}
	return out
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
	_, deepEvery := s.gateSweepCadence()
	return s.gateNow().Sub(run.UpdatedAt) >= gateSweepHorizon-2*time.Duration(deepEvery)*s.gateSweepEvery()
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
