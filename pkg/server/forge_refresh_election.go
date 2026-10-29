package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
)

// forgeRefreshLeasePace bounds how long a dead holder keeps the refresh down
// and how often the others campaign: a token's refresh lead is minutes, so
// the lease is paced in minutes too, whatever the sweep's own tick.
const forgeRefreshLeasePace = time.Minute

// forgeRefreshLeaseTTLFactor is the TTL in paces, like the other elected
// sweeps.
const forgeRefreshLeaseTTLFactor = 3

// runElectedForgeTokenRefresh runs the forge token refresh sweep on the ONE
// replica holding its lease, and campaigns for it on every other. Every
// replica sweeping was the race of #2682: a refresh token the forge rotates
// (GitLab, Forgejo) is consumed by whoever refreshes first, the next replica's
// refresh with the same token is answered invalid_grant, and a healthy
// connection ended needs_reauth. Each term opens with a sweep, for the reason
// the per-replica loop opened with one: a rolling deploy re-phases every
// ticker, and a token about to be refreshed must not sit dying until the
// first tick of a new term.
//
// The term also ends on the WORK failing: a holder whose forge egress is
// broken used to keep the lease while every refresh failed with a healthy
// fleet standing by — the defect of #2685, whose blast radius here is every
// GitHub-App installation token (1 h) and GitLab access token (2 h) the sweep
// renews. The sweep's own verdict is what ends it (lease.Health), never the
// absence of progress: a pass that finds nothing due is the normal state of a
// fleet whose tokens are all fresh.
func (s *Server) runElectedForgeTokenRefresh(ctx context.Context, every time.Duration, sweep func(context.Context) lease.PassVerdict) {
	spec := s.forgeRefreshLeaseSpec(every)
	err := lease.Run(ctx, s.leases, spec, func(ctx context.Context) {
		forgeRefreshPasses(ctx, every, spec.Health, sweep)
	})
	if err != nil {
		s.warnf("forge token refresh NOT started: %v — no forge connection token (oauth_app, github_app installation, security-read) and no project bot will be renewed", err)
	}
}

// forgeRefreshPasses sweeps once on entry, then on every tick, until ctx ends
// — or until the health says to hand over, in which case returning here is
// what releases the lease (lease.Run) so a peer campaigns.
func forgeRefreshPasses(ctx context.Context, every time.Duration, health *lease.Health, sweep func(context.Context) lease.PassVerdict) {
	if health.Observe(sweep(ctx)) {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if health.Observe(sweep(ctx)) {
				return
			}
		}
	}
}

// forgeRefreshLeaseSpec paces the lease on the shorter of the sweep tick and
// forgeRefreshLeasePace: a test that ticks in milliseconds elects in
// milliseconds, production elects by the minute under a ten-minute sweep.
func (s *Server) forgeRefreshLeaseSpec(every time.Duration) lease.Spec {
	pace := min(every, forgeRefreshLeasePace)
	return lease.Spec{
		Name:   leaseForgeTokenRefresh,
		Owner:  s.replicaID,
		TTL:    forgeRefreshLeaseTTLFactor * pace,
		Retry:  pace,
		Info:   s.infof,
		Warn:   s.warnf,
		Health: s.newLeaseHealth(),
	}
}
