package credpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r4GatewayDonor pledges a gateway (openai_compatible) key sealed WITH its
// base URL, as the create endpoint seals it.
func (h *harness) r4GatewayDonor(t *testing.T, userID string, lim Limits, mutate ...func(*Pledge)) Pledge {
	t.Helper()
	ctx := context.Background()
	keyID := "key-" + userID + "-gw"
	k := secrets.ApiKey{ID: keyID, TenantID: "team-1", ScopeTeamID: "team-1", ScopeUserID: userID,
		Provider: secrets.ProviderOpenAICompatible, Name: "lent-gw", BaseURL: "https://gw.example.test/" + userID,
		Fingerprint: secrets.FingerprintSHA256("sk-real-" + userID)}
	sealed, err := secrets.SealAPIKeyFor(h.sealer, k, []byte("sk-real-"+userID))
	if err != nil {
		t.Fatalf("seal gateway key: %v", err)
	}
	k.SealedSecret = sealed
	if err := h.apiKeys.Create(ctx, k); err != nil {
		t.Fatalf("seed gateway key: %v", err)
	}
	p := Pledge{ID: PledgeID(userID, SourceAPIKey, string(secrets.ProviderOpenAICompatible)), PoolID: "pool-1", UserID: userID,
		Credential: Credential{Source: SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible), KeyID: keyID},
		Enabled:    true, Health: HealthOK, Limits: lim}
	for _, m := range mutate {
		m(&p)
	}
	if err := h.pledges.Upsert(ctx, p); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	return p
}

func r4AttReq(runID string, want Credential, attID string, lims DrawLimits, attemptID string, side bool) Request {
	r := Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: "docs-refresh",
		Wants: []Credential{want}, PoolIDs: []string{"pool-1"}, AttachmentID: attID, DrawLimits: lims,
		SideBySide: side}
	setAttemptID(&r, attemptID)
	return r
}

// M1 — I6 (att: draw cap): a run whose ONLY att: unit was refunded (its
// first pool attempt released: a resume refused before publication, or the
// walk's unsealed gateway grant) is later admitted on a RENEW of the att:
// row — attReadmit counts the released lease as an admission — so it draws
// under the attachment holding no draw unit, and the scope's MaxRunsPerDay
// admits one run too many.
func TestPoolGuarantee_M1_aRefundedDrawUnitLetsTheRunDrawForFree(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "alice", Limits{})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{MaxRunsPerDay: 1})

	g1, err := h.broker.Acquire(ctx, r4AttReq("r1", claudeWant(), attID, lims, "A1", false))
	if err != nil {
		t.Fatal(err)
	}
	h.broker.ReleaseGrant(ctx, g1) // refused before its publication: the draw unit goes back
	g2, err := h.broker.Acquire(ctx, r4AttReq("r1", claudeWant(), attID, lims, "A2", false))
	if err != nil {
		t.Fatalf("r1's retry: %v", err)
	}
	g3, err3 := h.broker.Acquire(ctx, r4AttReq("r2", claudeWant(), attID, lims, "B1", false))
	day, _, _ := h.ledger.Usage(ctx, AttachmentLedgerID(attID), h.now)
	t.Logf("r1 retry admitted=%v; r2 admitted=%v (err=%v); att: row runs today=%d (draw cap 1)", g2 != nil, g3 != nil, err3, day.Runs)
	if g2 != nil && g3 != nil {
		t.Errorf("two runs draw today under a 1-run/day draw cap: r1's retry renewed the att: row on its RELEASED lease and consumed no unit")
	}
}

