package credpool

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// reset_hold's runtime half (ADR-164, #3122): a seal-time switch marks the
// lease it opens Held; a later acquisition of the same run prefers that
// entry; the entry reset lifts the hold.

func holdRequest(h *harness, poolIDs []string, attID, runID string, margin func(Pledge) (time.Time, bool)) Request {
	return Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "alice",
		PoolIDs: poolIDs, AttachmentID: attID, RankMode: StrategyPriority,
		ClosedAtMargin: margin, HoldOnSwitch: true,
		Wants: []Credential{{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}}}
}

func claudeWant() Credential {
	return Credential{Source: SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)}
}

// TestResetHold_aSwitchHoldsAndTheFirstChoiceDoesNot: the entry served
// AFTER a better-or-equal one declined is held; the strategy's own first
// choice is not.
func TestResetHold_aSwitchHoldsAndTheFirstChoiceDoesNot(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "first", Limits{}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "second", Limits{}, func(p *Pledge) { p.Priority = 5 })
	poolIDs, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority, ResetHold: true}, DrawLimits{})

	// No skip: the first choice serves, no hold.
	g, err := h.broker.Acquire(ctx, holdRequest(h, poolIDs, attID, "r-first", nil))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if g.DonorID != "first" || g.Held {
		t.Fatalf("first choice: served by %q held=%v, want first and NOT held", g.DonorID, g.Held)
	}

	// "first" sits past its margin: the strategy switches to "second",
	// and the lease it opens is held.
	near := func(p Pledge) (time.Time, bool) {
		if p.UserID == "first" {
			return h.now.Add(2 * time.Hour), true
		}
		return time.Time{}, false
	}
	g, err = h.broker.Acquire(ctx, holdRequest(h, poolIDs, attID, "r-switch", near))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if g.DonorID != "second" || !g.Held {
		t.Fatalf("switch: served by %q held=%v, want second and HELD", g.DonorID, g.Held)
	}
	held, err := h.broker.HeldPledge(ctx, "r-switch", claudeWant())
	if err != nil || held != g.PledgeID {
		t.Fatalf("HeldPledge = %q (%v), want the switched-to entry %q", held, err, g.PledgeID)
	}
	if held, _ := h.broker.HeldPledge(ctx, "r-first", claudeWant()); held != "" {
		t.Fatalf("a run served by its first choice holds %q — nothing switched", held)
	}
}

// TestResetHold_thePreferredEntryLeadsUntilItCannotServe: a resume naming
// the held entry ranks it first even when the better entry recovered; an
// ineligible held entry yields to the ordinary ranking.
func TestResetHold_thePreferredEntryLeadsUntilItCannotServe(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "first", Limits{}, func(p *Pledge) { p.Priority = 0 })
	second := h.donor(t, "second", Limits{}, func(p *Pledge) { p.Priority = 5 })
	poolIDs, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority, ResetHold: true}, DrawLimits{})

	// "first" is warm again (no margin predicate), the run holds "second".
	req := holdRequest(h, poolIDs, attID, "r1", nil)
	req.PreferPledgeID = second.ID
	g, err := h.broker.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if g.PledgeID != second.ID || !g.Held {
		t.Fatalf("held resume: served by %q held=%v, want the held entry %q, still held", g.PledgeID, g.Held, second.ID)
	}

	// The held entry cools down: the ordinary ranking serves "first", and
	// the strategy's own first choice is not held.
	h.pledges.Upsert(ctx, mustPledge(t, h, "second", func(p *Pledge) {
		until := h.now.Add(time.Hour)
		p.CooldownUntil = &until
	}))
	req.RunID = "r2"
	g, err = h.broker.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if g.DonorID != "first" || g.Held {
		t.Fatalf("ineligible held entry: served by %q held=%v, want first, NOT held", g.DonorID, g.Held)
	}
}

