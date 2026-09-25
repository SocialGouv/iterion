package credpool

import (
	"context"
	"testing"
	"time"
)

type interleavedLeaseStore struct {
	LeaseStore
	afterFirstRead func()
}

func (s *interleavedLeaseStore) ListOpenByRun(ctx context.Context, runID string) ([]Lease, error) {
	open, err := s.LeaseStore.ListOpenByRun(ctx, runID)
	if after := s.afterFirstRead; after != nil {
		s.afterFirstRead = nil
		after()
	}
	return open, err
}

func (s *interleavedLeaseStore) Close(ctx context.Context, leaseID string, cost float64, outcome string, when time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.LeaseStore.Close(ctx, leaseID, cost, outcome, when)
}

// Both acquisitions observe no open lease before either inserts one. The
// later request wins the launch; the older one finishes acquiring afterwards
// and must release its own lease, not the winner found by GetOpenByRun.
func TestReleaseGrant_ConcurrentAcquisitionsKeepTheWinnersReservation(t *testing.T) {
	for _, winnerTeam := range []string{"team-1", "team-2"} {
		t.Run(winnerTeam, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			pledge := h.donor(t, "alice", Limits{MaxRunsPerDay: 2, MaxUSDPerDay: 5})
			var winner *Grant
			var winnerLease Lease
			h.broker.leases = &interleavedLeaseStore{LeaseStore: h.leases, afterFirstRead: func() {
				h.now = h.now.Add(time.Minute)
				req := h.request("same-run")
				req.TenantID = winnerTeam
				var err error
				winner, err = h.broker.Acquire(ctx, req)
				if err != nil {
					t.Fatalf("winner Acquire: %v", err)
				}
				winnerLease, err = h.leases.GetOpenByRun(ctx, req.RunID)
				if err != nil {
					t.Fatal(err)
				}
			}}
			loser, err := h.broker.Acquire(ctx, h.request("same-run"))
			if err != nil || loser == nil || winner == nil {
				t.Fatalf("premise: both acquisitions must succeed: %v", err)
			}
			open, err := h.leases.ListOpenByRun(ctx, "same-run")
			if err != nil || len(open) != 2 || open[0].ID != winnerLease.ID {
				t.Fatalf("premise: two open leases, newest is winner: %v (%v)", open, err)
			}
			loserID := open[1].ID
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			for range 2 { // Cleanup is cancellation-immune and idempotent.
				h.broker.ReleaseGrant(cancelled, loser)
			}
			open, err = h.leases.ListOpenByRun(ctx, "same-run")
			if err != nil || len(open) != 1 || open[0].ID != winnerLease.ID {
				t.Fatalf("losing acquisition released the winner: %v (%v)", open, err)
			}
			closed, err := h.leases.Get(ctx, loserID)
			if err != nil || !closed.Closed || closed.Outcome != OutcomeNotLaunched {
				t.Fatalf("losing acquisition not released: %+v (%v)", closed, err)
			}
			day, _, err := h.ledger.Usage(ctx, pledge.ID, h.now)
			runs, committed, liveErr := h.leases.LiveCommitment(ctx, pledge.ID, "", h.now)
			if err != nil || liveErr != nil || day.Runs != 1 || runs != 1 || committed != 5 {
				t.Fatalf("winner lost quota: daily=%d live=%d committed=%v (%v, %v)", day.Runs, runs, committed, err, liveErr)
			}
			// A grant's public accounting fields cannot retarget its cleanup.
			loser.PledgeID = "other-pledge"
			loser.DonorID = "other-donor"
			h.broker.ReleaseGrant(ctx, loser)
			h.broker.ReleaseGrant(ctx, nil)
			h.broker.ReleaseGrant(ctx, &Grant{PledgeID: pledge.ID})
			var disabled *Broker
			disabled.ReleaseGrant(ctx, winner)
			if got, err := h.leases.GetOpenByRun(ctx, "same-run"); err != nil || got.ID != winnerLease.ID {
				t.Fatalf("empty or unrelated cleanup affected winner: %v (%v)", got, err)
			}
			// The winning launch's own grant still releases its consumed unit.
			for range 2 {
				h.broker.ReleaseGrant(ctx, winner)
			}
			day, _, err = h.ledger.Usage(ctx, pledge.ID, h.now)
			runs, committed, liveErr = h.leases.LiveCommitment(ctx, pledge.ID, "", h.now)
			if err != nil || liveErr != nil || day.Runs != 0 || runs != 0 || committed != 0 {
				t.Fatalf("winning grant not released exactly once: daily=%d live=%d committed=%v (%v, %v)", day.Runs, runs, committed, err, liveErr)
			}
		})
	}
}
