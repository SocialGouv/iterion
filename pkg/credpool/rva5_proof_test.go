package credpool

// Round-5 adversarial proofs (closing round of the #3123 loop). Each test is
// self-contained (r5 helpers) so it compiles on the pre-round-4 tree too,
// where it serves as the regression witness.

import (
	"context"
	"errors"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r5HookLeases runs a hook once, right after a Close of the named lease
// returns: the deterministic interleaving of a second report landing between
// the first report's settlement of the anchor and of its sibling ("a cancel
// racing a finish"). failID makes one Close of that lease fail (a store blip).
type r5HookLeases struct {
	*MemoryLeaseStore
	hookID string
	hook   func()
	failID string
}

func (s *r5HookLeases) Close(ctx context.Context, leaseID string, cost float64, outcome string, when time.Time) (bool, error) {
	if leaseID == s.failID {
		s.failID = ""
		return false, errors.New("mongo: connection reset")
	}
	won, err := s.MemoryLeaseStore.Close(ctx, leaseID, cost, outcome, when)
	if leaseID == s.hookID && s.hook != nil {
		h := s.hook
		s.hook = nil
		h()
	}
	return won, err
}

func r5Broker(h *harness, leases LeaseStore) *Broker {
	return NewBroker(BrokerConfig{Pools: h.pools, Pledges: h.pledges, Leases: leases, Ledger: h.ledger,
		OAuth: h.oauth, APIKeys: h.apiKeys, Sealer: h.sealer,
		Logger: iterlog.New(iterlog.LevelError, nil), Now: func() time.Time { return h.now }})
}

func r5AttReq(runID string, want Credential, attID string, lims DrawLimits, attemptID string, side bool) Request {
	return Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester", BotID: "docs-refresh",
		Wants: []Credential{want}, PoolIDs: []string{"pool-1"}, AttachmentID: attID, DrawLimits: lims,
		SideBySide: side, AttemptID: attemptID}
}

// r5SideBySide opens one attempt's two side-by-side leases (forfait +
// key) under an attachment and returns the anchor grant, the sibling grant
// and the attempt's publication.
func r5SideBySide(t *testing.T, h *harness, b *Broker, attID string, lims DrawLimits) (*Grant, *Grant, time.Time) {
	t.Helper()
	ctx := context.Background()
	g1, err := b.Acquire(ctx, r5AttReq("run-d", claudeWant(), attID, lims, "A1", false))
	if err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(40 * time.Millisecond)
	g2, err := b.Acquire(ctx, r5AttReq("run-d", Credential{Source: SourceAPIKey, Ref: "anthropic"}, attID, lims, "A1", true))
	if err != nil {
		t.Fatal(err)
	}
	pub := store.PublishAt(h.now.Add(20*time.Millisecond), g1.AcquiredAt)
	h.now = h.now.Add(time.Minute)
	return g1, g2, pub
}

// F1 — two COARSE final reports of ONE attempt interleave (Report's own
// doc: "a cancel racing a finish"): R1 wins the anchor's CAS (books the att:
// row on it), R2 loses the anchor (charged=false) and wins the sibling's CAS
// before R1 reaches it — attBooked is per call, so R2 books the att: row a
// second time. Each donor is charged once; the borrower's att: row twice.
func TestPoolReview5_F1_interleavedDuplicateCoarseReportsBookTheAttRowTwice(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	hl := &r5HookLeases{MemoryLeaseStore: h.leases}
	b := r5Broker(h, hl)
	alice := h.donor(t, "alice", Limits{})
	bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
	_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
	day := h.now
	_, g2, pub := r5SideBySide(t, h, b, attID, lims)

	// The publication pins the NEWEST lease acquired before it — the key
	// slot's (g2) — as the attempt's anchor, hence the coarse booker.
	hl.hookID = g2.releaseGuard.lease.ID
	var r2 error
	hl.hook = func() { r2 = b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 3}) }
	r1 := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 3})
	att := rva2Spent(t, h, AttachmentLedgerID(attID), day)
	t.Logf("one $3 attempt, two coarse finals interleaved (r1=%v r2=%v): alice=$%.2f bob=$%.2f att:row=$%.2f",
		r1, r2, rva2Spent(t, h, alice.ID, day), rva2Spent(t, h, bob.ID, day), att)
	if att != 3 {
		t.Errorf("the borrower's att: row booked $%.2f for ONE $3 attempt reported twice (want $3: a redelivery loses the CAS)", att)
	}
}

