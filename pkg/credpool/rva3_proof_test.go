package credpool

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// F1 — `capped` arms reset_hold on an entry that is merely BUSY: a $/day
// ceiling met by allowance PROMISED to an in-flight run (no concurrency
// dial = the first run is handed the whole remaining allowance), nothing
// spent. The entry frees as soon as that run ends.
func TestPoolReview3_F1_committedOnlyCostDenyArmsAHold(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	a := h.donor(t, "a", Limits{MaxUSDPerDay: 10}, func(p *Pledge) { p.Priority = 0 }) // no MaxConcurrentRuns: 0 = uncapped
	h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })

	other, err := h.broker.Acquire(ctx, h.request("other"))
	if err != nil || other.DonorID != "a" {
		t.Fatalf("other run: %+v %v", other, err)
	}
	t.Logf("other run admitted on a: granted allowance $%.2f; a's ledger spent $%.2f", other.RemainingUSD, rva2Spent(t, h, a.ID, h.now))

	g1 := rva2Acquire(t, h, "r")
	t.Logf("r launch while other is in flight (a spent $0, its $10 only promised): served by %s held=%v", g1.DonorID, g1.Held)

	// other ends having spent $0.10; r's attempt ends too.
	h.now = h.now.Add(time.Minute)
	_ = h.broker.Report(ctx, "other", Outcome{CostUSD: 0.1})
	_ = h.broker.Report(ctx, "r", Outcome{CostUSD: 0.1})
	h.now = h.now.Add(time.Minute)
	g2 := rva2Acquire(t, h, "r")
	t.Logf("r resume, a idle with $%.2f of $10 left: served by %s held=%v", 10-rva2Spent(t, h, a.ID, h.now), g2.DonorID, g2.Held)
	if g1.Held {
		t.Errorf("r was HELD off a whose only 'exhaustion' was allowance promised to an in-flight run (busy, not exhausted)")
	}
	if g2.DonorID != "a" {
		t.Errorf("r's resume stays on %s although a is idle with $9.80 left — the false hold pins the run until a manual reset", g2.DonorID)
	}
}

// F2 — a COARSE report of a side-by-side attempt cannot tell which slot's
// credential failed: conservative-on-ignore, the condition reaches EVERY
// sibling — the forfait that actually failed is marked (and, with its
// earlier failure, parked) instead of being cleared as ConditionOK while
// only the newest (metered/gateway) lease took the blame.
func TestPoolReview3_F2_coarseSideBySideConditionMisattributed(t *testing.T) {
	for _, cond := range []Condition{ConditionAuthFailed, ConditionUsageWindow} {
		t.Run(string(cond), func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			alice := h.donor(t, "alice", Limits{}) // the forfait (claude_code subscription), first slot
			bobP, _ := h.donorKey(t, "bob", "openai", Limits{})
			_, attID, _ := attachForTest(t, h, Strategy{}, DrawLimits{})
			// alice's subscription was already rejected once (a previous run).
			mustPledge(t, h, "alice", func(p *Pledge) { p.ConsecutiveAuthFailures = 1 })

			for i, attempt := range []string{"A1", "A2"} {
				fr := Request{RunID: "run-x", OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: "docs-refresh",
					Wants: []Credential{claudeWant()}, PoolIDs: []string{"pool-1"}, AttachmentID: attID}
				setAttemptID(&fr, attempt)
				g1, err := h.broker.Acquire(ctx, fr)
				if err != nil {
					// The forfait was parked by the previous attempt's
					// auth_failed (it already carried one failure): the
					// condition reached it — the property this test pins.
					if cond == ConditionAuthFailed && i > 0 {
						if pa, _ := h.pledges.Get(ctx, alice.ID); pa.Health != HealthOK {
							return
						}
					}
					t.Fatalf("%s forfait: %v", attempt, err)
				}
				g2, err := h.broker.Acquire(ctx, rva2AttKeyReq("run-x", "openai", attID, attempt, true))
				if err != nil {
					t.Fatalf("%s key: %v", attempt, err)
				}
				P := store.PublishAt(h.now.Add(50*time.Millisecond), g2.AcquiredAt)
				h.now = h.now.Add(time.Minute)
				// The provider rejected / closed alice's SUBSCRIPTION; no plan → coarse.
				if err := h.broker.ReportAttempt(ctx, "run-x", P, Outcome{CostUSD: 0.5, Condition: cond}); err != nil {
					t.Fatalf("report: %v", err)
				}
				pa, _ := h.pledges.Get(ctx, alice.ID)
				pb, _ := h.pledges.Get(ctx, bobP.ID)
				cdA, cdB := "none", "none"
				if pa.CooldownUntil != nil {
					cdA = pa.CooldownUntil.Format(time.Kitchen)
				}
				if pb.CooldownUntil != nil {
					cdB = pb.CooldownUntil.Format(time.Kitchen)
				}
				t.Logf("after %s (attempt %d, %s): alice(forfait) failures=%d health=%s cooldown=%s | bob(key, newest lease) failures=%d health=%s cooldown=%s | g1=%s g2=%s",
					attempt, i+1, cond, pa.ConsecutiveAuthFailures, pa.Health, cdA, pb.ConsecutiveAuthFailures, pb.Health, cdB, g1.DonorID, g2.DonorID)
				all, _ := h.leases.ListByRun(ctx, "run-x")
				for _, l := range all {
					if l.AttemptID == attempt {
						t.Logf("   lease %s donor=%s closed=%v outcome=%q cost=%.2f", l.AttemptID, l.DonorID, l.Closed, l.Outcome, l.CostUSD)
					}
				}
				h.now = h.now.Add(2 * time.Hour) // past any blind cooldown
			}
			pa, _ := h.pledges.Get(ctx, alice.ID)
			if cond == ConditionAuthFailed && pa.Health == HealthOK {
				t.Errorf("the coarse auth_failed reports never parked the forfait (failures=%d): the condition never reached it", pa.ConsecutiveAuthFailures)
			}
			if cond == ConditionUsageWindow && pa.CooldownUntil == nil {
				t.Errorf("the coarse usage_window reports never cooled the forfait: the condition never reached it")
			}
		})
	}
}

