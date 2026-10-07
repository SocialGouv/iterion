package runner

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/orgusage"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// spendWriteTimeout bounds each accounting write the runner makes for an
// attempt, detached from the run ctx.
const spendWriteTimeout = 5 * time.Second

// orgSpendBudget bounds recordOrgSpend: its three writes — the keys'
// last use, the per-credential ledger, the org's bucket — one after the
// other, each on its own bound.
const orgSpendBudget = 3 * spendWriteTimeout

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
	r.warnUnreportedUsage(msg, usage)
	// The per-CREDENTIAL ledger, charged per (backend, model) route rather
	// than from the run total — the same attempt, read by credential
	// instead of by org (#641). Independent of the org gate below: a route
	// the org bucket cannot break apart is exactly what this answers.
	r.recordCredentialSpend(ctx, msg, usage, now)
	costUSD, in, out, aggregate := usage.RunTotals()
	spent := costUSD > 0 || in > 0 || out > 0 || aggregate > 0
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
		bg, cancel := context.WithTimeout(context.Background(), spendWriteTimeout)
		if err := r.cfg.OrgUsage.AddSpend(bg, orgusage.OrgSubject(key), now, costUSD, in, out, aggregate); err != nil {
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
func (r *Runner) warnUnreportedUsage(msg *queue.RunMessage, usage *metricsEmitter) {
	if r.cfg.Logger == nil {
		return
	}
	for route, totals := range usage.RouteTotals() {
		if totals.unreportedCalls > 0 {
			r.cfg.Logger.Warn("runner: run %s made %d LLM call(s) on %s/%s whose usage the provider did not report — booked at %d tokens, a lower bound",
				msg.RunID, totals.unreportedCalls, route.backend, route.model, totals.tokens())
		}
	}
}

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
		crossTenant[fp] = crossTenant[fp] || !creds.IsTenantOwned(slot)
	}
	if len(fps) == 0 {
		return
	}
	bg, cancel := context.WithTimeout(context.Background(), spendWriteTimeout)
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
	if errors.Is(execErr, runtime.ErrResumeSuperseded) {
		// The run's open lease is the newer attempt's: this delivery ran
		// nothing, and its report would close that lease under it.
		return
	}
	if msg.PoolGrantless {
		// A grantless delivery holds no pool lease: its spend belongs to
		// no donor, and its report must never reach the broker — it would
		// be charged to the lease of the attempt this publication took the
		// run from.
		return
	}
	costUSD, in, out, aggregate := usage.RunTotals()
	condition, cooldownUntil := classifyPoolCondition(execErr, time.Now().UTC())
	// An auth rejection the recovery machinery absorbed into a human pause
	// leaves execErr saying only "paused". Without this the donor's dead
	// credential stays first in the rotation and pauses the next run too,
	// costing them a unit of their daily quota every time.
	if condition == credpool.ConditionOK && usage.SawAuthFailure() {
		condition = credpool.ConditionAuthFailed
	}
	bg, cancel := context.WithTimeout(context.Background(), spendWriteTimeout)
	defer cancel()
	// The report carries this delivery's attempt identity — its
	// publication — so a superseding acquisition's lease is never closed or
	// charged by a previous attempt's late teardown report. A publication
	// that cannot be read falls back to the open lease inside the broker.
	publishedAt, perr := time.Parse(time.RFC3339Nano, msg.PublishedAtRFC)
	if perr != nil {
		publishedAt = time.Time{}
	}
	// NO fine table rides yet (ADR-121 § Delivery 2, #2255): the emitter's
	// route key names a CHANNEL (providerFingerprint's routing label), not
	// a credential - the run's own OAuth and a donor's lent OAuth stamp the
	// same label, so a table keyed on it cannot tell whose money spent, and
	// the broker would book zero on every door-served run (revi R30d246).
	// The coarse whole-attempt booking stands until the publisher stamps a
	// node-to-credential-fingerprint plan into the message; the broker's
	// fine consumer (Outcome.ByFingerprint + effectiveCharge) is shipped
	// and tested, waiting for that producer.
	if err := r.cfg.CredPool.ReportAttempt(bg, msg.RunID, publishedAt, credpool.Outcome{
		CostUSD:         costUSD,
		InputTokens:     in,
		OutputTokens:    out,
		AggregateTokens: aggregate,
		Condition:       condition,
		CooldownUntil:   cooldownUntil,
		Interim:         interim,
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
