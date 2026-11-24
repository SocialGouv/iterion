package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The key pools' elected signal reconciler (ADR-164): one replica refreshes
// the ATTACHED pools' OAuth entries' usage readings on its own period,
// under lease — the price-feed sweep's sibling, for the same reason: the
// launch-path margin predicate reads the SHARED readings ledger, and a
// warm reading at launch costs no probe latency. Every other shape is
// reactive (no provider telemetry today: plain keys, gateway keys, the
// facade providers) and is skipped here exactly as the launch path skips
// it — the preview says "reactive" about them, and no poller may pretend
// otherwise.
//
// The sweep is idempotent: a reading already warm (within its trust
// window) is left alone, so a slow pass and a fast one converge on the
// same ledger. One failed probe logs and moves on — the lease health's
// K-failing-pass handover judges the TERM on the sweep's verdict, and one
// dead credential must not end it for the others.

// poolSignalSyncInterval is the reconciler's pass tick. The price-feed
// sibling runs at one minute because feeds carry their own periods; pool
// readings move on the providers' windows (five hours, seven days), so
// five minutes keeps them warm at a fraction of the probe traffic.
const poolSignalSyncInterval = 5 * time.Minute

// poolProbeTimeout bounds one subscription probe — the launch path's own
// bound, carried here so a hung provider costs a pass one timeout, not a
// term.
const poolProbeTimeout = 8 * time.Second

// runElectedPoolSignalSync is the lease campaign around the sweep. The
// election is inside, not around, so there is no unelected entry point to
// start by mistake.
func (s *Server) runElectedPoolSignalSync(ctx context.Context) {
	health := s.newLeaseHealth()
	err := lease.Run(ctx, s.leases, lease.Spec{
		Name:   leasePoolSignalSync,
		Owner:  s.replicaID,
		TTL:    3 * poolSignalSyncInterval,
		Retry:  poolSignalSyncInterval,
		Info:   s.infof,
		Warn:   s.warnf,
		Health: health,
	}, func(ctx context.Context) { s.poolSignalSyncPasses(ctx, health) })
	if err != nil && ctx.Err() == nil {
		s.warnf("pool signal sync: the lease campaign ended: %v", err)
	}
}

// poolSignalSyncPasses runs one pass at term start, then on every tick.
func (s *Server) poolSignalSyncPasses(ctx context.Context, health *lease.Health) {
	s.poolSignalSyncPass(ctx, health)
	ticker := time.NewTicker(poolSignalSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.poolSignalSyncPass(ctx, health)
		}
	}
}

func (s *Server) poolSignalSyncPass(ctx context.Context, health *lease.Health) {
	health.Observe(s.poolSignalSyncSweep(ctx))
}

