package runner

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// recordOrgSpend charges the run's accumulated LLM consumption to the
// org's monthly usage bucket AND bumps `last_used_at` on every credential
// the run actually spent tokens on. Called at the end of every execution
// attempt — paused/cancelled/failed attempts incurred real spend too,
// and a redelivered attempt re-charges only what it re-executed.
// Detached ctx on both writes: a Mongo blip must not fail the run path;
// misses are logged and the Prometheus counters still carry the global
// totals. The `last_used_at` bump reads the credential fingerprints out
// of ctx (secrets.WithCredentials was set by injectCredentials), so it
// keys on what the delegate actually spent — not what was granted at
// launch, which is the mute-frozen-at-launch signal the previous shape
// left an operator with (#659 pt 2).
func (r *Runner) recordOrgSpend(ctx context.Context, msg *queue.RunMessage, usage *metricsEmitter) {
	if usage == nil {
		return
	}
	costUSD, in, out := usage.RunTotals()
	spent := costUSD > 0 || in > 0 || out > 0
	if !spent {
		return
	}
	now := time.Now().UTC()

	// Half 1: org usage bucket — the existing behaviour.
	if r.cfg.OrgUsage != nil && msg.TenantID != "" {
		// Charge the same usage key the launch gate metered the run on:
		// the parent org (caps sum across the org's teams — charging the
		// team key instead leaves the org's cost-cap document at zero,
		// so the cap never trips in a multi-team org). OrgID is empty on
		// pre-orgid messages and org-less pre-backfill teams — both were
		// metered on the team key, so fall back to it.
		key := msg.OrgID
		if key == "" {
			key = msg.TenantID
		}
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.cfg.OrgUsage.AddSpend(bg, key, now, costUSD, in, out); err != nil {
			r.cfg.Logger.Warn("runner: org spend record for %s (run %s): %v", key, msg.RunID, err)
		}
		cancel()
	}

	// Half 2: per-credential last_used_at bump. Best-effort: nothing to
	// bump when the bundle carried no fingerprints (legacy run, env
	// fallback) or the ApiKey store is not wired.
	r.markCredFingerprintsUsed(ctx, msg, now)
}

// markCredFingerprintsUsed bumps `last_used_at` on every API key whose
// fingerprint sits in the run's injected credentials. Best-effort: a
// missing store, empty fingerprints, or a store failure all quietly
// leave the observation on the floor rather than fail the run. Detached
// context (5s bound) so the metering path is unaffected by cancellation.
func (r *Runner) markCredFingerprintsUsed(ctx context.Context, msg *queue.RunMessage, at time.Time) {
	if r.cfg.ApiKeys == nil {
		return
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok || len(creds.Fingerprints) == 0 {
		return
	}
	// Deduplicate: several slots may map to the same fingerprint (an
	// OAuth kind and the anthropic API key both keyed on the same
	// account) and the update is idempotent, but the DB round-trips
	// are not free.
	seen := map[string]bool{}
	fps := make([]string, 0, len(creds.Fingerprints))
	for _, fp := range creds.Fingerprints {
		if fp != "" && !seen[fp] {
			seen[fp] = true
			fps = append(fps, fp)
		}
	}
	if len(fps) == 0 {
		return
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, fp := range fps {
		if err := r.cfg.ApiKeys.MarkFingerprintUsed(bg, fp, at); err != nil {
			r.cfg.Logger.Warn("runner: mark api-key fingerprint used (run %s fp=%s): %v", msg.RunID, fp, err)
		}
	}
}

// recordPoolSpend closes the run's credential-pool lease: it charges the
// lending contributor's ledger, frees their concurrency slot, and reports
// the two conditions that must change their availability.
//
// A no-op for the vast majority of runs, which hold no lease — the broker
// looks the run up by id and returns quietly when there is none. Detached
// ctx and best-effort for the same reason as recordOrgSpend: accounting
// must never turn a finished run into a failed one. The lease's own TTL
// plus the server-side sweeper are the backstop if this write is lost.
// interim says this attempt does NOT settle the run: the queue will
// redeliver the same sealed bundle, so the next pod runs on this very
// lease. Decided by the caller, which is the only place the delivery's
// real disposition is known — a run on its last permitted delivery is
// parked, not redelivered.
func (r *Runner) recordPoolSpend(msg *queue.RunMessage, usage *metricsEmitter, execErr error, interim bool) {
	if r.cfg.CredPool == nil || usage == nil {
		return
	}
	costUSD, in, out := usage.RunTotals()
	condition, cooldownUntil := classifyPoolCondition(execErr, time.Now().UTC())
	// An auth rejection the recovery machinery absorbed into a human pause
	// leaves execErr saying only "paused". Without this the donor's dead
	// credential stays first in the rotation and pauses the next run too,
	// costing them a unit of their daily quota every time.
	if condition == credpool.ConditionOK && usage.SawAuthFailure() {
		condition = credpool.ConditionAuthFailed
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.cfg.CredPool.Report(bg, msg.RunID, credpool.Outcome{
		CostUSD:       costUSD,
		InputTokens:   in,
		OutputTokens:  out,
		Condition:     condition,
		CooldownUntil: cooldownUntil,
		Interim:       interim,
	}); err != nil {
		r.cfg.Logger.Warn("runner: credential-pool report for run %s: %v (the donor's slot frees on lease expiry)", msg.RunID, err)
	}
}

// classifyPoolCondition maps a terminal execution error onto what it means
// for the credential that produced it. The backend error types live in
// pkg/backend/delegate, so this translation belongs at the runner boundary
// rather than inside pkg/credpool.
//
// Everything that is neither a quota window nor a rejected credential is
// ConditionOK: a workflow that failed on its own logic says nothing about
// the donor, and must not cost them their place in the rotation.
func classifyPoolCondition(execErr error, now time.Time) (credpool.Condition, time.Time) {
	if execErr == nil {
		return credpool.ConditionOK, time.Time{}
	}
	// Same evidence chain the usage-window retry arms on, including its
	// text-parsed fallback — a runner with no dispatcher classifies
	// nothing, and the pool must still rest the donor rather than send the
	// next run into the same shut window.
	if at, _, ok := usageWindowEvidence(execErr); ok {
		if at.IsZero() {
			if parsed, pok := delegate.ParseResetHint(execErr.Error(), now); pok {
				at = parsed
			}
		}
		return credpool.ConditionUsageWindow, at
	}
	// Asked through recovery.Classify rather than a local type switch: it
	// is the engine's single source of truth for "the provider rejected
	// this credential", and it recognises the raw 401/403 an in-process
	// backend surfaces as well as the typed ErrAuthFailed the CLI ones
	// raise. A local check for the type alone missed claw entirely.
	if recovery.Classify(execErr) == runtime.ErrCodeAuthFailed {
		return credpool.ConditionAuthFailed, time.Time{}
	}
	return credpool.ConditionOK, time.Time{}
}
