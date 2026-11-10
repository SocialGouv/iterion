package credpool

// Settlement and hold guarantees under interleavings (#3147): a report whose
// lease moves between its snapshot and its CAS (a supersede, a reopen, the
// sweeper), the att: carrier, and the own-unit exemption against a spent $
// ceiling. The r6 hook store lands the other path deterministically.

import (
	"context"
	"errors"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r6HookLeases runs a hook once, right BEFORE a Close or a stamp of the named
// lease reaches the store: the deterministic interleaving of another path
// (the sweeper, a takeover's supersede, a rollback's reopen) landing between
// a report's snapshot of the attempt's leases and its CAS.
type r6HookLeases struct {
	*MemoryLeaseStore
	hookID string
	hook   func()
}

func (s *r6HookLeases) fire(leaseID string) {
	if leaseID == s.hookID && s.hook != nil {
		h := s.hook
		s.hook = nil
		h()
	}
}

func (s *r6HookLeases) Close(ctx context.Context, leaseID string, cost float64, outcome string, when time.Time) (bool, error) {
	s.fire(leaseID)
	return s.MemoryLeaseStore.Close(ctx, leaseID, cost, outcome, when)
}

func (s *r6HookLeases) StampSupersededReport(ctx context.Context, leaseID string, cost float64, when time.Time) (bool, error) {
	s.fire(leaseID)
	return s.MemoryLeaseStore.StampSupersededReport(ctx, leaseID, cost, when)
}

func r6Broker(h *harness, leases LeaseStore) *Broker {
	return NewBroker(BrokerConfig{Pools: h.pools, Pledges: h.pledges, Leases: leases, Ledger: h.ledger,
		OAuth: h.oauth, APIKeys: h.apiKeys, Sealer: h.sealer,
		Logger: iterlog.New(iterlog.LevelError, nil), Now: func() time.Time { return h.now }})
}

func r6AttReq(runID string, want Credential, attID string, lims DrawLimits, attemptID string, side bool) Request {
	return Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: "docs-refresh",
		Wants: []Credential{want}, PoolIDs: []string{"pool-1"}, AttachmentID: attID, DrawLimits: lims,
		SideBySide: side, AttemptID: attemptID}
}

// G1 — tryPledge's runs/day branch returns "busy, not exhausted" on the
// run's own unit BEFORE looking at the spend. Entry a's day is spent on BOTH
// axes (its 2 units: r's A1 and s; its $1: s's report). If A1's late report
// lands first, r's resume renews on a, Renew refuses on the spent $ and the
// switch to b is HELD; if the resume lands first, Reserve refuses on
// runs/day (checked before $), the own-unit exemption answers "serving" and
// the switch is NOT held — the very order dependence the exemption exists
// to remove, now in the opposite direction, on a really exhausted entry.
func TestPoolReview6_G1_theOwnUnitExemptionMasksASpentDollarCap(t *testing.T) {
	held := map[bool]bool{}
	for _, reportFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "late-report-first", false: "resume-first"}[reportFirst], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			h.donor(t, "a", Limits{MaxRunsPerDay: 2, MaxUSDPerDay: 1, MaxConcurrentRuns: 2}, func(p *Pledge) { p.Priority = 0 })
			h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
			g1 := rva2Acquire(t, h, "r") // A1 on a: unit 1 of the day
			pub1 := store.PublishAt(h.now.Add(20*time.Millisecond), g1.AcquiredAt)
			h.now = h.now.Add(time.Second)
			sreq := h.request("s")
			sreq.PoolIDs, sreq.RankMode = []string{"pool-1"}, StrategyPriority
			gs, err := h.broker.Acquire(ctx, sreq) // s on a: unit 2 of the day
			if err != nil || gs.DonorID != "a" {
				t.Fatalf("s: donor=%v %v", gs.DonorID, err)
			}
			if err := h.broker.Report(ctx, "s", Outcome{CostUSD: 1}); err != nil { // a's $1/day spent
				t.Fatal(err)
			}
			h.now = h.now.Add(time.Minute)
			if reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub1, Outcome{CostUSD: 0})
			}
			g2 := rva2Acquire(t, h, "r") // R2
			if !reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub1, Outcome{CostUSD: 0})
			}
			_ = h.broker.Report(ctx, "r", Outcome{CostUSD: 0.1})
			h.now = h.now.Add(24 * time.Hour) // a's day resets
			g3 := rva2Acquire(t, h, "r")
			day, _, _ := h.ledger.Usage(ctx, g1.PledgeID, g1.AcquiredAt)
			t.Logf("a's day: runs=%d/2 spent=$%.2f/$1; R2 on %s held=%v; after a's reset R3 on %s held=%v",
				day.Runs, day.CostUSD, g2.DonorID, g2.Held, g3.DonorID, g3.Held)
			held[reportFirst] = g2.Held
			if !g2.Held {
				t.Errorf("a's $1/day is SPENT (a real exhaustion for every run) — R2 moved to %s NOT held; R3 then went to %s", g2.DonorID, g3.DonorID)
			}
		})
	}
	if held[true] != held[false] {
		t.Errorf("the hold depends on whether A1's late report beat R2: report-first held=%v, resume-first held=%v", held[true], held[false])
	}
}

