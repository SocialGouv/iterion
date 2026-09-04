package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// Retry sweeper: the half of the usage-window retry that outlives the pod.
//
// The runner acks a quota-blocked run and persists WHEN to come back; nothing
// in the pod can hold a multi-day timer, and the work queue cannot either (a
// 24h max age, no delayed nak, against a weekly reset up to seven days out).
// So the wait lives in Mongo and this sweeper is what acts on it — the same
// shape as the orphan sweeper next door, and for the same reason: some run
// state can only be reconciled by something that keeps running.

const (
	// retrySweepInterval matches the orphan sweeper's cadence. A minute of
	// latency on a wait measured in hours is free, and the scan is a single
	// indexed query.
	retrySweepInterval = 60 * time.Second
	// retrySweepBatch bounds one pass. Several schedules routinely die on
	// one reset, and resuming all of them in the same second is how you
	// exhaust the freshly-reopened window immediately — the jitter at arm
	// time spreads them, and this caps the worst case regardless.
	retrySweepBatch = 5
	// retryReenqueueBackoff re-arms a retry whose resume failed for a
	// reason that may not be permanent (a transient publish error).
	retryReenqueueBackoff = 15 * time.Minute
)

// runResumer is the slice of runview.Service the sweeper uses. Narrowed to
// one method so the sweeper's failure branches (lost claim, denied
// admission, unresolvable source, failed publish) are testable without
// standing up a run service.
type runResumer interface {
	Resume(ctx context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error)
}

// retryDueLister is the store capability the sweeper scans with
// (implemented by the Mongo store; local mode has no durable retry state
// and no sweeper).
type retryDueLister interface {
	ListRunsDueForRetry(ctx context.Context, before time.Time, limit int) ([]mongostore.RetryDueRef, error)
}

// runRetrySweeper ticks sweepDueRetries until ctx is cancelled. Started by
// ListenAndServe in cloud mode when the store carries retry state.
//
// It announces itself on the way in. Every other signal this path emits reads
// identically whether the sweeper is idle or absent — the counters sit at 0 and
// a registered-but-never-Set gauge reports 0 — so without this line and the
// sweep counter, "no run is waiting" and "every waiting run is stranded" are
// the same observation. Verified against production and they were.
func (s *Server) runRetrySweeper(ctx context.Context, lister retryDueLister) {
	s.infof("retry sweeper: watching for runs whose provider quota window has reopened (every %s, up to %d per pass)",
		retrySweepInterval, retrySweepBatch)
	t := time.NewTicker(retrySweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepDueRetries(ctx, lister, s.runs, time.Now().UTC())
		}
	}
}

// sweepDueRetries performs one pass. Extracted (with an injectable clock)
// for tests.
func (s *Server) sweepDueRetries(ctx context.Context, lister retryDueLister, resumer runResumer, now time.Time) {
	retryStore := store.AsRunRetryStore(s.cfg.Store)
	if retryStore == nil || resumer == nil {
		return
	}
	// Platform-level scan; the per-run tenant comes back on each ref and is
	// re-stamped before every write below.
	refs, err := lister.ListRunsDueForRetry(store.WithoutTenantFilter(ctx), now, retrySweepBatch)
	if err != nil {
		s.warnf("retry sweeper: scan: %v", err)
		return
	}
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.RunsRetrySweeps.Inc()
		// Sampled, not authoritative: the batch cap bounds what one pass
		// sees. A gauge that sits at the cap is itself the signal that
		// retries are arriving faster than they are being resumed.
		s.cfg.Metrics.RunsRetryPending.Set(float64(len(refs)))
	}
	for _, ref := range refs {
		s.resumeDueRetry(ctx, retryStore, resumer, ref)
	}
}

