package runner

import (
	"context"
	"time"

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
