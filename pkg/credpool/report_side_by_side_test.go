package credpool

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// rvaAttempt numbers the attempts the requests below belong to, exactly as
// the attached walk does: a non-sibling acquisition opens a new attempt
// (one AttemptID per resolution), its siblings share it.
var rvaAttempt int

// rvaKeyReq is the attached walk's per-slot request for one API-key slot.
func rvaKeyReq(runID, prov string, sideBySide bool) Request {
	if !sideBySide {
		rvaAttempt++
	}
	return Request{RunID: runID, OrgID: testOrg, TenantID: "team-1", UserID: "requester",
		BotID: "docs-refresh", Wants: []Credential{{Source: SourceAPIKey, Ref: prov}}, SideBySide: sideBySide,
		AttemptID: fmt.Sprintf("%s-attempt-%d", runID, rvaAttempt)}
}

func rvaDump(t *testing.T, h *harness, runID, label string) {
	t.Helper()
	all, err := h.leases.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range all {
		t.Logf("%s: lease %.8s pledge=%-28s acq=%s closed=%v outcome=%-20q cost=%.2f held=%v",
			label, l.ID, l.PledgeID, l.AcquiredAt.Format("15:04:05.000"), l.Closed, l.Outcome, l.CostUSD, l.Held)
	}
}

func rvaSpent(t *testing.T, h *harness, pledgeID string, at time.Time) float64 {
	t.Helper()
	day, _, err := h.ledger.Usage(context.Background(), pledgeID, at)
	if err != nil {
		t.Fatal(err)
	}
	return day.CostUSD
}

// R1: the late teardown report of a side-by-side attempt (the unwind
// window ReportAttempt's own doc names) after a resume took it over.
// takeover=true is 3d0878e19's walk (first acquisition SideBySide=false);
// takeover=false is the pre-commit walk (every acquisition SideBySide).
func TestSideBySideReport_R1_lateReportOfASupersededSideBySideAttempt(t *testing.T) {
	for _, takeover := range []bool{true} {
		t.Run(map[bool]string{true: "post-3d0878e19", false: "pre-commit"}[takeover], func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 100})
			h.donorKey(t, "bob", "openai", Limits{MaxUSDPerDay: 100})
			pa := PledgeID("alice", SourceAPIKey, "anthropic")
			pb := PledgeID("bob", SourceAPIKey, "openai")
			fpA := secrets.FingerprintSHA256("sk-real-alice")
			fpB := secrets.FingerprintSHA256("sk-real-bob")
			day := h.now

			// Attempt A1: forfait-like slot first (takes over), sibling second.
			a1, err := h.broker.Acquire(ctx, rvaKeyReq("run-x", "anthropic", false))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.broker.Acquire(ctx, rvaKeyReq("run-x", "openai", true)); err != nil {
				t.Fatal(err)
			}
			P1 := store.PublishAt(h.now.Add(50*time.Millisecond), a1.AcquiredAt)

			// A1's teardown takes minutes: a resume lands inside it.
			h.now = h.now.Add(2 * time.Minute)
			a2, err := h.broker.Acquire(ctx, rvaKeyReq("run-x", "anthropic", !takeover))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.broker.Acquire(ctx, rvaKeyReq("run-x", "openai", true)); err != nil {
				t.Fatal(err)
			}
			P2 := store.PublishAt(h.now.Add(50*time.Millisecond), a2.AcquiredAt)
			rvaDump(t, h, "run-x", "after resume")

			// A1's late report: $1 on alice's key, $2 on bob's.
			h.now = h.now.Add(time.Minute)
			if err := h.broker.ReportAttempt(ctx, "run-x", P1, Outcome{CostUSD: 3, ByFingerprint: []FingerprintSpend{
				{Fingerprint: fpA, CostUSD: 1}, {Fingerprint: fpB, CostUSD: 2}}}); err != nil {
				t.Fatalf("A1 report: %v", err)
			}
			rvaDump(t, h, "run-x", "after A1 late report")
			open, _ := h.leases.ListOpenByRun(ctx, "run-x")
			t.Logf("open leases while A2 still runs: %d (want 2)", len(open))

			// A2 ends: $4 on alice, $6 on bob.
			h.now = h.now.Add(time.Hour)
			if err := h.broker.ReportAttempt(ctx, "run-x", P2, Outcome{CostUSD: 10, ByFingerprint: []FingerprintSpend{
				{Fingerprint: fpA, CostUSD: 4}, {Fingerprint: fpB, CostUSD: 6}}}); err != nil {
				t.Fatalf("A2 report: %v", err)
			}
			rvaDump(t, h, "run-x", "after A2 report")
			gotA, gotB := rvaSpent(t, h, pa, day), rvaSpent(t, h, pb, day)
			t.Logf("ledger: alice=$%.2f (want 5)  bob=$%.2f (want 8)", gotA, gotB)
			if len(open) != 2 {
				t.Errorf("A1's late report closed %d of A2's open leases while A2 still runs", 2-len(open))
			}
			if gotA != 5 || gotB != 8 {
				t.Errorf("donor ledgers wrong: alice=$%.2f want $5, bob=$%.2f want $8", gotA, gotB)
			}
		})
	}
}

