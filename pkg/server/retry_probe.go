package server

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// Pull-forward probe: the half of the usage-window retry that notices a reset
// nobody announced.
//
// The due pass can only ever act on `retry_after <= now`, so a retry armed
// for the instant the provider named is invisible to every pass until that
// instant arrives. An operator who tops up or clears their forfait at noon is
// therefore unobserved, and the run keeps waiting out a wall that has already
// fallen — measured on six reviews that slept four days.
//
// Nothing server-side can read a tenant credential's provider window (the
// readings arrive delegate-side, per session), but nothing needs to: the
// runner's usage preflight IS that reading, and it refuses before an attempt
// costs anything. So this pass pulls a not-yet-due retry forward to now and
// lets the ordinary due pass resume it on its next tick — worst case the run
// re-fails at the preflight and the announced instant comes back, best case
// the window really did reopen.
//
// Two bounds keep the speculation honest: a floor (one probe per row per
// floor — the price the codebase already blesses as usageWindowBlindWait,
// "one wasted pod an hour beats either giving up on the run or waiting a
// speculative week"), and a per-wait budget so a forfait nobody ever resets
// stops being probed and simply waits. Neither spends an attempt: the attempt
// budget is for work that gets PAST the preflight — with one bounded
// exception, the park of a pull older than store.ProbeMarkerTTL, which nothing
// can tell from such work and which that constant prices.
const (
	// EnvRetryProbeFloor overrides how long a row rests between probes, and
	// how close to its announced instant probing stops being worth a pod.
	EnvRetryProbeFloor = "ITERION_RETRY_PROBE_FLOOR"
	// EnvRetryProbeBudget overrides the probes one armed wait may spend.
	// `0` turns the pass off — the manual sweep (`iterion remote runs
	// resume --usage-blocked`) remains the operator's own path.
	EnvRetryProbeBudget = "ITERION_RETRY_PROBE_BUDGET"

	// defaultRetryProbeFloor matches usageWindowBlindWait: one speculative
	// pod an hour is the cost this codebase has already accepted for a run
	// whose reset instant is unknown, and a probe is the same bet.
	defaultRetryProbeFloor = time.Hour
	// defaultRetryProbeBudget covers a day of hourly observation. Past it
	// the run waits for the announced instant: an operator who has not
	// reset their forfait in 24 hours is not about to be caught by a 25th
	// pod.
	defaultRetryProbeBudget = 24
	// retryProbeBatch bounds one pass, for retrySweepBatch's reason — a
	// reset wave is several runs, and pulling them all into the same minute
	// is how a freshly reopened window is exhausted immediately — and for
	// one of its own: every row this pass pulls becomes DUE, so a batch as
	// large as retrySweepBatch fills the whole capacity of the pass that
	// resumes genuinely due runs (both scans sort ASC on retry_after, and a
	// row pulled a minute ago sorts ahead of a retry that comes due this
	// second). Two a pass still spends a whole probe budget far inside the
	// floor, and leaves the due pass three of its five slots — plus a
	// pending gauge that still MEANS something when it sits at the cap.
	retryProbeBatch = 2
)

// retryProbeBounds is the probe's two knobs, resolved per pass so an
// operator's change lands on the next tick.
type retryProbeBounds struct {
	floor  time.Duration
	budget int
}

// retryProbeBoundsFromEnv reads the operator's overrides. An unparseable or
// non-positive floor falls back to the default — a typo must not turn the
// floor into "probe every tick" — while a budget of 0 is a MEANINGFUL value
// (probing off) and is honoured as written.
func retryProbeBoundsFromEnv() retryProbeBounds {
	b := retryProbeBounds{floor: defaultRetryProbeFloor, budget: defaultRetryProbeBudget}
	if raw := strings.TrimSpace(os.Getenv(EnvRetryProbeFloor)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			b.floor = d
		}
	}
	if raw := strings.TrimSpace(os.Getenv(EnvRetryProbeBudget)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			b.budget = n
		}
	}
	return b
}

// sweepRetryProbes performs one probe pass. Same shape as sweepDueRetries
// (injectable clock, platform-wide scan, per-row tenant stamped before every
// write) and multi-replica-safe for the same reason: the pull is a
// compare-and-swap on the instant the scan read, so a loser just moves on.
func (s *Server) sweepRetryProbes(ctx context.Context, lister retryDueLister, now time.Time) {
	retryStore := store.AsRunRetryStore(s.cfg.Store)
	if retryStore == nil {
		return
	}
	bounds := retryProbeBoundsFromEnv()
	if bounds.budget <= 0 {
		return
	}
	refs, err := lister.ListRunsForRetryProbe(store.WithoutTenantFilter(ctx), now, bounds.floor, bounds.budget, retryProbeBatch)
	if err != nil {
		s.warnf("retry sweeper: probe scan: %v", err)
		return
	}
	for _, ref := range refs {
		s.probeRetry(ctx, retryStore, ref, bounds, now)
	}
}