// poolSignalSyncSweep walks every ENABLED attachment's pool and refreshes
// the OAuth-forfait entries whose readings went stale. Probes for exactly
// the shapes that carry proactive signal (claude_code today — the
// Anthropic usage endpoint); the margin predicate consumes what lands.
// The verdict follows the price-feed sibling's discipline: a store failure
// fails the pass, a successful probe progresses it, and a pass that found
// nothing warm-stale is idle.
func (s *Server) poolSignalSyncSweep(ctx context.Context) lease.PassVerdict {
	var pass lease.Pass
	// The pool set = stored attachments' pools PLUS schedule-bound pools
	// (ADR-164): a pool reached only through a schedule's pool: block has
	// no attachment row, and leaving it out would leave the margin skip
	// silently inert on the schedule lane — the reconcile exists to warm
	// exactly what the launch-path margin predicate reads.
	poolIDs := map[string]bool{}
	atts, err := s.credPoolAttachments.List(ctx)
	if err != nil {
		s.warnf("pool signal sync: list attachments: %v", err)
		pass.FailedUnlessDone(ctx, err)
		return pass.Verdict()
	}
	for _, att := range atts {
		if att.Enabled {
			poolIDs[att.PoolID] = true
		}
	}
	// The binding-level blocks the stored attachments cannot see: a
	// schedule row's, and a forge integration's on its webhook config (the
	// block the webhook and card lanes fold). A pool reached only through
	// one of them still needs warm readings, or its margin skip is inert on
	// that lane. A failed read is named and skipped — it must not drop the
	// attachments' warmth.
	if (s.scheduledBots != nil || s.webhookConfigs != nil) && s.authStore() != nil {
		teams, terr := identity.AllTeams(ctx, s.authStore())
		if terr != nil {
			s.warnf("pool signal sync: list teams for the binding-level pools: %v", terr)
		}
		for _, tm := range teams {
			if s.scheduledBots != nil {
				scheds, serr := s.scheduledBots.ListByTenant(ctx, tm.ID)
				if serr != nil {
					s.warnf("pool signal sync: list team %s's schedules: %v", tm.ID, serr)
				}
				for _, sb := range scheds {
					if sb.Pool != nil && sb.Pool.Enabled && sb.Pool.PoolID != "" {
						poolIDs[sb.Pool.PoolID] = true
					}
				}
			}
			if s.webhookConfigs != nil {
				cfgs, werr := s.webhookConfigs.ListByTenant(ctx, tm.ID)
				if werr != nil {
					s.warnf("pool signal sync: list team %s's webhook configs: %v", tm.ID, werr)
				}
				for _, c := range cfgs {
					if c.Enabled && c.Pool != nil && c.Pool.Enabled && c.Pool.PoolID != "" {
						poolIDs[c.Pool.PoolID] = true
					}
				}
			}
		}
	}
	for poolID := range poolIDs {
		pledges, err := s.credPoolPledges.ListByPool(ctx, poolID)
		if err != nil {
			s.warnf("pool signal sync: list pool %s's entries: %v", poolID, err)
			pass.FailedUnlessDone(ctx, err)
			continue
		}
		att := credpool.Attachment{PoolID: poolID}
		for _, pl := range pledges {
			// Reactive shapes are skipped honestly — no provider telemetry
			// today, no poller pretending otherwise (ADR-164 § signals).
			if pl.Source != credpool.SourceOAuth ||
				!strings.EqualFold(pl.Ref, string(secrets.OAuthKindClaudeCode)) {
				continue
			}
			if ctx.Err() != nil {
				return pass.Verdict()
			}
			refreshed, err := s.poolSignalProbeEntry(ctx, att, pl)
			if err != nil {
				s.warnf("pool signal sync: probe %s: %v", pl.ID, err)
				pass.FailedUnlessDone(ctx, err)
				continue
			}
			if refreshed {
				pass.SucceededUnlessDone(ctx)
			}
		}
	}
	return pass.Verdict()
}

// poolSignalProbeEntry refreshes one entry's readings: a warm ledger is
// left alone, a stale one is probed through the subscription's sealed
// payload and recorded where the margin predicate reads it. Returns
// whether a probe ran.
func (s *Server) poolSignalProbeEntry(ctx context.Context, att credpool.Attachment, pl credpool.Pledge) (bool, error) {
	rec, err := s.oauthStore.Get(ctx, pl.UserID, secrets.OAuthKind(pl.Ref))
	if errors.Is(err, secrets.ErrOAuthNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if rec.Fingerprint == "" {
		return false, nil
	}
	backend := cloudpublisher.UsageBackendForKind(rec.Kind)
	if backend == "" {
		return false, nil
	}
	// The scope collapses on an account fingerprint: one subscription, one
	// meter, wherever it lends from.
	key := usagecap.Key(backend, usagecap.ScopePlatform, rec.Fingerprint)
	readings, err := s.usageCaps.Latest(ctx, key)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	for _, r := range readings {
		if r.Fresh(now, usagecap.Trust{}) {
			return false, nil
		}
	}
	payload, err := secrets.OpenOAuthPayload(s.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		// A record we cannot open is the donor's to fix (a reconnect
		// re-seals it); the sweep moves on and the reactive cooldowns the
		// launch path already applies stay the guard.
		s.warnf("pool signal sync: pool %s entry %s cannot unseal its subscription: %v", att.PoolID, pl.ID, err)
		return false, nil
	}
	pctx, cancel := context.WithTimeout(ctx, poolProbeTimeout)
	defer cancel()
	fresh, err := s.poolUsageProbe(pctx, payload)
	if err != nil {
		s.warnf("pool signal sync: pool %s entry %s probe failed: %v", att.PoolID, pl.ID, err)
		return false, nil
	}
	for _, r := range fresh {
		if err := s.usageCaps.Record(ctx, key, r); err != nil {
			s.warnf("pool signal sync: could not record %s's reading: %v", key, err)
		}
		if r.Status == usagecap.StatusRejected || r.Utilization*100 >= credpool.DefaultMarginPct {
			s.infof("pool signal sync: pool %s entry %s reads %s at %.0f%%, window resets %s",
				att.PoolID, pl.ID, r.Status, r.Utilization*100, r.ResetsAt.Format(time.RFC3339))
		}
	}
	return true, nil
}
