package credpool

import (
	"context"
	"testing"
)

// A lease stamped by a pre-tenancy build (empty TenantID) must not block
// the stamped owner's retry: it supersedes. Symmetrically, an unstamped
// requester supersedes a stamped lease rather than refusing — the refusal
// is for two TEAMS disagreeing about a run id, not for a missing stamp.
func TestAcquire_AnUnstampedSideOfThePairSupersedesInsteadOfRefusing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 5})

	legacy := h.request("run-1")
	legacy.TenantID = ""
	if _, err := h.broker.Acquire(ctx, legacy); err != nil {
		t.Fatalf("legacy Acquire: %v", err)
	}

	mine := h.request("run-1")
	if _, err := h.broker.Acquire(ctx, mine); err != nil {
		t.Fatalf("stamped owner's retry against a legacy lease = %v, want superseded", err)
	}
	open, err := h.leases.ListOpenByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(open) != 1 || open[0].TenantID != "team-1" {
		t.Fatalf("run-1 open leases = %+v, want team-1's single lease", open)
	}

	legacyRetry := h.request("run-1")
	legacyRetry.TenantID = ""
	if _, err := h.broker.Acquire(ctx, legacyRetry); err != nil {
		t.Fatalf("unstamped requester's retry against a stamped lease = %v, want superseded", err)
	}
}