// r6SideBySide opens one attempt's two side-by-side leases (forfait + key)
// under an attachment through broker b; returns the anchor's sibling grant
// (g1), the anchor grant (g2: the newest lease, the publication's pin) and
// the attempt's publication.
func r6SideBySide(t *testing.T, h *harness, b *Broker, runID, attID string, lims DrawLimits) (*Grant, *Grant, time.Time) {
	t.Helper()
	ctx := context.Background()
	g1, err := b.Acquire(ctx, r6AttReq(runID, claudeWant(), attID, lims, "A1", false))
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(40 * time.Millisecond)
	g2, err := b.Acquire(ctx, r6AttReq(runID, Credential{Source: SourceAPIKey, Ref: "anthropic"}, attID, lims, "A1", true))
	if err != nil {
		t.Fatal(err)
	}
	pub := store.PublishAt(h.now.Add(20*time.Millisecond), g1.AcquiredAt)
	return g1, g2, pub
}

// G2 — the att: carrier is chosen from a SNAPSHOT; its CAS can still lose to
// a non-booking path between the snapshot and the close (the sweeper here):
// the carrier books nothing, every other lease had its AttachmentID cleared,
// so the borrower's att: row books nothing while bob's lease books the whole
// attempt. Round 4 ("first lease whose settlement charged") moved the row
// to the sibling.
func TestPoolReview6_G2_aSweepBetweenSnapshotAndCASLeavesTheAttRowUnbooked(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	hl := &r6HookLeases{MemoryLeaseStore: h.leases}
	b := r6Broker(h, hl)
	alice := h.donor(t, "alice", Limits{})
	bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now
	_, g2, pub := r6SideBySide(t, h, b, "run-d", attID, lims)
	anchor := g2.releaseGuard.lease.ID
	h.now = h.now.Add(time.Hour)
	hl.hookID = anchor
	hl.hook = func() {
		// The sweeper's CAS lands first on the anchor (its TTL is the
		// earliest of the attempt's), exactly between the report's
		// ListByRun and its Close.
		if won, err := h.leases.Close(ctx, anchor, 0, OutcomeAbandoned, h.now); !won || err != nil {
			t.Fatalf("sweep: won=%v err=%v", won, err)
		}
	}
	if err := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 3}); err != nil {
		t.Fatal(err)
	}
	att := rva2Spent(t, h, AttachmentLedgerID(attID), day)
	t.Logf("coarse $3 final, sweep between snapshot and CAS: alice=$%.2f bob=$%.2f att:row=$%.2f",
		rva2Spent(t, h, alice.ID, day), rva2Spent(t, h, bob.ID, day), att)
	if att != 3 {
		t.Errorf("the att: row booked $%.2f of a $3 attempt one of whose leases booked it whole", att)
	}
}