// F3 — a FINE report (table present) of a side-by-side attempt whose
// non-booker sibling is a key stored before fingerprint stamping (lease
// Fingerprint ""): withoutSpend zeroes it, contradicting effectiveCharge's
// "never zero a donor's charge on a missing stamp".
func TestPoolReview3_F3_fineReportZeroesAnUnstampedSibling(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	// alice: an anthropic key stored BEFORE fingerprint stamping.
	sealed, err := secrets.SealAPIKey(h.sealer, "key-alice-legacy", []byte("sk-real-alice"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.apiKeys.Create(ctx, secrets.ApiKey{ID: "key-alice-legacy", TenantID: "team-1", ScopeTeamID: "team-1", ScopeUserID: "alice",
		Provider: "anthropic", Name: "lent", SealedSecret: sealed}); err != nil {
		t.Fatal(err)
	}
	pa := Pledge{ID: PledgeID("alice", SourceAPIKey, "anthropic"), PoolID: "pool-1", UserID: "alice",
		Credential: Credential{Source: SourceAPIKey, Ref: "anthropic", KeyID: "key-alice-legacy"}, Enabled: true, Health: HealthOK, Limits: Limits{MaxUSDPerDay: 5}}
	if err := h.pledges.Upsert(ctx, pa); err != nil {
		t.Fatal(err)
	}
	h.donorKey(t, "bob", "openai", Limits{})
	pb := PledgeID("bob", SourceAPIKey, "openai")
	_, attID, _ := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now

	g1, err := h.broker.Acquire(ctx, rva2AttKeyReq("run-f", "anthropic", attID, "A1", false))
	if err != nil {
		t.Fatal(err)
	}
	g2, err := h.broker.Acquire(ctx, rva2AttKeyReq("run-f", "openai", attID, "A1", true))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("grant fingerprints: alice=%q bob=%q", g1.Fingerprint, g2.Fingerprint)
	P := store.PublishAt(h.now.Add(50*time.Millisecond), g2.AcquiredAt)
	h.now = h.now.Add(time.Minute)
	// The publisher sealed alice's key under the fingerprint it derived from
	// the plaintext (grant.Fingerprint was empty), so the runner's table
	// names it: alice's key spent $6 (past her $5/day), bob's $1.
	fpA := secrets.FingerprintSHA256("sk-real-alice")
	fpB := secrets.FingerprintSHA256("sk-real-bob")
	out := Outcome{CostUSD: 7, ByFingerprint: []FingerprintSpend{{Fingerprint: fpA, CostUSD: 6}, {Fingerprint: fpB, CostUSD: 1}}}
	if err := h.broker.ReportAttempt(ctx, "run-f", P, out); err != nil {
		t.Fatal(err)
	}
	gotA, gotB, att := rva2Spent(t, h, pa.ID, day), rva2Spent(t, h, pb, day), rva2Spent(t, h, AttachmentLedgerID(attID), day)
	t.Logf("fine report $7 (alice $6, bob $1): ledger alice=$%.2f bob=$%.2f att:row=$%.2f", gotA, gotB, att)
	if gotA == 0 {
		t.Errorf("alice's key spent $6 (her cap is $5/day) and her ledger books $0 — the unstamped sibling was zeroed")
	}
	// Her ceiling never sees it: the next run is admitted on her key.
	if g, err := h.broker.Acquire(ctx, rva2AttKeyReq("run-g", "anthropic", attID, "B1", false)); err == nil {
		t.Logf("next run admitted on %s with allowance $%.2f (alice's real spend today: $6 of $5)", g.DonorID, g.RemainingUSD)
	} else {
		t.Logf("next run refused: %v", err)
	}
}

// F2b — the cascade: a fail-open side-by-side walk (forfait + metered key,
// coarse report) whose FORFAIT is rejected. Every metered donor of the slot
// is parked in turn (2 attempts each) before the forfait takes a single
// failure — and the forfait's counter is reset on every one of those attempts.
func TestPoolReview3_F2b_coarseAuthFailedParksEveryInnocentKeyDonorFirst(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	alice := h.donor(t, "alice", Limits{})
	h.donorKey(t, "bob", "openai", Limits{})
	h.donorKey(t, "carol", "openai", Limits{})
	_, attID, _ := attachForTest(t, h, Strategy{}, DrawLimits{})
	for i := 1; i <= 8; i++ {
		attempt := "A" + string(rune('0'+i))
		fr := Request{RunID: "run-y", OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: "docs-refresh",
			Wants: []Credential{claudeWant()}, PoolIDs: []string{"pool-1"}, AttachmentID: attID}
		setAttemptID(&fr, attempt)
		g1, err := h.broker.Acquire(ctx, fr)
		if err != nil {
			t.Logf("attempt %d: the forfait is finally out of rotation: %v", i, err)
			break
		}
		last := g1
		key := "-"
		if g2, err := h.broker.Acquire(ctx, rva2AttKeyReq("run-y", "openai", attID, attempt, true)); err == nil {
			last, key = g2, g2.DonorID
		}
		P := store.PublishAt(h.now.Add(50*time.Millisecond), last.AcquiredAt)
		h.now = h.now.Add(time.Minute)
		_ = h.broker.ReportAttempt(ctx, "run-y", P, Outcome{CostUSD: 0.2, Condition: ConditionAuthFailed})
		pa, _ := h.pledges.Get(ctx, alice.ID)
		pb, _ := h.pledges.Get(ctx, PledgeID("bob", SourceAPIKey, "openai"))
		pc, _ := h.pledges.Get(ctx, PledgeID("carol", SourceAPIKey, "openai"))
		t.Logf("attempt %d (forfait=alice, key=%s) rejected: alice %d/%s  bob %d/%s  carol %d/%s",
			i, key, pa.ConsecutiveAuthFailures, pa.Health, pb.ConsecutiveAuthFailures, pb.Health, pc.ConsecutiveAuthFailures, pc.Health)
		h.now = h.now.Add(time.Minute)
	}
}

// F4 — the spend face of the same change: in a coarse side-by-side attempt
// the booker is the NEWEST lease (the last slot: a metered key / the
// gateway), so the forfait donor of the first slot books $0 on every run —
// their MaxUSDPerDay never binds, however much of the attempt ran on them.
func TestPoolReview3_F4_coarseSideBySideNeverChargesTheForfaitCeiling(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	alice := h.donor(t, "alice", Limits{MaxUSDPerDay: 2}) // forfait, $2/day estimated
	h.donorKey(t, "bob", "openai", Limits{})
	_, attID, _ := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now
	for i := 1; i <= 4; i++ {
		run := "run-" + string(rune('0'+i))
		att := "A" + string(rune('0'+i))
		fr := Request{RunID: run, OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: "docs-refresh",
			Wants: []Credential{claudeWant()}, PoolIDs: []string{"pool-1"}, AttachmentID: attID}
		setAttemptID(&fr, att)
		g1, err := h.broker.Acquire(ctx, fr)
		if err != nil {
			t.Logf("run %d: forfait refused (ceiling bound): %v", i, err)
			continue
		}
		g2, err := h.broker.Acquire(ctx, rva2AttKeyReq(run, "openai", attID, att, true))
		if err != nil {
			t.Fatal(err)
		}
		P := store.PublishAt(h.now.Add(50*time.Millisecond), g2.AcquiredAt)
		h.now = h.now.Add(time.Minute)
		// $3 spent, a fail-open walk: no plan, coarse.
		if err := h.broker.ReportAttempt(ctx, run, P, Outcome{CostUSD: 3}); err != nil {
			t.Fatal(err)
		}
		t.Logf("run %d served by forfait %s (allowance $%.2f) + key %s; ledger after: alice=$%.2f bob=$%.2f",
			i, g1.DonorID, g1.RemainingUSD, g2.DonorID, rva2Spent(t, h, alice.ID, day), rva2Spent(t, h, PledgeID("bob", SourceAPIKey, "openai"), day))
	}
}
