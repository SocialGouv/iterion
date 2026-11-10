package credpool

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func rva2AttKeyReq(runID, prov, attID, attemptID string, sideBySide bool) Request {
	r := Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester",
		BotID: "docs-refresh", Wants: []Credential{{Source: SourceAPIKey, Ref: prov}}, SideBySide: sideBySide,
		PoolIDs: []string{"pool-1"}, AttachmentID: attID}
	setAttemptID(&r, attemptID)
	return r
}

// A COARSE report (ByFingerprint nil: a publication without a plan — the
// fail-open walk that brings the gateway want in — or a plan miss) of a
// side-by-side attempt: every sibling books the WHOLE attempt, so the
// borrower's att: row is charged N x the attempt and each donor the full
// attempt. Variant "superseded": the late report after a resume took the
// attempt over (round 1 now charges the superseded sibling too).
func TestPoolReview2_coarseReportOfASideBySideAttemptChargesEverySiblingTheWholeAttempt(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		t.Run(map[bool]string{false: "open", true: "superseded"}[superseded], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 100})
			h.donorKey(t, "bob", "openai", Limits{MaxUSDPerDay: 100})
			_, attID, _ := attachForTest(t, h, Strategy{}, DrawLimits{})
			pa := PledgeID("alice", SourceAPIKey, "anthropic")
			pb := PledgeID("bob", SourceAPIKey, "openai")
			day := h.now

			a1, err := h.broker.Acquire(ctx, rva2AttKeyReq("run-c", "anthropic", attID, "A1", false))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.broker.Acquire(ctx, rva2AttKeyReq("run-c", "openai", attID, "A1", true)); err != nil {
				t.Fatal(err)
			}
			P1 := store.PublishAt(h.now.Add(50*time.Millisecond), a1.AcquiredAt)
			if superseded {
				h.now = h.now.Add(2 * time.Minute)
				if _, err := h.broker.Acquire(ctx, rva2AttKeyReq("run-c", "anthropic", attID, "A2", false)); err != nil {
					t.Fatal(err)
				}
			}
			h.now = h.now.Add(time.Minute)
			// The attempt spent $3 in total; no fine table.
			if err := h.broker.ReportAttempt(ctx, "run-c", P1, Outcome{CostUSD: 3}); err != nil {
				t.Fatalf("report: %v", err)
			}
			att := rva2Spent(t, h, AttachmentLedgerID(attID), day)
			t.Logf("attempt spent $3 (coarse): alice=$%.2f bob=$%.2f att:row=$%.2f", rva2Spent(t, h, pa, day), rva2Spent(t, h, pb, day), att)
			if att > 3.0001 {
				t.Errorf("the borrower's att: row booked $%.2f for a $3 attempt — every sibling booked the whole coarse attempt", att)
			}
		})
	}
}

func rva2Spent(t *testing.T, h *harness, pledgeID string, at time.Time) float64 {
	t.Helper()
	day, _, err := h.ledger.Usage(context.Background(), pledgeID, at)
	if err != nil {
		t.Fatal(err)
	}
	return day.CostUSD
}
