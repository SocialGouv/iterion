package runner

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// recordOrgSpend charges the run's accumulated LLM consumption to the
// org's monthly usage bucket. Called at the end of every execution
// attempt — paused/cancelled/failed attempts incurred real spend too,
// and a redelivered attempt re-charges only what it re-executed.
// Detached ctx: a Mongo blip must not fail the run path; the miss is
// logged and the Prometheus counters still carry the global totals.
func (r *Runner) recordOrgSpend(msg *queue.RunMessage, usage *metricsEmitter) {
	if r.cfg.OrgUsage == nil || usage == nil || msg.TenantID == "" {
		return
	}
	costUSD, in, out := usage.RunTotals()
	if costUSD <= 0 && in <= 0 && out <= 0 {
		return
	}
	// Charge the same usage key the launch gate metered the run on:
	// the parent org (caps sum across the org's teams — charging the
	// team key instead leaves the org's cost-cap document at zero, so
	// the cap never trips in a multi-team org). OrgID is empty on
	// pre-orgid messages and org-less pre-backfill teams — both were
	// metered on the team key, so fall back to it.
	key := msg.OrgID
	if key == "" {
		key = msg.TenantID
	}
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.cfg.OrgUsage.AddSpend(bg, key, time.Now().UTC(), costUSD, in, out); err != nil {
		r.cfg.Logger.Warn("runner: org spend record for %s (run %s): %v", key, msg.RunID, err)
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
func (r *Runner) recordPoolSpend(msg *queue.RunMessage, usage *metricsEmitter, execErr error) {
	if r.cfg.CredPool == nil || usage == nil {
		return
	}
	costUSD, in, out := usage.RunTotals()
	condition, cooldownUntil := classifyPoolCondition(execErr, time.Now().UTC())
	bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.cfg.CredPool.Report(bg, msg.RunID, credpool.Outcome{
		CostUSD:       costUSD,
		InputTokens:   in,
		OutputTokens:  out,
		Condition:     condition,
		CooldownUntil: cooldownUntil,
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
	var auth *delegate.ErrAuthFailed
	if errors.As(execErr, &auth) {
		return credpool.ConditionAuthFailed, time.Time{}
	}
	return credpool.ConditionOK, time.Time{}
}
