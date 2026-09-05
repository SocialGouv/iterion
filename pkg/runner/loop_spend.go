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
	"github.com/SocialGouv/iterion/pkg/store"
)

// recordOrgSpend charges the run's accumulated LLM consumption to the
// org's monthly usage bucket AND bumps `last_used_at` on every API key
// the attempt held. Called at the end of every execution attempt —
// paused/cancelled/failed attempts incurred real spend too, and a
// redelivered attempt re-charges only what it re-executed. Detached ctx
// on both writes: a Mongo blip must not fail the run path; misses are
// logged and the Prometheus counters still carry the global totals.
//
// The bump is NOT behind the spend gate. RunTotals is a lossy signal —
// a delegate that streams no usage, a run refused at its first call —
// and the key was held for the whole attempt either way; gating the bump
// on it is what left `last_used_at` frozen for hours on a key that was
// serving (#659 pt 2). Bumped at attempt START too (injectCredentials);
// nothing moves it DURING a turn — there is no live per-call signal to
// key on, so a long attempt shows its start until it ends.
func (r *Runner) recordOrgSpend(ctx context.Context, msg *queue.RunMessage, usage *metricsEmitter) {
	now := time.Now().UTC()
	// Half 2 first: the held keys are a fact of the attempt, whatever it
	// measured.
	r.markCredFingerprintsUsed(ctx, msg, now)
	if usage == nil {
		return
	}
	costUSD, in, out := usage.RunTotals()
	spent := costUSD > 0 || in > 0 || out > 0
	if !spent {
		return
	}

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
}

// markCredFingerprintsUsed bumps `last_used_at` on every API key whose
// fingerprint sits in the run's injected credentials — at attempt start
// (from injectCredentials) and at attempt end (from recordOrgSpend).
// Best-effort: a missing store, empty fingerprints, or a store failure
// all quietly leave the observation on the floor rather than fail the
// run. Detached context (5s bound) so the metering path is unaffected by
// cancellation.
//
// Scope follows the key's TIER. A tenant's own key is bumped under the
// run's tenant, so another tenant that stored the byte-identical secret
// never sees its own key read as "in use" (the studio shows last_used_at
// as exactly that, before a rotate or delete). A platform-tier or
// pool-lent key is bumped WITHOUT a tenant filter: its row lives under the
// platform sentinel or in the donor's tenant, and it serves every tenant.
func (r *Runner) markCredFingerprintsUsed(ctx context.Context, msg *queue.RunMessage, at time.Time) {
	if r.cfg.ApiKeys == nil {
		return
	}
	creds, ok := secrets.CredentialsFromContext(ctx)
	if !ok || len(creds.Fingerprints) == 0 {
		return
	}
	// Only API-key slots: an OAuth slot's fingerprint (a subscription's
	// connect-time identity) lives in the OAuth store and would only
	// cost the api_keys collection a lookup that matches nothing.
	// Deduplicated: the update is idempotent, the round-trips are not
	// free. A fingerprint any slot holds from another tier is bumped
	// cross-tenant.
	crossTenant := map[string]bool{}
	fps := make([]string, 0, len(creds.Fingerprints))
	for slot, fp := range creds.Fingerprints {
		if secrets.OAuthKind(slot).Valid() || fp == "" {
			continue
		}
		if _, seen := crossTenant[fp]; !seen {
			fps = append(fps, fp)
		}
		crossTenant[fp] = crossTenant[fp] || creds.IsPlatformSourced(slot) || creds.IsPoolSourced(slot)
	}
	if len(fps) == 0 {
		return
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, fp := range fps {
		bctx := bg
		if !crossTenant[fp] && msg.TenantID != "" {
			bctx = store.WithTenant(bg, msg.TenantID)
		}
		if err := r.cfg.ApiKeys.MarkFingerprintUsed(bctx, fp, at); err != nil {
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
