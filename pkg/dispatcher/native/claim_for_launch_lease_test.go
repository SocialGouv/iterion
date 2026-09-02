package native

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
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

// The EXPORTED promote helper (the board-deps surface's path) must stamp
// the same descriptive reason the store's internal locked promote does —
// the type-assert fallback to bare SetState was a silent twin divergence
// inside one store.
func TestPromoteHelper_StampsUnblockedOnTheFSTwin(t *testing.T) {
	s := newTestStore(t)
	// The blocker completes BEFORE the dependent exists, so the store's
	// internal locked cascade never fires — the exported helper is the
	// ONLY promoter this test exercises (with the dependent pre-existing,
	// the internal path promotes first and the assertion reads ITS event:
	// vacuous for the helper).
	blocker, err := s.Create(Issue{Title: "blocker", State: StateInProgress})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStateOwned(blocker.ID, StateDone, mustClaim(t, s, blocker.ID)); err != nil {
		t.Fatal(err)
	}
	dep, err := s.Create(Issue{Title: "dep", State: StateWaitingDeps, Blockers: []string{blocker.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := PromoteUnblockedDependents(s, blocker.ID); err != nil {
		t.Fatal(err)
	}
	var last map[string]any
	if err := s.ScanEvents(func(e *Event) bool {
		if e.Type == EvtIssueState && e.IssueID == dep.ID {
			last = e.Payload
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if last == nil {
		t.Fatal("dependent never promoted")
	}
	if got, _ := last["reason"].(string); got != tracker.ReasonUnblocked {
		t.Fatalf("exported promote stamped reason %q, want %q — the helper silently degraded to the bare SetState", got, tracker.ReasonUnblocked)
	}
}

func mustClaim(t *testing.T, s *Store, id string) tracker.ClaimToken {
	t.Helper()
	tok, err := s.Claim(id, "host-t11")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