// resumeDueRetry claims one due retry and re-enqueues the run's resume.
func (s *Server) resumeDueRetry(ctx context.Context, retryStore store.RunRetryStore, resumer runResumer, ref mongostore.RetryDueRef) {
	runCtx := store.WithIdentity(ctx, ref.TenantID, "retry-sweeper")

	// Claim first, act second. The CAS is what makes this safe to run on
	// every replica: the loser simply moves on, and an operator who resumed
	// the run in the meantime has already invalidated the claim.
	won, err := retryStore.ClaimRunRetry(runCtx, ref.ID, ref.RetryAfter())
	if err != nil {
		s.warnf("retry sweeper: claim %s: %v", ref.ID, err)
		return
	}
	if !won {
		return
	}

	// An automatic resume spends real money, so it passes the same
	// admission gate as the operator-initiated one. Whether a denial ends
	// the retry depends on the denial: a monthly quota refills on the 1st
	// and a concurrency/rate cap clears in minutes, so abandoning on those
	// would throw the run away for a condition that resolves itself. A
	// suspended org or a missing workspace needs a human.
	adm, deny := s.gateLaunch(retryLaunchCtx(runCtx, ref))
	if deny != nil {
		if retryDenialIsTransient(deny.reason) {
			s.reArmRetry(runCtx, retryStore, ref, fmt.Errorf("admission deferred: %s", deny.reason))
			return
		}
		s.abandonRetry(runCtx, retryStore, ref.TenantID, ref.ID, fmt.Sprintf("auto-retry abandoned: %s", deny.reason))
		return
	}

	filePath, source, lb, err := s.resolveResumeSource(runCtx, ref.BotSourceTenant, ref.FilePath, "", "")
	if err != nil {
		adm.rollback(s.logger)
		if errors.Is(err, errResumeResolveTransient) {
			// A store blip is not "the bot is gone" — re-arm within the
			// attempt budget instead of permanently discarding a paid
			// usage-window retry.
			s.reArmRetry(runCtx, retryStore, ref, err)
			return
		}
		// A bot removed from the catalog will never resolve; re-arming
		// would just re-fail every 15 minutes forever.
		s.abandonRetry(runCtx, retryStore, ref.TenantID, ref.ID, fmt.Sprintf("auto-retry abandoned: %v", err))
		return
	}
	defer lb.Cleanup()

	retrySpec := runview.ResumeSpec{
		RunID:    ref.ID,
		FilePath: filePath,
		Source:   source,
		// Machinery resume: refuse a cancelled doc even with retry_after
		// still set on the record. Without this a cancel that lands after
		// ClaimRunRetry wins races the SubmitResume CAS `cancelled → queued`
		// (which clears run.Error), delivering a message with no PR-closed
		// marker to the runner — the review runs on the merged PR (#663 D1).
		Automatic: true,
	}
	if lb != nil {
		retrySpec.BundleDir, retrySpec.BotBundle = lb.BundleDir, lb.Ref
	}
	// Narrow the race window: read the run right before the resume and refuse
	// if it is no longer auto-resumable (cancelled by stop-on-close, moved
	// to a paused state by another writer). The SubmitResume CAS is the
	// authoritative close, but this pre-check spares the queue publish + a
	// rollback on the common case.
	if r, err := s.cfg.Store.LoadRun(runCtx, ref.ID); err == nil && r != nil && !r.Status.CanAutoResume() {
		adm.rollback(s.logger)
		s.abandonRetry(runCtx, retryStore, ref.TenantID, ref.ID,
			fmt.Sprintf("auto-retry abandoned: run is %s (%s)", r.Status, strings.TrimSpace(r.Error)))
		return
	}
	if _, err := resumer.Resume(runCtx, retrySpec); err != nil {
		// Could be transient (a publish blip) or permanent (the bot was
		// redeployed and the workflow hash moved). We do NOT pass Force:
		// resuming a checkpoint against a workflow that changed underneath
		// is how a run silently does the wrong thing. Re-arm within the
		// attempt budget and surface the error verbatim.
		adm.rollback(s.logger)
		s.reArmRetry(runCtx, retryStore, ref, err)
		return
	}

	// The claim was a lease, not a disarm (so a pod death before this point
	// leaves the retry re-claimable). Now that the resume is enqueued, clear
	// it — otherwise a past retry_after would survive and re-fire the moment
	// this run failed again for an unrelated reason.
	if err := retryStore.ClearRunRetry(runCtx, ref.ID); err != nil {
		s.warnf("retry sweeper: clear %s: %v", ref.ID, err)
	}
	s.countRetry("enqueued")
	s.auditRetry(ref, "run.retry.enqueued", map[string]any{
		"attempt": retryAttempts(ref),
		"reason":  retryReason(ref),
	})
	s.infof("retry sweeper: run %s (tenant %s) resumed after its provider quota window reopened (attempt %d)",
		ref.ID, ref.TenantID, retryAttempts(ref))
}

// retryDenialIsTransient reports whether an admission denial is expected to
// clear on its own, so the retry should be deferred rather than dropped.
// Unknown reasons are treated as PERMANENT: a new denial code that silently
// re-armed forever would be worse than one that stops and says why.
func retryDenialIsTransient(reason string) bool {
	switch reason {
	case denyMonthlyRunQuota, denyMonthlyCostCap, denyConcurrencyCap, denyLaunchRateLimited:
		return true
	default:
		return false
	}
}

