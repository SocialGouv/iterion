package credpool

import (
	"errors"

	"context"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"testing"
	"time"
)

// rvaResume is the publisher's per-slot sequence for one forfait slot:
// HeldPledge, then Acquire with the preference and the hold dial.
func rvaResume(t *testing.T, h *harness, runID string, mode StrategyMode, margin func(Pledge) (time.Time, bool)) *Grant {
	t.Helper()
	ctx := context.Background()
	held, err := h.broker.HeldPledge(ctx, runID, claudeWant())
	if err != nil {
		t.Fatal(err)
	}
	req := Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: "docs-refresh",
		PoolIDs: []string{"pool-1"}, RankMode: mode, ClosedAtMargin: margin, HoldOnSwitch: true,
		PreferPledgeID: held, Wants: []Credential{claudeWant()}}
	g, err := h.broker.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return g
}

func rvaSetCooldown(t *testing.T, h *harness, user string, until *time.Time) {
	t.Helper()
	mustPledge(t, h, user, func(p *Pledge) { p.CooldownUntil = until })
}

// H-A: the hold resurrects. A (prio 0) / B (prio 5): launch switches to B
// (held); a resume finds B cooling and serves A (not held — the run's LAST
// sealed entry is A); the next resume, both healthy, flips back to B because
// HeldPledge reads the newest HELD lease, skipping the newer unheld one.
func TestResetHoldSwitch_HA_theHoldResurrectsAfterTheRunReturnedToItsFirstChoice(t *testing.T) {
	h := newHarnessWithAttachments(t)
	h.donor(t, "a", Limits{}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
	nearA := func(p Pledge) (time.Time, bool) { return h.now.Add(time.Hour), p.UserID == "a" }

	g1 := rvaResume(t, h, "r", StrategyPriority, nearA)
	t.Logf("launch:   served by %s held=%v", g1.DonorID, g1.Held)
	h.now = h.now.Add(10 * time.Minute)
	until := h.now.Add(time.Hour)
	rvaSetCooldown(t, h, "b", &until)
	g2 := rvaResume(t, h, "r", StrategyPriority, nil)
	t.Logf("resume 1: served by %s held=%v (B cooling)", g2.DonorID, g2.Held)
	h.now = h.now.Add(10 * time.Minute)
	rvaSetCooldown(t, h, "b", nil)
	g3 := rvaResume(t, h, "r", StrategyPriority, nil)
	t.Logf("resume 2: served by %s held=%v (both healthy; the run last sealed A)", g3.DonorID, g3.Held)
	if g3.DonorID != "a" {
		t.Errorf("resume 2 flipped back to %s — the run's last sealed entry was a; the hold resurrected from an older lease", g3.DonorID)
	}
}

// H-B: an entry that was never in play for this bot (bot-filtered) or that
// the donor paused counts as a "switch": the strategy's first choice is held.
func TestResetHoldSwitch_HB_aNeverInPlayEntryMakesTheFirstChoiceHeld(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Pledge)
	}{
		{"bot-filtered", func(p *Pledge) { p.Bots = []string{"some-other-bot"} }},
		{"paused", func(p *Pledge) { p.Enabled = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWithAttachments(t)
			h.donor(t, "x", Limits{}, func(p *Pledge) { p.Priority = 0; tc.mutate(p) })
			h.donor(t, "y", Limits{}, func(p *Pledge) { p.Priority = 1 })
			g := rvaResume(t, h, "r", StrategyPriority, nil)
			t.Logf("served by %s held=%v", g.DonorID, g.Held)
			if g.Held {
				t.Errorf("%s: y is this bot's FIRST choice (x %s) — held=%v", tc.name, tc.name, g.Held)
			}
		})
	}
}