// M2 — I2 (att: row once): a COARSE report whose booker (the attempt's own
// lease) was closed by the sweeper while its sibling — acquired a moment
// later, so expiring a moment later — was still open: the sibling's donor
// books the whole attempt, the borrower's att: row books nothing.
func TestPoolGuarantee_M2_coarseReportWithAnAbandonedBookerNeverBooksTheAttRow(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "alice", Limits{})
	bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now

	g1, err := h.broker.Acquire(ctx, r4AttReq("run-l", claudeWant(), attID, lims, "A1", false))
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(40 * time.Millisecond) // the next slot's acquisition, a few round trips later
	if _, err := h.broker.Acquire(ctx, r4AttReq("run-l", Credential{Source: SourceAPIKey, Ref: "anthropic"}, attID, lims, "A1", true)); err != nil {
		t.Fatal(err)
	}
	pub := store.PublishAt(h.now.Add(20*time.Millisecond), g1.AcquiredAt)

	// A long run: a sweeper tick lands between the two leases' expiries.
	h.now = g1.AcquiredAt.Add(DefaultLeaseTTL).Add(10 * time.Millisecond)
	if n, _ := h.broker.ReleaseExpired(ctx, credPoolSweepBatchForTest); n != 1 {
		t.Fatalf("sweeper freed %d, want the first lease only", n)
	}
	h.now = h.now.Add(time.Hour)
	if err := h.broker.ReportAttempt(ctx, "run-l", pub, Outcome{CostUSD: 3}); err != nil {
		t.Fatal(err)
	}
	att := rva2Spent(t, h, AttachmentLedgerID(attID), day)
	t.Logf("coarse $3 attempt: bob=$%.2f (whole attempt, his lease served) att:row=$%.2f", rva2Spent(t, h, bob.ID, day), att)
	if att != 3 {
		t.Errorf("the borrower's att: row booked $%.2f of a $3 attempt whose sibling was charged it whole — the booker was abandoned, nobody else booked", att)
	}
}

const credPoolSweepBatchForTest = 200

// M3 — reset_hold misses a switch: an entry whose runs/day ceiling is spent
// AND whose concurrency slot is full declines as DenyConcurrency (both
// ledgers check concurrency first), so `capped` is false and the run that
// moves to the next entry is NOT held — round 3's own rule "a runs/day
// ceiling always counts" does not hold when the entry is also busy. After
// the entry's day resets the run flips back: the thrash reset_hold stops.
func TestPoolGuarantee_M3_aSpentButBusyEntryArmsNoHold(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "control-not-busy", true: "busy"}[busy], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			lim := Limits{MaxRunsPerDay: 1}
			if busy {
				lim.MaxConcurrentRuns = 1
			}
			h.donor(t, "a", lim, func(p *Pledge) { p.Priority = 0 })
			h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
			other, err := h.broker.Acquire(ctx, h.request("other")) // a's one run of the day, still in flight
			if err != nil || other.DonorID != "a" {
				t.Fatalf("other: %+v %v", other, err)
			}
			g1 := rva2Acquire(t, h, "r")
			t.Logf("r served by %s held=%v (a: runs/day spent, busy=%v)", g1.DonorID, g1.Held, busy)
			_ = h.broker.Report(ctx, "r", Outcome{CostUSD: 0.1})
			_ = h.broker.Report(ctx, "other", Outcome{CostUSD: 0.1})
			h.now = h.now.Add(24 * time.Hour)
			g2 := rva2Acquire(t, h, "r")
			t.Logf("r's resume after a's day reset: served by %s held=%v", g2.DonorID, g2.Held)
			if g1.DonorID != "b" || !g1.Held {
				t.Errorf("a's runs/day ceiling is spent and the run moved to b — not held (busy=%v)", busy)
			}
		})
	}
}

