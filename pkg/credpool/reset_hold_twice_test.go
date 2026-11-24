package credpool

// Adversarial proof (rva round 1, surface reviewer) — isolated copy only.
// Promise under test: the entry reset "lifts every reset_hold naming the
// entry (the runs holding it re-rank at their next resume)"
// (pkg/server/pool_routes.go handleResetPoolEntry godoc).

import (
	"context"
	"testing"
	"time"
)

func TestResetHoldTwice_ResettingTheHeldEntryReRanksTheRun(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "first", Limits{}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "second", Limits{}, func(p *Pledge) { p.Priority = 5 })
	h.donor(t, "third", Limits{}, func(p *Pledge) { p.Priority = 10 })
	poolIDs, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority, ResetHold: true}, DrawLimits{})
	nearFirst := func(p Pledge) (time.Time, bool) {
		if p.UserID == "first" {
			return h.now.Add(2 * time.Hour), true
		}
		return time.Time{}, false
	}

	// Attempt 1: "first" past its margin -> the strategy switches to
	// "second", held.
	g1, err := h.broker.Acquire(ctx, holdRequest(h, poolIDs, attID, "r1", nearFirst))
	if err != nil || g1.DonorID != "second" || !g1.Held {
		t.Fatalf("attempt 1 = %+v, %v; want second, held", g1, err)
	}

	// Attempt 2 (a resume, later): the held "second" cools down, "first"
	// still past its margin -> "third" serves, held (the run's newest hold).
	h.now = h.now.Add(2 * time.Millisecond)
	h.pledges.Upsert(ctx, mustPledge(t, h, "second", func(p *Pledge) {
		until := h.now.Add(time.Hour)
		p.CooldownUntil = &until
	}))
	req := holdRequest(h, poolIDs, attID, "r1", nearFirst)
	req.PreferPledgeID, _ = h.broker.HeldPledge(ctx, "r1", claudeWant())
	g2, err := h.broker.Acquire(ctx, req)
	if err != nil || g2.DonorID != "third" || !g2.Held {
		t.Fatalf("attempt 2 = %+v, %v; want third, held", g2, err)
	}
	if held, _ := h.broker.HeldPledge(ctx, "r1", claudeWant()); held != g2.PledgeID {
		t.Fatalf("before the reset the run holds %q, want third %q", held, g2.PledgeID)
	}

	// The operator resets the entry the run holds ("third").
	if _, err := h.broker.ClearHolds(ctx, g2.PledgeID); err != nil {
		t.Fatal(err)
	}
	held, _ := h.broker.HeldPledge(ctx, "r1", claudeWant())
	t.Logf("after `pools reset` on the held entry %s (third), HeldPledge(r1) = %q (second = %s)", g2.PledgeID, held, g1.PledgeID)
	if held != "" {
		t.Fatalf("the reset of the held entry did not re-rank the run: its next resume still prefers %q (an OLDER hold, %s), not the strategy's order", held, map[bool]string{true: "second", false: "?"}[held == g1.PledgeID])
	}
}

// Promise: "The strategy's first choice is never held" (PR body, commit
// 3d0878e19, ADR-164 amendment). The run's held entry is moved to the
// front; when it then fails INSIDE tryPledge (its credential is gone), the
// strategy's own first choice serves at index 1 and `i > 0` marks it held.
func TestResetHoldTwice_TheFirstChoiceIsNeverHeld(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "first", Limits{}, func(p *Pledge) { p.Priority = 0 })
	second := h.donor(t, "second", Limits{}, func(p *Pledge) { p.Priority = 5 })
	poolIDs, attID, _ := attachForTest(t, h, Strategy{Mode: StrategyPriority, ResetHold: true}, DrawLimits{})

	// The run holds "second"; its OAuth record is then deleted (credential gone).
	rec, err := h.oauth.Get(ctx, "second", "claude_code")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.oauth.Delete(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	req := holdRequest(h, poolIDs, attID, "r1", nil)
	req.PreferPledgeID = second.ID
	g, err := h.broker.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	t.Logf("served by %q held=%v", g.DonorID, g.Held)
	if g.DonorID == "first" && g.Held {
		t.Fatalf("the strategy's first choice %q was HELD — the preferred entry ahead of it failed, so i>0 read as a switch", g.DonorID)
	}
}