// H-B': least_consumed (all priorities 0): one paused entry anywhere in the
// kind holds EVERY run's first acquisition, and the hold then overrides the
// least-consumed rotation on every resume.
func TestResetHoldSwitch_HBprime_leastConsumedRotationDefeatedByAPausedPeer(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "x", Limits{MaxUSDPerDay: 100})
	h.donor(t, "y", Limits{MaxUSDPerDay: 100})
	h.donor(t, "z", Limits{MaxUSDPerDay: 100}, func(p *Pledge) { p.Enabled = false })
	g1 := rvaResume(t, h, "r", StrategyLeastConsumed, nil)
	t.Logf("launch: served by %s held=%v", g1.DonorID, g1.Held)
	// The run's attempt and other runs consume g1's donor heavily.
	_ = h.ledger.AddSpend(ctx, g1.PledgeID, h.now, 60, 0, 0, 0)
	h.now = h.now.Add(time.Minute)
	g2 := rvaResume(t, h, "r", StrategyLeastConsumed, nil)
	t.Logf("resume: served by %s held=%v (least consumed is the other one)", g2.DonorID, g2.Held)
	if g2.DonorID == g1.DonorID {
		t.Errorf("least_consumed resume pinned to %s by a hold no switch ever made", g2.DonorID)
	}
}

// H-E: a resume refused after its acquisition (rollback: ReleaseGrant +
// ReopenSuperseded) leaves its Held lease behind; the next resume prefers
// an entry the run never ran on.
func TestResetHoldSwitch_HE_aRolledBackResumeLeavesItsHold(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "a", Limits{}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
	h.donor(t, "c", Limits{}, func(p *Pledge) { p.Priority = 9 })
	g1 := rvaResume(t, h, "r", StrategyPriority, nil) // a, first choice, not held
	t.Logf("A1: %s held=%v", g1.DonorID, g1.Held)
	h.now = h.now.Add(time.Minute)
	nearA := func(p Pledge) (time.Time, bool) { return h.now.Add(time.Hour), p.UserID == "a" }
	g2 := rvaResume(t, h, "r", StrategyPriority, nearA) // switches to b, held
	t.Logf("refused resume: %s held=%v", g2.DonorID, g2.Held)
	h.broker.ReleaseGrant(ctx, g2)
	h.broker.ReopenSuperseded(ctx, g2)
	open, _ := h.leases.ListOpenByRun(ctx, "r")
	held, _ := h.broker.HeldPledge(ctx, "r", claudeWant())
	t.Logf("after rollback: open=%d (A1's reopened lease on %s); HeldPledge=%q", len(open), open[0].DonorID, held)
	if held != "" {
		t.Errorf("a resume that never ran left the run holding %q", held)
	}
}

// blipOAuth fails the OAuth read of one donor while armed (a Mongo blip).
type blipOAuth struct {
	secrets.OAuthStore
	user  string
	armed *bool
}

func (b blipOAuth) Get(ctx context.Context, userID string, kind secrets.OAuthKind) (secrets.OAuthRecord, error) {
	if *b.armed && userID == b.user {
		return secrets.OAuthRecord{}, errors.New("mongo blip")
	}
	return b.OAuthStore.Get(ctx, userID, kind)
}

// H-F: a TRANSIENT store error on the first choice (logged, skipped as
// unhealthy, the walk moves on) holds the next entry — the run stays pinned
// to the worse entry after the blip heals, until an operator reset.
func TestResetHoldSwitch_HF_aTransientBlipOnTheFirstChoiceHoldsTheNext(t *testing.T) {
	h := newHarnessWithAttachments(t)
	h.donor(t, "a", Limits{}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
	armed := true
	h.broker.oauth = blipOAuth{OAuthStore: h.oauth, user: "a", armed: &armed}
	g1 := rvaResume(t, h, "r", StrategyPriority, nil)
	t.Logf("launch during a store blip on a: served by %s held=%v", g1.DonorID, g1.Held)
	armed = false
	pa, _ := h.pledges.Get(context.Background(), PledgeID("a", SourceOAuth, "claude_code"))
	t.Logf("a after the blip: health=%q (not parked — a transient error)", pa.Health)
	h.now = h.now.Add(time.Minute)
	g2 := rvaResume(t, h, "r", StrategyPriority, nil)
	t.Logf("resume, blip healed, a healthy: served by %s held=%v", g2.DonorID, g2.Held)
	if g2.DonorID != "a" {
		t.Errorf("a one-off store error pinned the run to %s until a manual reset", g2.DonorID)
	}
}