// G3 (pre-existing, outside round 5's diff) — a FINAL report's snapshot reads
// its lease OPEN; the run's resume supersedes it before the report's Close
// CAS. The Close loses, settleLease reads the loss as "another report got
// there first; it did the charging" — but the winner was the supersede,
// which charges nothing, and the stamp path is never tried: the attempt's
// spend reaches no donor and no att: row, and the lease stays a plain
// "superseded" (a non-admission) forever.
func TestPoolReview6_G3_aSupersedeBetweenSnapshotAndCloseDropsTheWholeCharge(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	hl := &r6HookLeases{MemoryLeaseStore: h.leases}
	b := r6Broker(h, hl)
	alice := h.donor(t, "alice", Limits{})
	bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now
	_, g2, pub := r6SideBySide(t, h, b, "run-d", attID, lims)
	h.now = h.now.Add(time.Hour)
	hl.hookID = g2.releaseGuard.lease.ID
	hl.hook = func() {
		// The run's resume R2: its acquisition supersedes A1's open leases.
		if _, err := b.Acquire(ctx, r6AttReq("run-d", claudeWant(), attID, lims, "A2", false)); err != nil && !errors.Is(err, ErrNoDonor) {
			t.Fatalf("R2: %v", err)
		}
	}
	if err := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 3}); err != nil {
		t.Fatal(err)
	}
	ls, _ := h.leases.ListByRun(ctx, "run-d")
	for _, l := range ls {
		t.Logf("  lease %.8s attempt=%s pledge=%s closed=%v outcome=%q cost=%.2f", l.ID, l.AttemptID, l.PledgeID, l.Closed, l.Outcome, l.CostUSD)
	}
	a, bb, att := rva2Spent(t, h, alice.ID, day), rva2Spent(t, h, bob.ID, day), rva2Spent(t, h, AttachmentLedgerID(attID), day)
	t.Logf("coarse $3 final, resume between snapshot and CAS: alice=$%.2f bob=$%.2f att:row=$%.2f", a, bb, att)
	if a != 3 || bb != 3 || att != 3 {
		t.Errorf("A1 spent $3 on both credentials (coarse): alice=$%.2f bob=$%.2f att=$%.2f, want $3 each", a, bb, att)
	}
}

// G3b — the reverse move: the report's snapshot reads A1's leases
// SUPERSEDED by the resume R2, and R2's takeover is rolled back (its lease
// released, the superseded ones reopened) between the snapshot and the
// stamp. The stamp loses — the lease is open again — and the report must
// close it: it is still this attempt's final report, and nothing else will
// book its spend.
func TestPoolReview6_G3b_aStampLostToAReopenClosesTheLease(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	hl := &r6HookLeases{MemoryLeaseStore: h.leases}
	b := r6Broker(h, hl)
	alice := h.donor(t, "alice", Limits{})
	bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now
	_, g2, pub := r6SideBySide(t, h, b, "run-d", attID, lims)
	h.now = h.now.Add(time.Hour)
	r2, err := b.Acquire(ctx, r6AttReq("run-d", claudeWant(), attID, lims, "A2", false))
	if err != nil {
		t.Fatalf("R2: %v", err)
	}
	hl.hookID = g2.releaseGuard.lease.ID
	hl.hook = func() {
		// R2's takeover did not happen: its grant goes back, A1 goes on.
		b.ReleaseGrant(ctx, r2)
		b.ReopenSuperseded(ctx, r2)
	}
	if err := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 3}); err != nil {
		t.Fatal(err)
	}
	ls, _ := h.leases.ListByRun(ctx, "run-d")
	for _, l := range ls {
		t.Logf("  lease %.8s attempt=%s pledge=%s closed=%v outcome=%q cost=%.2f", l.ID, l.AttemptID, l.PledgeID, l.Closed, l.Outcome, l.CostUSD)
		if l.AttemptID == "A1" && (!l.Closed || l.Outcome != "ok") {
			t.Errorf("A1's lease %.8s on %s: closed=%v outcome=%q after its final report, want closed ok", l.ID, l.PledgeID, l.Closed, l.Outcome)
		}
	}
	a, bb, att := rva2Spent(t, h, alice.ID, day), rva2Spent(t, h, bob.ID, day), rva2Spent(t, h, AttachmentLedgerID(attID), day)
	t.Logf("coarse $3 final, reopen between snapshot and stamp: alice=$%.2f bob=$%.2f att:row=$%.2f", a, bb, att)
	if a != 3 || bb != 3 || att != 3 {
		t.Errorf("A1 spent $3 on both credentials (coarse): alice=$%.2f bob=$%.2f att=$%.2f, want $3 each", a, bb, att)
	}
}