// F1b — the sequential shape: one close of the first final fails (a store
// blip; the report returns the error), then the redelivered final of the
// same attempt wins the CAS the first one could not. Variant "booker": the
// booker's close fails, R1 hands the att: row to the sibling (charged), R2
// books it again on the booker. Variant "sibling": R1 books on the booker,
// the sibling's close fails, R2 loses the booker and books on the sibling.
func TestPoolReview5_F1b_aBlipThenTheRedeliveryBooksTheAttRowTwice(t *testing.T) {
	for _, blipOnBooker := range []bool{true, false} {
		t.Run(map[bool]string{true: "blip-on-booker", false: "blip-on-sibling"}[blipOnBooker], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			hl := &r5HookLeases{MemoryLeaseStore: h.leases}
			b := r5Broker(h, hl)
			alice := h.donor(t, "alice", Limits{})
			bob, _ := h.donorKey(t, "bob", "anthropic", Limits{})
			_, attID, lims := attachForTest(t, h, Strategy{}, DrawLimits{})
			day := h.now
			g1, g2, pub := r5SideBySide(t, h, b, attID, lims)
			if blipOnBooker {
				hl.failID = g2.releaseGuard.lease.ID
			} else {
				hl.failID = g1.releaseGuard.lease.ID
			}
			r1 := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 3})
			r2 := b.ReportAttempt(ctx, "run-d", pub, Outcome{CostUSD: 3}) // the redelivered final
			att := rva2Spent(t, h, AttachmentLedgerID(attID), day)
			t.Logf("first final: %v; redelivered final: %v; alice=$%.2f bob=$%.2f att:row=$%.2f",
				r1, r2, rva2Spent(t, h, alice.ID, day), rva2Spent(t, h, bob.ID, day), att)
			if att != 3 {
				t.Errorf("the att: row booked $%.2f for ONE $3 attempt (want $3)", att)
			}
		})
	}
}

// r5Req is the publisher's per-slot request for one forfait slot with the
// hold dial (rva2Acquire's), runID free.
func r5Acquire(t *testing.T, h *harness, runID string, hold bool) *Grant {
	t.Helper()
	ctx := context.Background()
	held := ""
	if hold {
		var err error
		if held, err = h.broker.HeldPledge(ctx, runID, claudeWant()); err != nil {
			t.Fatal(err)
		}
	}
	g, err := h.broker.Acquire(ctx, Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester",
		BotID: "docs-refresh", PoolIDs: []string{"pool-1"}, RankMode: StrategyPriority, HoldOnSwitch: hold,
		PreferPledgeID: held, Wants: []Credential{claudeWant()}})
	if err != nil {
		t.Fatalf("acquire %s: %v", runID, err)
	}
	return g
}

// F2 — M7 exempts ANY own unit, an ABANDONED one included: run r's lease on
// a (MaxRunsPerDay 1) was swept as abandoned — a lease no late report can
// ever re-admit — and on the NEXT day another run took a's only unit. r's
// resume is refused by a on runs/day for a reason that has nothing to do with
// r (its own unit sits in yesterday's bucket), yet M7 reads "own earlier
// unit — busy": the switch to b is not held, and once a's day resets r
// flips back — the thrash reset_hold exists to stop.
func TestPoolReview5_F2_anAbandonedUnitOfAnotherDayDisarmsALegitimateHold(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donor(t, "a", Limits{MaxRunsPerDay: 1}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })

	g0 := r5Acquire(t, h, "r", true) // day D, 12:00: a's unit of D
	h.now = h.now.Add(DefaultLeaseTTL + time.Minute)
	if n, _ := h.broker.ReleaseExpired(ctx, 100); n != 1 {
		t.Fatalf("sweeper freed %d", n)
	}
	other := r5Acquire(t, h, "other", false) // day D+1: a's only unit of D+1
	_ = h.broker.Report(ctx, "other", Outcome{CostUSD: 0.1})
	h.now = h.now.Add(time.Hour)
	g1 := r5Acquire(t, h, "r", true)
	_ = h.broker.Report(ctx, "r", Outcome{CostUSD: 0.1})
	h.now = h.now.Add(24 * time.Hour) // a's day resets
	g2 := r5Acquire(t, h, "r", true)
	t.Logf("D: r on %s (abandoned); D+1: other on %s; r's resume on %s held=%v; D+2: r on %s held=%v",
		g0.DonorID, other.DonorID, g1.DonorID, g1.Held, g2.DonorID, g2.Held)
	if g1.DonorID != "b" || !g1.Held {
		t.Errorf("a is exhausted for r on D+1 (another run took its only unit; r's abandoned unit is D's) — the switch to b is not held")
	}
	if g2.DonorID != "b" {
		t.Errorf("r flipped back to %s after a's day reset", g2.DonorID)
	}
}

// F2b — same day: r's lease on a (MaxRunsPerDay 2) was swept as abandoned,
// another run took a's second unit; r's resume is refused on runs/day. No
// late report can ever turn r's abandoned lease into an admission, so no
// ordering can make a serve r: a declined for exhaustion, deterministically.
func TestPoolReview5_F2b_anAbandonedUnitOfTheSameDayDisarmsTheHold(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.now = time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC)
	h.donor(t, "a", Limits{MaxRunsPerDay: 2}, func(p *Pledge) { p.Priority = 0 })
	h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
	r5Acquire(t, h, "r", true)
	h.now = h.now.Add(DefaultLeaseTTL + time.Minute)
	if n, _ := h.broker.ReleaseExpired(ctx, 100); n != 1 {
		t.Fatalf("sweeper freed %d", n)
	}
	r5Acquire(t, h, "other", false)
	_ = h.broker.Report(ctx, "other", Outcome{CostUSD: 0.1})
	g1 := r5Acquire(t, h, "r", true)
	t.Logf("r's resume after its abandoned lease, a full for the day: served by %s held=%v", g1.DonorID, g1.Held)
	if g1.DonorID != "b" || !g1.Held {
		t.Errorf("served by %s held=%v, want b HELD (an abandoned lease is never re-admitted: no ordering serves r on a)", g1.DonorID, g1.Held)
	}
}