// R1b: same window, the resume moved the anthropic slot to ANOTHER entry
// (alice cooling): the superseded sibling of A1 (alice's) is never charged.
func TestSideBySideReport_R1b_supersededSiblingOfALateReportIsNeverCharged(t *testing.T) {
	for _, takeover := range []bool{true} {
		t.Run(map[bool]string{true: "post-3d0878e19", false: "pre-commit"}[takeover], func(t *testing.T) { rvaR1b(t, takeover) })
	}
}

func rvaR1b(t *testing.T, takeover bool) {
	ctx := context.Background()
	h := newHarness(t)
	h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 100}, func(p *Pledge) { p.Priority = 0 })
	h.donorKey(t, "carol", "anthropic", Limits{MaxUSDPerDay: 100}, func(p *Pledge) { p.Priority = 1 })
	h.donorKey(t, "bob", "openai", Limits{MaxUSDPerDay: 100})
	pa := PledgeID("alice", SourceAPIKey, "anthropic")
	pc := PledgeID("carol", SourceAPIKey, "anthropic")
	pb := PledgeID("bob", SourceAPIKey, "openai")
	fpA := secrets.FingerprintSHA256("sk-real-alice")
	fpB := secrets.FingerprintSHA256("sk-real-bob")
	day := h.now
	req := func(prov string, side bool) Request {
		r := rvaKeyReq("run-y", prov, side)
		r.RankMode = StrategyPriority
		return r
	}
	a1, err := h.broker.Acquire(ctx, req("anthropic", false))
	if err != nil || a1.PledgeID != pa {
		t.Fatalf("A1 anthropic slot: %+v %v", a1, err)
	}
	if _, err := h.broker.Acquire(ctx, req("openai", true)); err != nil {
		t.Fatal(err)
	}
	P1 := store.PublishAt(h.now.Add(50*time.Millisecond), a1.AcquiredAt)

	// alice cools down; the resume's walk (post-commit) takes over.
	h.now = h.now.Add(2 * time.Minute)
	p, _ := h.pledges.Get(ctx, pa)
	until := h.now.Add(time.Hour)
	p.CooldownUntil = &until
	_ = h.pledges.Upsert(ctx, p)
	c2, err := h.broker.Acquire(ctx, req("anthropic", !takeover))
	if err != nil || c2.PledgeID != pc {
		t.Fatalf("A2 anthropic slot: %+v %v", c2, err)
	}
	if _, err := h.broker.Acquire(ctx, req("openai", true)); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	if err := h.broker.ReportAttempt(ctx, "run-y", P1, Outcome{CostUSD: 3, ByFingerprint: []FingerprintSpend{
		{Fingerprint: fpA, CostUSD: 1}, {Fingerprint: fpB, CostUSD: 2}}}); err != nil {
		t.Fatalf("A1 report: %v", err)
	}
	rvaDump(t, h, "run-y", "after A1 late report")
	t.Logf("ledger after A1's report: alice=$%.2f (want 1)  carol=$%.2f (want 0)  bob=$%.2f (want 2)",
		rvaSpent(t, h, pa, day), rvaSpent(t, h, pc, day), rvaSpent(t, h, pb, day))
	if got := rvaSpent(t, h, pa, day); got != 1 {
		t.Errorf("alice's A1 spend: ledger $%.2f, want $1 — her superseded lease was never stamped", got)
	}
}