// G4 — control: an INTERIM then a FINAL coarse report of one attempt book the
// att: row once each (two deliveries, two spends) on the same carrier, and a
// FINE report books each lease's own slice on the att: row — whatever the
// carrier rule. Run on both heads: the numbers must not differ.
func TestPoolReview6_G4_controlInterimThenFinalAndFine(t *testing.T) {
	for _, fine := range []bool{false, true} {
		t.Run(map[bool]string{false: "coarse-interim-then-final", true: "fine-with-abandoned-anchor"}[fine], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			b := h.broker
			alice := h.donor(t, "alice", Limits{})
			rec, err := h.oauth.Get(ctx, "alice", secrets.OAuthKindClaudeCode)
			if err != nil {
				t.Fatal(err)
			}
			rec.Fingerprint = "fp-oauth-alice"
			if err := h.oauth.Upsert(ctx, rec); err != nil {
				t.Fatal(err)
			}
			bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
			_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
			day := h.now
			g1, g2, pub := r6SideBySide(t, h, b, "run-d", attID, lims)
			t.Logf("fingerprints: alice=%q bob=%q", g1.Fingerprint, g2.Fingerprint)
			h.now = h.now.Add(time.Hour)
			if !fine {
				if err := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 1, Interim: true}); err != nil {
					t.Fatal(err)
				}
				if err := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 2}); err != nil {
					t.Fatal(err)
				}
				if err := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 2}); err != nil { // redelivered final
					t.Fatal(err)
				}
			} else {
				if won, err := h.leases.Close(ctx, g2.releaseGuard.lease.ID, 0, OutcomeAbandoned, h.now); !won || err != nil {
					t.Fatalf("sweep: %v %v", won, err)
				}
				out := Outcome{CostUSD: 5, ByFingerprint: []FingerprintSpend{
					{Fingerprint: g1.Fingerprint, CostUSD: 2}, {Fingerprint: g2.Fingerprint, CostUSD: 3}}}
				if err := b.ReportAttempt(ctx, "run-d", pub, out); err != nil {
					t.Fatal(err)
				}
			}
			a, bb, att := rva2Spent(t, h, alice.ID, day), rva2Spent(t, h, bob.ID, day), rva2Spent(t, h, AttachmentLedgerID(attID), day)
			t.Logf("alice=$%.2f bob=$%.2f att:row=$%.2f", a, bb, att)
			if !fine && (a != 3 || bb != 3 || att != 3) {
				t.Errorf("coarse interim $1 + final $2 (+ a redelivery): want alice=bob=att=$3")
			}
			if fine && (a != 2 || bb != 0 || att != 2) {
				t.Errorf("fine $2/$3 with bob's lease abandoned: want alice=$2 bob=$0 att=$2")
			}
		})
	}
}

