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
// Like every leased net here, the term ends on lease loss or shutdown, not on
// the work failing: a holder whose forge egress is broken keeps the lease
// while every refresh fails, and no peer covers. Handing over after a run of
// failed passes is the follow-up that belongs to lease.Run's callers as a
// class (#2685).
func (s *Server) runElectedForgeTokenRefresh(ctx context.Context, every time.Duration, sweep func(context.Context)) {
	err := lease.Run(ctx, s.leases, s.forgeRefreshLeaseSpec(every), func(ctx context.Context) {
		forgeRefreshPasses(ctx, every, sweep)
	})
	if err != nil {
		s.warnf("forge token refresh NOT started: %v — no forge connection token (oauth_app, github_app installation, security-read) and no project bot will be renewed", err)
	}
}

// forgeRefreshPasses sweeps once on entry, then on every tick, until ctx ends.
func forgeRefreshPasses(ctx context.Context, every time.Duration, sweep func(context.Context)) {
	sweep(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep(ctx)
		}
	}
}

// forgeRefreshLeaseSpec paces the lease on the shorter of the sweep tick and
// forgeRefreshLeasePace: a test that ticks in milliseconds elects in
// milliseconds, production elects by the minute under a ten-minute sweep.
func (s *Server) forgeRefreshLeaseSpec(every time.Duration) lease.Spec {
	pace := min(every, forgeRefreshLeasePace)
	return lease.Spec{
		Name:  leaseForgeTokenRefresh,
		Owner: s.replicaID,
		TTL:   forgeRefreshLeaseTTLFactor * pace,
		Retry: pace,
		Info:  s.infof,
		Warn:  s.warnf,
	}
}