// M5 — the acquisition floor moves a lease across midnight from the
// instant its admission was judged: the unit is reserved on day D, the
// lease is stamped D+1, and its release refunds D+1 — a unit of D+1 the
// run never took — so D+1 admits one run past the donor's MaxRunsPerDay.
func TestPoolGuarantee_M5_theFloorRefundsTheNextDay(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "alice", Limits{})
	gw := h.r4GatewayDonor(t, "bob", Limits{MaxRunsPerDay: 1})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
	gwWant := Credential{Source: SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible)}
	h.now = time.Date(2026, 8, 3, 23, 59, 59, 999_000_000, time.UTC)

	if _, err := h.broker.Acquire(ctx, r4AttReq("x", claudeWant(), attID, lims, "X1", false)); err != nil {
		t.Fatal(err)
	}
	gx, err := h.broker.Acquire(ctx, r4AttReq("x", gwWant, attID, lims, "X1", true)) // same millisecond: floored to 00:00:00.000
	if err != nil {
		var nd *NoDonorError
		if errors.As(err, &nd) {
			t.Logf("skips: %+v", nd.Skips)
		}
		p, _ := h.pledges.Get(ctx, gw.ID)
		t.Fatalf("x gateway: %v (pledge health=%s detail=%q)", err, p.Health, p.HealthDetail)
	}
	h.now = h.now.Add(2 * time.Second) // day D+1
	gy, _ := h.broker.Acquire(ctx, r4AttReq("y", gwWant, attID, lims, "Y1", false))
	h.broker.ReleaseGrant(ctx, gx) // the walk refuses x's gateway grant (no URL)
	gz, errz := h.broker.Acquire(ctx, r4AttReq("z", gwWant, attID, lims, "Z1", false))
	t.Logf("x's grant acquired %s (reserved on %s); y=%v z=%v (err=%v) on %s, bob MaxRunsPerDay=1",
		gx.AcquiredAt.Format(time.RFC3339Nano), "2026-08-03", gy != nil, gz != nil, errz, dayKey(h.now))
	if gy != nil && gz != nil {
		t.Errorf("bob served y AND z on %s under MaxRunsPerDay=1: x's refund landed on the day its floored lease names", dayKey(h.now))
	}
	_ = gw
}

// r4HookLedger runs a hook once, right after the att: row refuses a
// Reserve: the deterministic interleaving of a concurrent release landing
// between two candidates of one walk.
type r4HookLedger struct {
	*MemoryLedger
	hook func()
}

func (l *r4HookLedger) Reserve(ctx context.Context, pledgeID string, when time.Time, lim Limits, live LiveCommitment) (float64, DenyReason, error) {
	rem, deny, err := l.MemoryLedger.Reserve(ctx, pledgeID, when, lim, live)
	if deny != DenyNone && IsAttachmentLedgerID(pledgeID) && l.hook != nil {
		h := l.hook
		l.hook = nil
		h()
	}
	return rem, deny, err
}

// M6 — I5: the BORROWER's own draw cap refusing the first candidate sets
// capped=true (tryPledge's att: deny) and arms rankedDeclined. When another
// run's grant is released between two candidates (a concurrent release —
// interleaved deterministically here), the second candidate is admitted
// and its lease is HELD although no entry was exhausted: the run is then
// pinned off its first choice until a manual reset.
func TestPoolGuarantee_M6_theScopesOwnDrawCapArmsAHold(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	hl := &r4HookLedger{MemoryLedger: h.ledger}
	h.broker = NewBroker(BrokerConfig{Pools: h.pools, Pledges: h.pledges, Leases: h.leases, Ledger: hl,
		OAuth: h.oauth, APIKeys: h.apiKeys, Sealer: h.sealer, Now: func() time.Time { return h.now }})
	h.donor(t, "first", Limits{}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "second", Limits{}, func(p *Pledge) { p.Priority = 5 })
	poolIDs, attID, lims := attachForTest(t, h, Strategy{Mode: StrategyPriority, ResetHold: true}, DrawLimits{MaxRunsPerDay: 1})

	x, err := h.broker.Acquire(ctx, holdRequestWithDraw(h, poolIDs, attID, lims, "x"))
	if err != nil {
		t.Fatal(err)
	}
	hl.hook = func() { h.broker.ReleaseGrant(ctx, x) } // x's launch fails right then
	g, err := h.broker.Acquire(ctx, holdRequestWithDraw(h, poolIDs, attID, lims, "r"))
	if err != nil {
		t.Logf("r: no donor (%v) — no hold either", err)
		return
	}
	t.Logf("r served by %s held=%v (no entry exhausted; the scope's draw cap refused 'first' while x held the day's draw unit)", g.DonorID, g.Held)
	if g.Held {
		t.Errorf("a hold armed by the borrower's own draw cap, not by any entry's exhaustion")
	}
}