// G5 — the brief's question on the concurrency branch: with the own-unit
// exemption (runs = 0) a BUSY entry whose $ is really spent still reads
// capped — the exemption drops only the run count. F3's shape plus a spent
// $1/day ceiling: R3 must be HELD in both orderings.
func TestPoolReview6_G5_theConcurrencyBranchStillSeesASpentDollarCap(t *testing.T) {
	held := map[bool]bool{}
	for _, reportFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "late-report-before-R3", false: "late-report-after-R3"}[reportFirst], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			a := h.donor(t, "a", Limits{MaxRunsPerDay: 1, MaxConcurrentRuns: 1, MaxUSDPerDay: 1}, func(p *Pledge) { p.Priority = 0 })
			h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
			h.now = time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
			if g := r5Acquire(t, h, "s", false); g.DonorID != "a" {
				t.Fatalf("s on %s", g.DonorID)
			}
			_ = h.broker.Report(ctx, "s", Outcome{CostUSD: 0.1})
			h.now = time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
			g1 := r5Acquire(t, h, "r", true)
			pub1 := store.PublishAt(h.now.Add(20*time.Millisecond), g1.AcquiredAt)
			h.now = h.now.Add(time.Minute)
			g2 := r5Acquire(t, h, "r", true)
			if reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub1, Outcome{CostUSD: 0})
			}
			h.now = h.now.Add(time.Minute)
			gs := r5Acquire(t, h, "s", false)                                               // s renews on a: a's only slot
			if err := h.ledger.AddSpend(ctx, a.ID, gs.AcquiredAt, 1, 0, 0, 0); err != nil { // a's $1 of the day spent
				t.Fatal(err)
			}
			h.now = h.now.Add(time.Minute)
			g3 := r5Acquire(t, h, "r", true)
			if !reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub1, Outcome{CostUSD: 0})
			}
			t.Logf("A1 on %s; R2 on %s held=%v; s on %s; R3 on %s held=%v", g1.DonorID, g2.DonorID, g2.Held, gs.DonorID, g3.DonorID, g3.Held)
			held[reportFirst] = g3.Held
			if !g3.Held {
				t.Errorf("a is busy AND its $1/day is spent: R3's switch to %s must hold", g3.DonorID)
			}
		})
	}
	if held[true] != held[false] {
		t.Errorf("order-dependent hold: %v vs %v", held[true], held[false])
	}
}

// G6 — the not_launched clause of closedWithoutBooking is seen by no test
// (the mutant that drops it survives the package, the randomized suite
// included). M2b's shape with three slots: the publication pins the OLDEST
// lease (k0) as the anchor, the walk released the gateway grant (k2,
// not_launched), the sweeper closed k0 at its expiry, k1 is still open. The
// att: row must ride k1.
func TestPoolReview6_G6_aReturnedSiblingNeverCarriesTheAttRow(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "alice", Limits{})
	bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
	h.r4GatewayDonor(t, "carol", Limits{})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now
	g0, err := h.broker.Acquire(ctx, r6AttReq("run-g", claudeWant(), attID, lims, "A1", false))
	if err != nil {
		t.Fatal(err)
	}
	g1, err := h.broker.Acquire(ctx, r6AttReq("run-g", Credential{Source: SourceAPIKey, Ref: "anthropic"}, attID, lims, "A1", true))
	if err != nil {
		t.Fatal(err)
	}
	g2, err := h.broker.Acquire(ctx, r6AttReq("run-g", Credential{Source: SourceAPIKey, Ref: "openai_compatible"}, attID, lims, "A1", true))
	if err != nil {
		t.Fatal(err)
	}
	h.broker.ReleaseGrant(ctx, g2) // the walk refuses the gateway grant
	pub := store.PublishAt(h.now, g0.AcquiredAt)
	t.Logf("acq k0=%s k1=%s k2=%s pub=%s", g0.AcquiredAt.Format("05.000"), g1.AcquiredAt.Format("05.000"), g2.AcquiredAt.Format("05.000"), pub.Format("05.000"))
	h.now = g0.AcquiredAt.Add(DefaultLeaseTTL)
	if n, _ := h.broker.ReleaseExpired(ctx, 200); n != 1 {
		t.Fatalf("sweeper freed %d, want the anchor only", n)
	}
	h.now = h.now.Add(time.Hour)
	if err := h.broker.ReportAttempt(ctx, "run-g", pub, Outcome{CostUSD: 3}); err != nil {
		t.Fatal(err)
	}
	att := rva2Spent(t, h, AttachmentLedgerID(attID), day)
	t.Logf("coarse $3: bob=$%.2f att:row=$%.2f", rva2Spent(t, h, bob.ID, day), att)
	if att != 3 {
		t.Errorf("the att: row booked $%.2f of a $3 attempt whose open sibling booked it whole", att)
	}
}
