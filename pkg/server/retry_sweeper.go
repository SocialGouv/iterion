package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/retrycoord"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
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
	// ListRunsForRetryProbe is the other half of the same capability: the
	// not-yet-due rows the due scan structurally cannot see (retry_probe.go).
	ListRunsForRetryProbe(ctx context.Context, now time.Time, floor time.Duration, maxProbes, limit int) ([]mongostore.RetryDueRef, error)
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
	bounds := retryProbeBoundsFromEnv()
	s.infof("retry sweeper: watching for runs whose provider quota window has reopened (every %s, up to %d per pass; probing not-yet-due waits every %s, %d per wait)",
		retrySweepInterval, retrySweepBatch, bounds.floor, bounds.budget)
	t := time.NewTicker(retrySweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepDueRetries(ctx, lister, s.runs, time.Now().UTC())
			// Second pass, after the due one: what it pulls forward is
			// resumed by the due pass on the next tick, never here.
			s.sweepRetryProbes(ctx, lister, time.Now().UTC())
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

	// A DLQ park is final for automation — only an operator's resume wakes
	// it (its replay is refused) — and the gate reconciler has already
	// answered for it. The transition tail disarms retry_after on every final write
	// (statusTransitionSet / applyStatusTransitionOutcome), so an armed
	// retry on a parked doc is one that predates that rule or bypassed the
	// tail; resuming it would run the bot a second time on the same head,
	// sharing the reconciler's gate context. Disarm it and say so. The
	// park's own outcome event already reached every consumer, so this
	// path must NOT replay it (the DLQ notice has no dedup of its own).
	// The read is a guard, not a gate: a store that cannot answer leaves
	// the retry row as the authority, loudly.
	run, lerr := s.cfg.Store.LoadRun(runCtx, ref.ID)
	if lerr != nil {
		s.warnf("retry sweeper: run %s: cannot read the run doc before resuming its retry (%v) — proceeding on the retry row alone: a DLQ-parked run whose stale retry survived would be resumed on top of the gate reconciler's repair", ref.ID, lerr)
	} else if run != nil && run.FailureCode == store.FailureDLQParked {
		s.disarmRetry(runCtx, retryStore, ref.TenantID, ref.ID,
			"auto-retry abandoned: the run is parked on the DLQ (DLQ_PARKED) — its armed retry was stale; only an operator's resume (iterion remote runs resume) wakes it")
		return
	}

	// An automatic resume spends real money, so it passes the same
	// admission gate as the operator-initiated one. Whether a denial ends
	// the retry depends on the denial: a monthly quota refills on the 1st
	// and a concurrency/rate cap clears in minutes, so abandoning on those
	// would throw the run away for a condition that resolves itself. A
	// suspended org or a missing workspace needs a human.
	// The run doc read above already carries what the gate needs to know
	// about this launch, so pass it: a retry judged as ordinary work would
	// face the ceiling its OWN reservation lowered, and the reservation would
	// refuse the run it exists to protect. Nil (the store could not answer)
	// falls back to the ordinary subject, like every other unknown.
	var subj launchSubject
	if run != nil {
		subj = launchSubject{BotID: run.BotID, Repo: run.ProjectPath}
	}
	adm, deny := s.gateLaunch(retryLaunchCtx(runCtx, ref), subj)
	if deny != nil {
		if retryDenialIsTransient(deny.reason) {
			s.reArmRetry(runCtx, retryStore, ref, fmt.Errorf("admission deferred: %s", deny.reason))
			return
		}
		s.abandonRetry(runCtx, retryStore, ref.TenantID, ref.ID, fmt.Sprintf("auto-retry abandoned: %s", deny.reason))
		return
	}

	// Admission comes before the provider circuit: a permanently denied run
	// must be abandoned now, and a retry re-armed for another transient reason
	// must not be held behind an unrelated provider window.
	if key := retrycoord.Key(run); key != "" {
		decisionNow := time.Now().UTC()
		circuit, circuitErr := retrycoord.Open(runCtx, s.cfg.Store, key, decisionNow)
		if circuitErr != nil {
			s.warnf("retry sweeper: run %s: cannot read retry circuit (%v) — proceeding with the per-run retry", ref.ID, circuitErr)
		} else if delayedUntil, shouldDelay := circuitRetryDelayAt(ref, run, circuit, decisionNow); shouldDelay {
			delayed, delayErr := retryStore.DelayRunRetry(runCtx, ref.ID, ref.RetryAfter(), delayedUntil)
			if delayErr != nil {
				adm.rollback(s.logger)
				s.warnf("retry sweeper: run %s: cannot delay retry behind open circuit: %v", ref.ID, delayErr)
				return
			}
			if delayed {
				s.auditRetry(ref, "run.retry.circuit_delayed", map[string]any{"retry_after": delayedUntil})
				s.infof("retry sweeper: run %s delayed behind workflow circuit until %s", ref.ID, delayedUntil.Format(time.RFC3339))
			}
			adm.rollback(s.logger)
			return
		}
	}

	filePath, source, lb, err := s.resolveResumeSource(runCtx, ref.BotSourceTenant, ref.FilePath, "", "", run)
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
		// Machinery resume: a cancelled doc is refused even with retry_after
		// still set on the record — the SubmitResume CAS would otherwise
		// flip cancelled → queued (clearing run.Error) and the runner would
		// receive a message with no PR-closed marker left on the doc.
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
		// attempt budget and surface the error verbatim. The metered slot
		// follows the error's own fact (#1725): a resume whose publish
		// landed and then reported failure keeps the slot — the runner may
		// already have claimed that message, and a resume meters an
		// execution attempt by design (the same charge handleResumeRun
		// makes). Only a resume proven not to have started hands it back.
		if !runview.RunMayHaveStarted(err) {
			adm.rollback(s.logger)
		}
		// A pool re-mapping (ErrPoolRemapped) is permanent — no retry cures
		// it, and re-arming would flip the doc queued→failed every cycle
		// until the budget burned. Abandon with the named reason instead.
		if errors.Is(err, cloudpublisher.ErrPoolRemapped) {
			// No second rollback: ErrPoolRemapped leaves BEFORE any publish,
			// so RunMayHaveStarted is false and the refund above already
			// fired — and ReleaseRun is a non-idempotent $inc (rva-2 F1).
			s.abandonRetry(runCtx, retryStore, ref.TenantID, ref.ID,
				"auto-retry abandoned: the team's runner-pool mapping moved past the run's frozen pool — relaunch the run on the new pool")
			return
		}
		s.reArmRetry(runCtx, retryStore, ref, err)
		return
	}

	// A PROBE's resume is speculative by construction: the preflight it
	// exists to ask refuses before anything spends, so it is not a billable
	// launch. gateLaunch's last step is also the metering increment, and
	// adm.rollback is the only refund — the branches above call it on
	// FAILURE, which a resume that succeeded never reaches: the refusal
	// happens later, inside the pod, where no admission handle exists. Left
	// charged, one parked wait spends its whole probe budget of the org's
	// monthly run quota (plus one per-minute launch token each) on launches
	// whose refusal is EXPECTED, and the tenant's real launches are denied
	// 402 until the month rolls over. The residual under-count — a probe
	// that finds the window genuinely reopened — is one run the original
	// launch already metered, once per probed wait: the park that follows
	// that run's real work spends an attempt, and a paid arming clears the
	// probe ledger (store/mongo.ScheduleRunRetry), so the resumes after it
	// are metered again. A marker left over from a pull whose own resume is
	// long gone names no probe in flight (store.ProbeInFlight bounds it by the
	// age of the pull): that row is due on its own account and its resume is
	// an ordinary billable launch.
	if ref.RetryState.ProbeInFlight(time.Now().UTC()) {
		adm.rollback(s.logger)
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

// circuitRetryDelayAt spreads a circuit-delayed cohort while preserving the
// per-run max_wait horizon across repeated sweeper passes. Once that horizon
// has elapsed the circuit becomes advisory and the already-due retry proceeds.
func circuitRetryDelayAt(ref mongostore.RetryDueRef, run *store.Run, circuit *store.RetryCircuitState, now time.Time) (time.Time, bool) {
	if circuit == nil || circuit.OpenUntil == nil || !circuit.OpenUntil.After(now) {
		return time.Time{}, false
	}
	pol := retryPolicyForSweeper(run)
	delayedUntil := circuit.OpenUntil.UTC()
	if jitter := pol.JitterDuration(); jitter > 0 {
		delayedUntil = delayedUntil.Add(rand.N(jitter))
	}
	anchor := ref.RetryAfter().UTC()
	// On a probe-pulled row the armed instant is "now", so the legacy
	// fallback above would measure the horizon from the probe instead of from
	// the wait the probe interrupted — the announced instant it set aside is
	// that row's real origin.
	if probed := ref.RetryState.AnnouncedProbeFrom(now); probed != nil {
		anchor = probed.UTC()
	}
	if ref.RetryState != nil && ref.RetryState.ScheduledAt != nil {
		anchor = ref.RetryState.ScheduledAt.UTC()
	}
	if run != nil && run.RetryState != nil && run.RetryState.ScheduledAt != nil {
		anchor = run.RetryState.ScheduledAt.UTC()
	}
	ceiling := anchor.Add(pol.MaxWaitDuration())
	if !ceiling.After(now) {
		return time.Time{}, false
	}
	if delayedUntil.After(ceiling) {
		delayedUntil = ceiling
	}
	return delayedUntil, delayedUntil.After(now)
}

func retryPolicyForSweeper(run *store.Run) retrypolicy.Policy {
	var pol retrypolicy.Policy
	if run != nil && run.RetryPolicy != nil {
		pol = retrypolicy.Policy{
			UsageWindow: run.RetryPolicy.UsageWindow,
			MaxAttempts: run.RetryPolicy.MaxAttempts,
			MaxWait:     run.RetryPolicy.MaxWait,
			Jitter:      run.RetryPolicy.Jitter,
		}
	}
	return retrypolicy.Clamp(retrypolicy.Normalize(pol), retrypolicy.CeilingFromEnv(), nil)
}

// retryDenialIsTransient reports whether an admission denial is expected to
// clear on its own, so the retry should be deferred rather than dropped.
// Unknown reasons are treated as PERMANENT: a new denial code that silently
// re-armed forever would be worse than one that stops and says why.
func retryDenialIsTransient(reason string) bool {
	switch reason {
	// denyRepoQuota belongs with the other MONTHLY ceilings: it refills on
	// the 1st like they do, so abandoning the run on it would throw work
	// away for a condition that resolves itself on a calendar.
	case denyMonthlyRunQuota, denyMonthlyCostCap, denyConcurrencyCap, denyLaunchRateLimited, denyRepoQuota:
		return true
	default:
		return false
	}
}

// abandonRetry records a permanent stop, audits it, and republishes the
// run's outcome so the consumers that stood down on the armed retry get
// the event they were promised.
func (s *Server) abandonRetry(ctx context.Context, retryStore store.RunRetryStore, tenantID, runID, reason string) {
	if s.standDownProbedRetry(ctx, retryStore, tenantID, runID, reason) {
		return
	}
	if !s.disarmRetry(ctx, retryStore, tenantID, runID, reason) {
		return
	}

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

// disarmRetry is the abandon WITHOUT the outcome replay: the store write,
// the counter, the audit line and the log. False when the write did not
// land — the retry then stays claimable, the next sweep tick walks the
// same path again, and anything a caller would do on top (a republish)
// must wait for the tick that actually makes the stop permanent.
func (s *Server) disarmRetry(ctx context.Context, retryStore store.RunRetryStore, tenantID, runID, reason string) bool {
	if err := retryStore.AbandonRunRetry(ctx, runID, reason); err != nil {
		s.warnf("retry sweeper: abandon %s: %v", runID, err)
		return false
	}
	s.countRetry("abandoned")
	s.auditSystem(tenantID, "retry-sweeper", "run.retry.abandoned", "run", runID, map[string]any{"reason": reason})
	s.warnf("retry sweeper: run %s: %s", runID, reason)
	return true
}

// reArmRetry gives a failed resume another chance inside the attempt
// budget. ScheduleRunRetry's own budget check is what stops this looping.
func (s *Server) reArmRetry(ctx context.Context, retryStore store.RunRetryStore, ref mongostore.RetryDueRef, cause error) {
	if s.standDownProbedRetry(ctx, retryStore, ref.TenantID, ref.ID, cause.Error()) {
		return
	}
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

// retryLaunchCtx stamps the machine identity the admission gate reads. The
// run's own tenant and owner are used so the retry is metered against the
// same org as the original launch, not against the platform; the kind is
// what exempts it from the gate's member rung — an automatic resume carries
// no human standing, and the owner it names may since have left the team.
func retryLaunchCtx(ctx context.Context, ref mongostore.RetryDueRef) context.Context {
	return machineLaunchCtx(ctx, ref.TenantID, ref.OwnerID)
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