// probeRetry pulls one row forward. It does NOT resume: the due pass owns
// that, and routing the probe through it keeps one resume path with one set
// of failure branches.
func (s *Server) probeRetry(ctx context.Context, retryStore store.RunRetryStore, ref mongostore.RetryDueRef, bounds retryProbeBounds, now time.Time) {
	runCtx := store.WithIdentity(ctx, ref.TenantID, "retry-sweeper")
	announced := ref.RetryAfter()
	pulled, err := retryStore.PullRunRetry(runCtx, ref.ID, announced, now)
	if err != nil {
		s.warnf("retry sweeper: probe pull %s: %v", ref.ID, err)
		return
	}
	if !pulled {
		// Another replica pulled it, or the row moved under the scan. Both
		// are ordinary outcomes of a CAS, not failures.
		return
	}
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.RunsRetryProbes.Inc()
	}
	probe := retryProbeCount(ref) + 1
	s.auditRetry(ref, "run.retry.probed", map[string]any{
		"announced_retry_after": announced,
		"probe":                 probe,
		"probe_budget":          bounds.budget,
	})
	s.infof("retry sweeper: run %s (tenant %s) pulled forward for usage-window probe %d/%d — its provider reset is announced for %s, the runner's preflight answers now",
		ref.ID, ref.TenantID, probe, bounds.budget, announced.Format(time.RFC3339))
}

// standDownProbedRetry puts the provider's announced instant back on a
// probe-pulled row, spending no attempt, and reports true — which tells its
// caller that its own re-arm or give-up must NOT run. A probe may give a run
// an earlier chance to resume; it must never give it a shorter life.
//
// Both of the sweeper's settlement paths would otherwise undo that: reArmRetry
// charges an attempt and overwrites retry_after with now+retryReenqueueBackoff,
// so a pulled run denied for a TRANSIENT admission reason (a concurrency cap,
// a monthly quota) loses the announced instant and re-probes every quarter
// hour until the attempt budget is gone — about an hour; and abandonRetry
// retires the run days before the wall it waits on falls, on a classification
// whose default is "permanent". Deferring both to the announced instant costs
// the wait that was already arranged, and the due pass makes the real decision
// there.
func (s *Server) standDownProbedRetry(ctx context.Context, retryStore store.RunRetryStore, tenantID, runID, cause string) bool {
	run, err := s.cfg.Store.LoadRun(ctx, runID)
	if err != nil {
		// Whether this row is a probe's is exactly what cannot be read, and
		// the two wrong answers are not symmetric: settling a probed row
		// destroys the announced instant for good, deferring one costs a
		// tick. The row stays armed at the probe instant, so the next pass
		// walks the same path.
		s.warnf("retry sweeper: run %s: cannot read the run doc to tell a probed retry from an ordinary one (%v) — deferring the decision to the next pass", runID, err)
		return true
	}
	if run == nil {
		return false
	}
	// The WALL door (AnnouncedProbeFrom), not the payment one: an instant
	// already behind us belongs to a wait that is over, and restoring it would
	// arm the row in the past — a resume on every tick instead of the caller's
	// backoff — while a pull whose resume the circuit deferred for longer than
	// store.ProbeMarkerTTL still has the provider's word to put back, and
	// settling such a row would destroy it for good.
	marker := run.RetryState.AnnouncedProbeFrom(time.Now().UTC())
	if marker == nil {
		return false
	}
	announced := marker.UTC()
	rearmed, err := retryStore.RearmProbedRunRetry(ctx, runID, announced)
	if err != nil {
		s.warnf("retry sweeper: run %s: cannot restore the provider's announced instant %s after a probe (%v) — deferring the decision to the next pass",
			runID, announced.Format(time.RFC3339), err)
		return true
	}
	if !rearmed {
		// The marker went away under us (a resume, an abandon, an operator):
		// this is no longer a probed row, so the caller's ordinary path is
		// the right one.
		return false
	}
	s.auditSystem(tenantID, "retry-sweeper", "run.retry.probe_stood_down", "run", runID, map[string]any{
		"retry_after": announced,
		"cause":       cause,
	})
	s.infof("retry sweeper: run %s: the usage-window probe did not resume it (%s) — the provider's announced instant %s is back and no attempt was spent",
		runID, cause, announced.Format(time.RFC3339))
	return true
}

// retryProbeCount reports the probes already charged to this armed wait.
func retryProbeCount(ref mongostore.RetryDueRef) int {
	if ref.RetryState == nil {
		return 0
	}
	return ref.RetryState.ProbeCount
}
