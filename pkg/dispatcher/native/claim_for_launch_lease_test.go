package native

import (
	"testing"
)

// RVA-T9 R2: ClaimForLaunch is a SECOND launch authority that never reads
// the claim family. Under ADR-096 the claim+lease is THE mutual exclusion
// the dispatcher wins with; the dispatcher's move out of StateReady is
// offloaded off the actor (launchDispatchSetup), so a card can be legally
// claimed-and-launching while still sitting in Ready. A studio pipeline
// admission loop in another process then wins ClaimForLaunch and launches
// a second run against the same card.
func TestRVAT9_ClaimForLaunchIgnoresALiveLease(t *testing.T) {
	s := newTestStore(t)
	iss, err := s.Create(Issue{Title: "ready card", State: StateReady, Bot: "feature-dev"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The dispatcher wins the claim with a FRESH lease and starts its run;
	// the in_progress move has not landed yet (it is offloaded).
	tok, err := s.Claim(iss.ID, "dispatcher-host-a")
	if err != nil {
		t.Fatalf("ClaimLease: %v", err)
	}
	cur, _ := s.Get(iss.ID)
	if cur.State != StateReady || cur.ClaimLeaseUntil.IsZero() {
		t.Fatalf("precondition: card must be Ready with a live lease, got state=%q lease=%s", cur.State, cur.ClaimLeaseUntil)
	}
	// The other process's pipeline admission loop.
	_, won, err := s.ClaimForLaunch(iss.ID)
	if err != nil {
		t.Fatalf("ClaimForLaunch: %v", err)
	}
	if won {
		after, _ := s.Get(iss.ID)
		t.Fatalf("R2 REPRODUCED: ClaimForLaunch won a card held under a LIVE lease by %q (epoch %d) — "+
			"two launchers now own the same card (state now %q, claim still %q)",
			tok.Marker, tok.Epoch, after.State, after.Claim)
	}
}
