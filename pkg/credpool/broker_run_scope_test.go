package credpool

import (
	"context"
	"errors"
	"testing"
)

// The leases say which team's work a donation served. A launch naming a
// run id another team already holds used to close that run's leases before
// its own save failed on the duplicate id: the donor's slot was freed and
// the report had no lease to charge. Acquiring must refuse an id another
// team holds, and keep superseding only the requesting team's own leases.
func TestAcquire_AnotherTeamsRunIDIsRefusedWithoutTouchingTheRunsLeases(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})

	mine := h.request("run-1")
	if _, err := h.broker.Acquire(ctx, mine); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	foreign := h.request("run-1")
	foreign.TenantID = "team-2"
	foreign.UserID = "intruder"
	if _, err := h.broker.Acquire(ctx, foreign); !errors.Is(err, ErrRunHeldElsewhere) {
		t.Fatalf("another team's acquire on run-1 = %v, want ErrRunHeldElsewhere", err)
	}

	open, err := h.leases.ListOpenByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 || open[0].TenantID != "team-1" {
		t.Fatalf("run-1 open leases = %+v, want team-1's single untouched lease", open)
	}

	// The owning team's own retry still supersedes, as before.
	if _, err := h.broker.Acquire(ctx, mine); err != nil {
		t.Fatalf("owning team's retry: %v", err)
	}
	open, err = h.leases.ListOpenByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("list after the retry: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("run-1 holds %d open leases after the retry, want 1", len(open))
	}
}