func holdRequestWithDraw(h *harness, poolIDs []string, attID string, lims DrawLimits, runID string) Request {
	r := holdRequest(h, poolIDs, attID, runID, nil)
	r.UserID = "requester"
	r.DrawLimits = lims
	return r
}

// M2b — the shape the randomized run found: siblings acquired inside one
// millisecond (the floor stamps the second 1 ms later), so the publication
// pins the FIRST lease as the attempt's anchor — the coarse booker; a
// sweeper tick between the two expiries abandons only the anchor.
func TestPoolGuarantee_M2b_coarseReportWithAnAbandonedAnchorBooksNoAttRow(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "alice", Limits{})
	bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now
	g1, err := h.broker.Acquire(ctx, r4AttReq("run-l", claudeWant(), attID, lims, "A1", false))
	if err != nil {
		t.Fatal(err)
	}
	g2, err := h.broker.Acquire(ctx, r4AttReq("run-l", Credential{Source: SourceAPIKey, Ref: "anthropic"}, attID, lims, "A1", true))
	if err != nil {
		t.Fatal(err)
	}
	pub := store.PublishAt(h.now, g1.AcquiredAt)
	t.Logf("leases %s / %s, publication %s", g1.AcquiredAt.Format("05.000"), g2.AcquiredAt.Format("05.000"), pub.Format("05.000"))
	h.now = g1.AcquiredAt.Add(DefaultLeaseTTL) // the first lease's expiry, not the second's
	if n, _ := h.broker.ReleaseExpired(ctx, 200); n != 1 {
		t.Fatalf("sweeper freed %d, want the anchor only", n)
	}
	h.now = h.now.Add(time.Hour)
	if err := h.broker.ReportAttempt(ctx, "run-l", pub, Outcome{CostUSD: 3}); err != nil {
		t.Fatal(err)
	}
	att := rva2Spent(t, h, AttachmentLedgerID(attID), day)
	t.Logf("coarse $3 attempt: bob=$%.2f att:row=$%.2f", rva2Spent(t, h, bob.ID, day), att)
	if att != 3 {
		t.Errorf("the att: row booked $%.2f of a $3 attempt whose open sibling was charged it whole", att)
	}
}

// M7 — order dependence: run r's attempt A1 took entry a's ONLY daily
// unit. Its resume R2 lands inside A1's unwind window. If A1's late report
// lands FIRST, a's lease reads served and R2 renews on a (no switch). If
// R2's acquisition lands first, its own takeover supersedes a's lease
// (a non-admission), R2 must Reserve a second unit, a refuses on runs/day —
// r's own unit — and R2 switches to b AND HOLDS it until a manual reset.
func TestPoolGuarantee_M7_theHoldDependsOnWhetherTheLateReportBeatTheResume(t *testing.T) {
	for _, reportFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "late-report-first", false: "resume-first"}[reportFirst], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			h.donor(t, "a", Limits{MaxRunsPerDay: 1}, func(p *Pledge) { p.Priority = 0 })
			h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
			g1 := rva2Acquire(t, h, "r")
			pub1 := store.PublishAt(h.now.Add(20*time.Millisecond), g1.AcquiredAt)
			h.now = h.now.Add(time.Minute)
			if reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub1, Outcome{CostUSD: 0.1})
			}
			g2 := rva2Acquire(t, h, "r")
			if !reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub1, Outcome{CostUSD: 0.1})
			}
			t.Logf("A1 on %s; R2 served by %s held=%v", g1.DonorID, g2.DonorID, g2.Held)
			// The switch to b on a resume-first ordering is the existing
			// doctrine (a superseded lease is a non-admission); the HOLD is
			// what must not depend on the ordering — r's own unit is no
			// exhaustion of a for r.
			if g2.Held {
				t.Errorf("same run, same entry, same unit: R2 on %s HELD only because its acquisition beat A1's late report", g2.DonorID)
			}
		})
	}
}