// F3 — the class M7 closes on the runs/day branch is open on the
// concurrency branch round 4 added: a refused as BUSY reads its usage, and
// day.Runs+1 counts r's OWN superseded unit as exhaustion. Whether A1's
// late report landed before r's next resume decides the hold — exactly what
// M7 says must not.
func TestPoolReview5_F3_theConcurrencyBranchCountsTheRunsOwnUnit(t *testing.T) {
	for _, reportFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "late-report-before-R3", false: "late-report-after-R3"}[reportFirst], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			h.donor(t, "a", Limits{MaxRunsPerDay: 1, MaxConcurrentRuns: 1}, func(p *Pledge) { p.Priority = 0 })
			h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
			// s: a run a admitted yesterday — its resume renews, no unit.
			h.now = time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
			if g := r5Acquire(t, h, "s", false); g.DonorID != "a" {
				t.Fatalf("s on %s", g.DonorID)
			}
			_ = h.broker.Report(ctx, "s", Outcome{CostUSD: 0.1})

			h.now = time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
			g1 := r5Acquire(t, h, "r", true) // A1 on a: a's only unit of the day
			pub1 := store.PublishAt(h.now.Add(20*time.Millisecond), g1.AcquiredAt)
			h.now = h.now.Add(time.Minute)
			g2 := r5Acquire(t, h, "r", true) // R2: takes A1 over; runs/day → M7 busy → b, no hold
			if reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub1, Outcome{CostUSD: 0.1}) // A1's late report
			}
			h.now = h.now.Add(time.Minute)
			gs := r5Acquire(t, h, "s", false) // s resumes: renews on a, takes its only slot
			h.now = h.now.Add(time.Minute)
			g3 := r5Acquire(t, h, "r", true) // R3
			if !reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub1, Outcome{CostUSD: 0.1})
			}
			t.Logf("A1 on %s; R2 on %s held=%v; s on %s; R3 on %s held=%v", g1.DonorID, g2.DonorID, g2.Held, gs.DonorID, g3.DonorID, g3.Held)
			if g3.Held {
				t.Errorf("R3 on %s HELD only because its acquisition beat A1's late report (a refused busy; its runs/day 'exhaustion' is r's own unit)", g3.DonorID)
			}
		})
	}
}

// F2-control — a SUPERSEDED own unit of the previous day is M7's case done
// right: the late report re-admits it (superseded_reported → renewal, no
// runs/day check), so a serves r in the report-first world — "busy, no hold"
// in the resume-first world matches it. Only the ABANDONED unit (F2) has no
// such world.
func TestPoolReview5_F2control_aSupersededUnitOfThePreviousDayIsReadmittedByItsLateReport(t *testing.T) {
	for _, reportFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "late-report-first", false: "resume-first"}[reportFirst], func(t *testing.T) {
			ctx := context.Background()
			h := newHarnessWithAttachments(t)
			h.now = time.Date(2026, 8, 3, 23, 50, 0, 0, time.UTC)
			h.donor(t, "a", Limits{MaxRunsPerDay: 1}, func(p *Pledge) { p.Priority = 0 })
			h.donor(t, "b", Limits{}, func(p *Pledge) { p.Priority = 5 })
			g0 := r5Acquire(t, h, "r", true) // A1 on a, unit of D
			pub := store.PublishAt(h.now.Add(20*time.Millisecond), g0.AcquiredAt)
			h.now = time.Date(2026, 8, 4, 0, 10, 0, 0, time.UTC)
			// R2 supersedes A1 first in BOTH worlds: Acquire closes A1's lease.
			if _, err := h.broker.Acquire(ctx, Request{RunID: "r", OrgID: testOrg, TenantID: "team-1", UserID: "requester",
				BotID: "docs-refresh", PoolIDs: []string{"pool-1"}, RankMode: StrategyPriority,
				Wants: []Credential{{Source: SourceAPIKey, Ref: "none"}}}); err == nil {
				t.Fatal("setup: the probe must find no donor")
			}
			other := r5Acquire(t, h, "other", false) // a's only unit of D+1
			_ = h.broker.Report(ctx, "other", Outcome{CostUSD: 0.1})
			if reportFirst {
				_ = h.broker.ReportAttempt(ctx, "r", pub, Outcome{CostUSD: 0.1})
			}
			g1 := r5Acquire(t, h, "r", true)
			t.Logf("other on %s; r's resume on %s held=%v", other.DonorID, g1.DonorID, g1.Held)
		})
	}
}
