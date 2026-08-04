package server

import (
	"context"
	"time"
)

// Abandoned-lease sweeper.
//
// A run that draws on the credential pool holds a lease against its donor
// for as long as it runs — that lease IS the donor's concurrency reading.
// A runner pod killed at the wrong instant (eviction, OOM, a node going
// away) never reports, so without a sweeper the donor would keep a slot
// consumed by a run that no longer exists, and a contributor who offered
// "one run at a time" would silently stop being able to serve any.
//
// Each lease carries its own expiry, so the sweep is a plain scan: close
// anything live and past its instant. No spend is charged for those runs —
// nothing told us what they consumed, and inventing a figure would
// misreport a contributor's donation in the direction that costs them.
//
// Multi-replica-safe: closing a lease is conditional on it still being
// open, so two replicas sweeping the same instant produce one close.

const (
	// credPoolSweepInterval is how often abandoned leases are collected.
	// Frequent enough that a donor's slot comes back within minutes of the
	// lease expiring, cheap enough to be irrelevant (one indexed query).
	credPoolSweepInterval = 2 * time.Minute
	// credPoolSweepBatch bounds one pass so a large backlog is drained
	// over several ticks rather than in one long transaction.
	credPoolSweepBatch = 200
)

// runCredPoolSweeper loops until ctx is cancelled. Started by
// ListenAndServe when a credential pool is wired.
func (s *Server) runCredPoolSweeper(ctx context.Context) {
	t := time.NewTicker(credPoolSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepCredPoolLeases(ctx)
		}
	}
}

// sweepCredPoolLeases performs one pass. Extracted for tests.
func (s *Server) sweepCredPoolLeases(ctx context.Context) {
	if s.credPool == nil {
		return
	}
	freed, err := s.credPool.ReleaseExpired(ctx, credPoolSweepBatch)
	if err != nil {
		s.logger.Warn("credential pool: sweep abandoned leases: %v", err)
		return
	}
	if freed > 0 {
		s.logger.Info("credential pool: freed %d abandoned lease(s) — their donors can serve again", freed)
	}
}