// abandonRetry records a permanent stop and audits it.
func (s *Server) abandonRetry(ctx context.Context, retryStore store.RunRetryStore, tenantID, runID, reason string) {
	if err := retryStore.AbandonRunRetry(ctx, runID, reason); err != nil {
		// The abandon did not land: the retry stays claimable, the next sweep
		// tick walks this path again, and republishing NOW would repeat on
		// every tick until the write finally sticks. The single republish
		// belongs to the tick that actually makes the stop permanent.
		s.warnf("retry sweeper: abandon %s: %v", runID, err)
		return
	}
	s.countRetry("abandoned")
	s.auditSystem(tenantID, "retry-sweeper", "run.retry.abandoned", "run", runID, map[string]any{"reason": reason})
	s.warnf("retry sweeper: run %s: %s", runID, reason)

	// The run's terminal event fired back when it FAILED — while a retry was
	// still armed, so every outcome consumer that defers to an armed retry
	// (the merge-gate reconciler foremost) deliberately stood down. Abandoning
	// is the moment the run becomes permanently dead, and nothing else fires
	// for it: republish the outcome so those consumers get the event they
	// were promised. The episode-keyed dedup in each consumer absorbs the
	// replay for anyone who already acted.
	if bus := s.eventsBus(); bus != nil && s.cfg.Store != nil {
		ev := trigger.BuildRunOutcome(ctx, s.cfg.Store, runID, fmt.Errorf("%s", reason))
		if err := bus.Publish(ctx, ev); err != nil {
			s.warnf("retry sweeper: republish outcome for abandoned run %s: %v", runID, err)
		}
	}
}

// reArmRetry gives a failed resume another chance inside the attempt
// budget. ScheduleRunRetry's own budget check is what stops this looping.
func (s *Server) reArmRetry(ctx context.Context, retryStore store.RunRetryStore, ref mongostore.RetryDueRef, cause error) {
	maxAttempts := s.retryMaxAttempts(ctx, ref.ID)
	at := time.Now().UTC().Add(retryReenqueueBackoff)
	scheduled, _, err := retryStore.ScheduleRunRetry(ctx, ref.ID, at, retryReason(ref), retryCode(ref), maxAttempts)
	if err != nil {
		s.warnf("retry sweeper: re-arm %s: %v", ref.ID, err)
		return
	}
	if !scheduled {
		s.abandonRetry(ctx, retryStore, ref.TenantID, ref.ID, fmt.Sprintf("auto-retry gave up after a failed resume: %v", cause))
		return
	}
	s.countRetry("failed")
	s.auditRetry(ref, "run.retry.failed", map[string]any{"error": cause.Error()})
	s.warnf("retry sweeper: run %s: resume failed (%v) — retrying at %s", ref.ID, cause, at.Format(time.RFC3339))
}

// retryMaxAttempts reads the run's launch-time attempt budget. Falls back
// to the package default when the run predates the snapshot, so a re-arm is
// still bounded.
func (s *Server) retryMaxAttempts(ctx context.Context, runID string) int {
	r, err := s.cfg.Store.LoadRun(ctx, runID)
	if err != nil || r == nil || r.RetryPolicy == nil || r.RetryPolicy.MaxAttempts <= 0 {
		return retrypolicy.DefaultMaxAttempts
	}
	return r.RetryPolicy.MaxAttempts
}

func retryAttempts(ref mongostore.RetryDueRef) int {
	if ref.RetryState == nil {
		return 0
	}
	return ref.RetryState.Attempts
}

func retryReason(ref mongostore.RetryDueRef) string {
	if ref.RetryState == nil || ref.RetryState.Reason == "" {
		return "usage_window"
	}
	return ref.RetryState.Reason
}

func retryCode(ref mongostore.RetryDueRef) string {
	if ref.RetryState == nil {
		return ""
	}
	return ref.RetryState.Code
}

// retryLaunchCtx stamps the synthetic identity the admission gate reads.
// The run's own tenant and owner are used so the retry is metered against
// the same org as the original launch, not against the platform.
func retryLaunchCtx(ctx context.Context, ref mongostore.RetryDueRef) context.Context {
	return auth.WithIdentity(ctx, auth.Identity{TeamID: ref.TenantID, UserID: ref.OwnerID})
}

// auditRetry records a sweeper decision on the team's audit trail, keyed by
// run id, so "what happened to my Monday digest" is one query.
func (s *Server) auditRetry(ref mongostore.RetryDueRef, action string, meta map[string]any) {
	s.auditSystem(ref.TenantID, "retry-sweeper", action, "run", ref.ID, meta)
}

// countRetry records a sweeper outcome. Nil-safe: local mode has no
// metrics registry.
func (s *Server) countRetry(result string) {
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.RunsRetryResumed.WithLabelValues(result).Inc()
	}
}

func (s *Server) warnf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(format, args...)
	}
}

func (s *Server) infof(format string, args ...any) {
	if s.logger != nil {
		s.logger.Info(format, args...)
	}
}