// R2: a sealed slot and a LATER sibling the walk refused to seal (released,
// not_launched — the gateway grant without its URL): the attempt's report
// picks the released lease (newest before the publication) and returns
// without charging or closing the sealed one.
func TestSideBySideReport_R2_aReleasedLastSiblingSwallowsTheAttemptsReport(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 100})
	h.donorKey(t, "bob", "openai", Limits{MaxUSDPerDay: 100})
	pa := PledgeID("alice", SourceAPIKey, "anthropic")
	day := h.now
	a, err := h.broker.Acquire(ctx, rvaKeyReq("run-z", "anthropic", false))
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.broker.Acquire(ctx, rvaKeyReq("run-z", "openai", true))
	if err != nil {
		t.Fatal(err)
	}
	h.broker.ReleaseGrant(ctx, b) // the walk refused to seal it
	// publishInstant follows res.grant (the FIRST sealed grant) only.
	P := store.PublishAt(h.now.Add(50*time.Millisecond), a.AcquiredAt)
	h.now = h.now.Add(time.Hour)
	if err := h.broker.ReportAttempt(ctx, "run-z", P, Outcome{CostUSD: 5}); err != nil {
		t.Fatalf("report: %v", err)
	}
	rvaDump(t, h, "run-z", "after the attempt's report")
	open, _ := h.leases.ListOpenByRun(ctx, "run-z")
	t.Logf("open leases after the run's final report: %d (want 0); alice ledger=$%.2f (want 5)", len(open), rvaSpent(t, h, pa, day))
	live, committed, _ := h.leases.LiveCommitment(ctx, pa, "other-run", h.now)
	t.Logf("alice's live commitment seen by OTHER runs: runs=%d committed=$%.2f", live, committed)
	if len(open) != 0 || rvaSpent(t, h, pa, day) != 5 {
		t.Errorf("the sealed lease leaked: open=%d, alice charged $%.2f of $5", len(open), rvaSpent(t, h, pa, day))
	}
}

