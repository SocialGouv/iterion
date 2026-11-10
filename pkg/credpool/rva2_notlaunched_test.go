package credpool

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A resume whose publish REPORTS an error after the message landed (the
// ack timeout the publisher's own rollback comments name): the resume
// releases its grant (not_launched), the flip cannot be reverted (the
// runner claimed it), and the attempt RUNS on the released lease's
// credential. Meanwhile the previous attempt's lease — open when the
// resume took over (a killed pod, or the unwind window) — is superseded.
// The resumed attempt's report then skips its own not_launched lease and
// lands on the PREVIOUS attempt's superseded lease: it consumes that
// lease's one stamp (CAS) with the wrong attempt's spend, and the previous
// attempt's own late report then charges nothing.
func TestPoolReview2_aRanButReleasedAttemptsReportStampsThePreviousAttemptsLease(t *testing.T) {
	for _, fine := range []bool{true, false} {
		t.Run(map[bool]string{true: "fine", false: "coarse"}[fine], func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 100})
			h.donorKey(t, "bob", "anthropic", Limits{MaxUSDPerDay: 100}, func(p *Pledge) { p.Priority = 5 })
			pa := PledgeID("alice", SourceAPIKey, "anthropic")
			pb := PledgeID("bob", SourceAPIKey, "anthropic")
			fpA := secrets.FingerprintSHA256("sk-real-alice")
			fpB := secrets.FingerprintSHA256("sk-real-bob")
			day := h.now

			// A1 on alice; its pod is slow to report (lease open).
			r1 := h.wantKey("run-n", "anthropic")
			setAttemptID(&r1, "A1")
			g1, err := h.broker.Acquire(ctx, r1)
			if err != nil || g1.DonorID != "alice" {
				t.Fatalf("A1: %+v %v", g1, err)
			}
			P1 := store.PublishAt(h.now.Add(50*time.Millisecond), g1.AcquiredAt)

			// The resume: alice now cooling, bob serves; supersedes A1's lease.
			h.now = h.now.Add(2 * time.Minute)
			until := h.now.Add(time.Hour)
			p, _ := h.pledges.Get(ctx, pa)
			p.CooldownUntil = &until
			_ = h.pledges.Upsert(ctx, p)
			r2 := h.wantKey("run-n", "anthropic")
			setAttemptID(&r2, "A2")
			g2, err := h.broker.Acquire(ctx, r2)
			if err != nil || g2.DonorID != "bob" {
				t.Fatalf("A2: %+v %v", g2, err)
			}
			P2 := store.PublishAt(h.now.Add(50*time.Millisecond), g2.AcquiredAt)
			// The publish reported an error: the grant is released; the flip
			// could not be reverted (claimed), so nothing is reopened.
			h.broker.ReleaseGrant(ctx, g2)

			// A2 RAN on bob's key: $4.
			h.now = h.now.Add(time.Minute)
			out2 := Outcome{CostUSD: 4}
			if fine {
				out2.ByFingerprint = []FingerprintSpend{{Fingerprint: fpB, CostUSD: 4}}
			}
			if err := h.broker.ReportAttempt(ctx, "run-n", P2, out2); err != nil {
				t.Fatalf("A2 report: %v", err)
			}
			// A1's late teardown report: $3 on alice's key.
			out1 := Outcome{CostUSD: 3}
			if fine {
				out1.ByFingerprint = []FingerprintSpend{{Fingerprint: fpA, CostUSD: 3}}
			}
			if err := h.broker.ReportAttempt(ctx, "run-n", P1, out1); err != nil {
				t.Fatalf("A1 report: %v", err)
			}
			gotA, gotB := rva2Spent(t, h, pa, day), rva2Spent(t, h, pb, day)
			all, _ := h.leases.ListByRun(ctx, "run-n")
			for _, l := range all {
				t.Logf("lease %s pledge=%s closed=%v outcome=%q cost=%.2f", l.PledgeID[:12], l.PledgeID, l.Closed, l.Outcome, l.CostUSD)
			}
			t.Logf("ledger: alice=$%.2f (A1 really spent $3 on her key)  bob=$%.2f (A2's $4 ran on his)", gotA, gotB)
			if gotA != 3 {
				t.Errorf("alice's ledger = $%.2f, want $3 — A1's own lease was stamped by A2's report", gotA)
			}
		})
	}
}