// TestResetHold_withoutTheDialNothingHolds: HoldOnSwitch off = the leases
// never carry the mark, switch or not.
func TestResetHold_withoutTheDialNothingHolds(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "first", Limits{}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "second", Limits{}, func(p *Pledge) { p.Priority = 5 })
	poolIDs, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority}, DrawLimits{})
	near := func(p Pledge) (time.Time, bool) { return h.now.Add(time.Hour), p.UserID == "first" }
	req := holdRequest(h, poolIDs, attID, "r1", near)
	req.HoldOnSwitch = false
	g, err := h.broker.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if g.DonorID != "second" || g.Held {
		t.Fatalf("served by %q held=%v, want second and NOT held — the dial is off", g.DonorID, g.Held)
	}
}

// testLeaseClearHeld is the LeaseStore contract of ClearHeld: only the
// named entry's held leases lose the mark, any state.
func testLeaseClearHeld(t *testing.T, ctx context.Context, s LeaseStore) {
	t.Helper()
	at := time.Unix(1700000000, 0).UTC()
	for _, l := range []Lease{
		{ID: "l-open", RunID: "r1", PledgeID: "p-a", Held: true, AcquiredAt: at, ExpiresAt: at.Add(time.Hour)},
		{ID: "l-closed", RunID: "r2", PledgeID: "p-a", Held: true, Closed: true, AcquiredAt: at.Add(time.Second), ExpiresAt: at.Add(time.Hour)},
		{ID: "l-other", RunID: "r3", PledgeID: "p-b", Held: true, AcquiredAt: at, ExpiresAt: at.Add(time.Hour)},
	} {
		if err := s.Put(ctx, l); err != nil {
			t.Fatalf("put %s: %v", l.ID, err)
		}
	}
	n, err := s.ClearHeld(ctx, "p-a")
	if err != nil || n != 2 {
		t.Fatalf("ClearHeld(p-a) = %d, %v, want 2 (the open and the closed lease)", n, err)
	}
	for id, want := range map[string]bool{"l-open": false, "l-closed": false, "l-other": true} {
		l, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if l.Held != want {
			t.Errorf("%s held = %v, want %v", id, l.Held, want)
		}
	}
	if n, err := s.ClearHeld(ctx, "p-a"); err != nil || n != 0 {
		t.Fatalf("a second ClearHeld = %d, %v, want 0 — nothing left to lift", n, err)
	}
}

func TestLeaseClearHeld_Memory(t *testing.T) {
	testLeaseClearHeld(t, context.Background(), NewMemoryLeaseStore())
}

// TestResetHold_theResetLiftsTheHold: ClearHolds on the entry empties the
// run's held answer.
func TestResetHold_theResetLiftsTheHold(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "first", Limits{}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "second", Limits{}, func(p *Pledge) { p.Priority = 5 })
	poolIDs, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority, ResetHold: true}, DrawLimits{})
	near := func(p Pledge) (time.Time, bool) { return h.now.Add(time.Hour), p.UserID == "first" }
	g, err := h.broker.Acquire(ctx, holdRequest(h, poolIDs, attID, "r1", near))
	if err != nil || !g.Held {
		t.Fatalf("acquire = %+v, %v, want a held switch", g, err)
	}
	if n, err := h.broker.ClearHolds(ctx, g.PledgeID); err != nil || n != 1 {
		t.Fatalf("ClearHolds = %d, %v, want 1", n, err)
	}
	if held, _ := h.broker.HeldPledge(ctx, "r1", claudeWant()); held != "" {
		t.Fatalf("the run still holds %q after the reset", held)
	}
}

// TestResetHold_anEqualPriorityPeerNearItsMarginStillHolds: two entries of
// the SAME priority — the one serving after its peer was skipped at the
// margin is a switch, held: returning to the peer at its reset is the
// thrash the dial stops.
func TestResetHold_anEqualPriorityPeerNearItsMarginStillHolds(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "near", Limits{}, func(p *Pledge) { p.Priority = 2 })
	h.donor(t, "ok", Limits{}, func(p *Pledge) { p.Priority = 2 })
	poolIDs, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority, ResetHold: true}, DrawLimits{})
	near := func(p Pledge) (time.Time, bool) { return h.now.Add(time.Hour), p.UserID == "near" }
	g, err := h.broker.Acquire(ctx, holdRequest(h, poolIDs, attID, "r1", near))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if g.DonorID != "ok" || !g.Held {
		t.Fatalf("served by %q held=%v, want ok and HELD — its equal-priority peer was skipped", g.DonorID, g.Held)
	}
}