// R3: an INTERIM report (runner drain / lost heartbeat: JetStream redelivers
// the SAME bundle onto the SAME leases) of a side-by-side attempt closes the
// SIBLING leases ("ok") — the redelivered delivery then runs on a sibling
// credential with no open lease, and its final report never charges it.
func TestSideBySideReport_R3_anInterimReportClosesTheSiblings(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.donorKey(t, "alice", "anthropic", Limits{MaxUSDPerDay: 100, MaxConcurrentRuns: 1})
	h.donorKey(t, "bob", "openai", Limits{MaxUSDPerDay: 100, MaxConcurrentRuns: 1})
	pa := PledgeID("alice", SourceAPIKey, "anthropic")
	pb := PledgeID("bob", SourceAPIKey, "openai")
	fpA := secrets.FingerprintSHA256("sk-real-alice")
	fpB := secrets.FingerprintSHA256("sk-real-bob")
	day := h.now
	a, err := h.broker.Acquire(ctx, rvaKeyReq("run-i", "anthropic", false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.broker.Acquire(ctx, rvaKeyReq("run-i", "openai", true)); err != nil {
		t.Fatal(err)
	}
	P := store.PublishAt(h.now.Add(50*time.Millisecond), a.AcquiredAt)
	h.now = h.now.Add(10 * time.Minute)
	// Delivery 1 is drained mid-run: interim report, the queue redelivers.
	if err := h.broker.ReportAttempt(ctx, "run-i", P, Outcome{CostUSD: 3, Interim: true, ByFingerprint: []FingerprintSpend{
		{Fingerprint: fpA, CostUSD: 1}, {Fingerprint: fpB, CostUSD: 2}}}); err != nil {
		t.Fatal(err)
	}
	rvaDump(t, h, "run-i", "after the INTERIM report")
	open, _ := h.leases.ListOpenByRun(ctx, "run-i")
	// While delivery 2 runs on the same bundle, another run asks bob's key.
	other, oerr := h.broker.Acquire(ctx, rvaKeyReq("run-other", "anthropic", false))
	t.Logf("open leases during the redelivery: %d (want 2); another run admitted on alice 1-slot key (closed sibling): %v (err=%v)", len(open), other != nil, oerr)
	// Delivery 2 finishes: final report, same publication.
	h.now = h.now.Add(10 * time.Minute)
	if err := h.broker.ReportAttempt(ctx, "run-i", P, Outcome{CostUSD: 7, ByFingerprint: []FingerprintSpend{
		{Fingerprint: fpA, CostUSD: 3}, {Fingerprint: fpB, CostUSD: 4}}}); err != nil {
		t.Fatal(err)
	}
	rvaDump(t, h, "run-i", "after the FINAL report")
	t.Logf("ledger: alice=$%.2f (want 4)  bob=$%.2f (want 6)", rvaSpent(t, h, pa, day), rvaSpent(t, h, pb, day))
	if len(open) != 2 || rvaSpent(t, h, pa, day) != 4 || rvaSpent(t, h, pb, day) != 6 {
		t.Errorf("the interim report closed a sibling: open=%d during redelivery", len(open))
	}
}

// A lease returned unused (closed not_launched) is never the report's
// target, even without an attempt id to relate the leases: the report
// must land on the lease that served, or it settles nothing and the
// serving lease holds the donor's slot until its TTL.
func TestSideBySideReport_aReturnedLeaseIsNeverTheTarget(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithAttachments(t)
	h.donorKey(t, "alice", "anthropic", Limits{})
	h.donorKey(t, "bob", "openai", Limits{})
	served, err := h.broker.Acquire(ctx, Request{RunID: "r1", OrgID: testOrg, TenantID: "team-1", UserID: "requester",
		BotID: "docs-refresh", Wants: []Credential{{Source: SourceAPIKey, Ref: "anthropic"}}})
	if err != nil {
		t.Fatalf("acquire served: %v", err)
	}
	h.now = h.now.Add(time.Second)
	returned, err := h.broker.Acquire(ctx, Request{RunID: "r1", OrgID: testOrg, TenantID: "team-1", UserID: "requester",
		BotID: "docs-refresh", Wants: []Credential{{Source: SourceAPIKey, Ref: "openai"}}, SideBySide: true})
	if err != nil {
		t.Fatalf("acquire returned: %v", err)
	}
	h.broker.ReleaseGrant(ctx, returned)
	pub := h.now.Add(time.Second)
	if err := h.broker.ReportAttempt(ctx, "r1", pub, Outcome{CostUSD: 3}); err != nil {
		t.Fatalf("report: %v", err)
	}
	l, err := h.leases.Get(ctx, leaseIDOf(t, h, "r1", served.PledgeID))
	if err != nil {
		t.Fatal(err)
	}
	if !l.Closed || l.CostUSD != 3 {
		t.Fatalf("the serving lease = closed %v cost %.2f, want closed with the $3 report — the report landed on the returned lease", l.Closed, l.CostUSD)
	}
}

func leaseIDOf(t *testing.T, h *harness, runID, pledgeID string) string {
	t.Helper()
	all, err := h.leases.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range all {
		if l.PledgeID == pledgeID {
			return l.ID
		}
	}
	t.Fatalf("no lease of %s on %s", pledgeID, runID)
	return ""
}
