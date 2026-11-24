package credpool

import (
	"context"
	"testing"
	"time"
)

// rva2Acquire is the publisher's per-slot sequence for one forfait slot:
// HeldPledge, then Acquire with the preference and the hold dial.
func rva2Acquire(t *testing.T, h *harness, runID string) *Grant {
	t.Helper()
	ctx := context.Background()
	held, err := h.broker.HeldPledge(ctx, runID, claudeWant())
	if err != nil {
		t.Fatal(err)
	}
	req := Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: "docs-refresh",
		PoolIDs: []string{"pool-1"}, RankMode: StrategyPriority, HoldOnSwitch: true,
		PreferPledgeID: held, Wants: []Credential{claudeWant()}}
	g, err := h.broker.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return g
}

// A donor whose DAILY SPEND CEILING is truly spent ($10 of $10) while one
// other run is still in flight on it: the walk's decline reads "serving"
// (skipStatus), and switchStatus does not count "serving" as exhaustion —
// the run that moves to the next entry is NOT held, and the next resume
// after the first entry's day resets flips straight back to it.
func TestPoolReview2_spendCeilingWithRunsInFlightIsAnExhaustionTheHoldMustKeep(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	a := h.donor(t, "a", Limits{MaxUSDPerDay: 10}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })

	// Another run is served by a and is still in flight.
	other, err := h.broker.Acquire(ctx, h.request("other"))
	if err != nil || other.DonorID != "a" {
		t.Fatalf("other run: %+v %v", other, err)
	}
	// a's whole day is spent.
	if err := h.ledger.AddSpend(ctx, a.ID, h.now, 10, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	g1 := rva2Acquire(t, h, "r")
	t.Logf("launch, a's $10/day spent with another run in flight: served by %s held=%v", g1.DonorID, g1.Held)
	if g1.DonorID != "b" {
		t.Fatalf("served by %s, want b", g1.DonorID)
	}
	if !g1.Held {
		t.Errorf("a's daily spend ceiling is exhausted ($10/$10) — the switch to b is NOT held because the decline read 'serving'")
	}

	// The run's attempt ends, the other run ends, and a's day resets.
	_ = h.broker.Report(ctx, "r", Outcome{CostUSD: 0.1})
	_ = h.broker.Report(ctx, "other", Outcome{CostUSD: 0.1})
	h.now = h.now.Add(24 * time.Hour)
	g2 := rva2Acquire(t, h, "r")
	t.Logf("resume after a's day reset: served by %s held=%v", g2.DonorID, g2.Held)
	if g2.DonorID != "b" {
		t.Errorf("resume flipped back to %s — the reset window pulled the run back (the thrash reset_hold exists to stop)", g2.DonorID)
	}
}

// Control: the SAME exhaustion with nothing in flight reads "exhausted" and
// holds — the only difference is another run in flight on the donor.
func TestPoolReview2_control_spendCeilingWithNothingInFlightHolds(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	a := h.donor(t, "a", Limits{MaxUSDPerDay: 10}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
	if err := h.ledger.AddSpend(ctx, a.ID, h.now, 10, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	g1 := rva2Acquire(t, h, "r")
	t.Logf("launch, a's $10/day spent, nothing in flight: served by %s held=%v", g1.DonorID, g1.Held)
	if g1.DonorID != "b" || !g1.Held {
		t.Errorf("served by %s held=%v, want b HELD", g1.DonorID, g1.Held)
	}
}
