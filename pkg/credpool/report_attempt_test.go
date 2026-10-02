package credpool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// leaseOfRun returns the run's currently-open lease, or nil.
func leaseOfRun(t *testing.T, h *harness, runID string) *Lease {
	t.Helper()
	l, err := h.leases.GetOpenByRun(context.Background(), runID)
	if err != nil {
		return nil
	}
	return &l
}

// supersedeWindow seeds the supersede window the probes read: attempt A
// acquires and publishes at T_A; an operator resume acquires inside A's
// unwind (superseding A's lease, its own grant returned for the caller to
// release or keep) and holds the run's open lease. Returns T_A, A's lease,
// the resume's lease and the resume's grant.
func supersedeWindow(t *testing.T, h *harness, runID string) (time.Time, Lease, Lease, *Grant) {
	t.Helper()
	ctx := context.Background()
	// The delivery's publication is stamped by store.PublishAt: never
	// inside its own lease's acquisition millisecond.
	T_A := store.PublishAt(h.now, h.now)
	if _, err := h.broker.Acquire(ctx, h.request(runID)); err != nil {
		t.Fatalf("attempt A acquire: %v", err)
	}
	L := leaseOfRun(t, h, runID)
	if L == nil {
		t.Fatal("attempt A holds no lease")
	}
	// The resume happens later: the clock moves, the supersede and the
	// successor's acquisition land in a later millisecond than A's.
	h.now = h.now.Add(2 * time.Millisecond)
	G, err := h.broker.Acquire(ctx, h.request(runID))
	if err != nil {
		t.Fatalf("resume acquire: %v", err)
	}
	L2 := leaseOfRun(t, h, runID)
	if L2 == nil || L2.ID == L.ID {
		t.Fatalf("the resume's lease: %+v (A's was %s)", L2, L.ID)
	}
	return T_A, *L, *L2, G
}

// TestReportAttempt_aLateTeardownReportStaysOnItsOwnLease: an attempt's
// teardown can take minutes (the bank, the upload). Inside that window an
// operator resume acquires — superseding the attempt's lease — and the late
// report must land on the attempt's own (superseded) lease, charged once
// through the stamp, never on the successor's open lease.
func TestReportAttempt_aLateTeardownReportStaysOnItsOwnLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	T_A, L, L2, _ := supersedeWindow(t, h, "run-1")

	if err := h.broker.ReportAttempt(ctx, "run-1", T_A, Outcome{CostUSD: 3}); err != nil {
		t.Fatalf("attempt A report: %v", err)
	}
	stamped, err := h.leases.Get(ctx, L.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stamped.Closed || stamped.Outcome != OutcomeSupersededReported || stamped.CostUSD != 3 {
		t.Fatalf("A's lease after its report: %+v, want closed %q with $3", stamped, OutcomeSupersededReported)
	}
	open := leaseOfRun(t, h, "run-1")
	if open == nil || open.ID != L2.ID {
		t.Fatalf("the successor lease is not open for the successor: %+v", open)
	}

	// A redelivered report of the same attempt loses the stamp's CAS: the
	// donor is charged once.
	if err := h.broker.ReportAttempt(ctx, "run-1", T_A, Outcome{CostUSD: 3}); err != nil {
		t.Fatalf("attempt A report again: %v", err)
	}
	stamped, err = h.leases.Get(ctx, L.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stamped.CostUSD != 3 {
		t.Fatalf("A's lease carries $%.2f after a redelivered report, want $3", stamped.CostUSD)
	}

	// The successor reports its own spend through its own lease.
	if err := h.broker.Report(ctx, "run-1", Outcome{CostUSD: 2}); err != nil {
		t.Fatalf("successor report: %v", err)
	}
	day, _, err := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if err != nil {
		t.Fatal(err)
	}
	if day.CostUSD != 5 {
		t.Fatalf("the donor's ledger records $%.2f, want 3+2", day.CostUSD)
	}
}

// TestReportAttempt_anInterimReportStaysOnItsOwnLease: an interim report of
// an attempt superseded mid-unwind charges the attempt's own lease (its
// audit trail), and leaves the successor's open.
func TestReportAttempt_anInterimReportStaysOnItsOwnLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	T_A, L, L2, _ := supersedeWindow(t, h, "run-1")

	if err := h.broker.ReportAttempt(ctx, "run-1", T_A, Outcome{CostUSD: 1.5, Interim: true}); err != nil {
		t.Fatalf("attempt A interim report: %v", err)
	}
	stamped, err := h.leases.Get(ctx, L.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stamped.Closed || stamped.Outcome != OutcomeSupersededReported || stamped.CostUSD != 1.5 {
		t.Fatalf("A's lease after its interim report: %+v, want closed %q with $1.50", stamped, OutcomeSupersededReported)
	}
	if open := leaseOfRun(t, h, "run-1"); open == nil || open.ID != L2.ID {
		t.Fatalf("the successor lease is not open for the successor: %+v", open)
	}
}

// TestReportAttempt_aRefusedResumeCannotReopenAReportedLease: the stamp
// takes a lease out of Reopen's CAS, so a refused resume's rollback — which
// reopens the lease it superseded — cannot reopen a lease whose only
// reporter already reported through the stamp. (A gate of the shape "do not
// reopen while another lease of the run is open" would NOT hold here: it
// reads the run's present tense, and there are windows in which no lease is
// open — the successor's own report can land inside the same unwind — where
// that gate would reopen exactly this lease.)
func TestReportAttempt_aRefusedResumeCannotReopenAReportedLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	T_A, L, L2, G := supersedeWindow(t, h, "run-1")

	// A's report lands inside the window, carrying T_A: it stamps A's lease.
	if err := h.broker.ReportAttempt(ctx, "run-1", T_A, Outcome{CostUSD: 3}); err != nil {
		t.Fatalf("attempt A report: %v", err)
	}

	// The refused resume's rollback, in SubmitResume's order.
	h.broker.ReleaseGrant(ctx, G)
	h.broker.ReopenSuperseded(ctx, G)

	if open := leaseOfRun(t, h, "run-1"); open != nil {
		t.Fatalf("%s is open after the rollback with no reporter coming (A's was %s, the resume's %s)", open.ID, L.ID, L2.ID)
	}
	day, _, err := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if err != nil {
		t.Fatal(err)
	}
	if day.CostUSD != 3 {
		t.Fatalf("the donor's ledger records $%.2f, want A's 3", day.CostUSD)
	}
}

// TestReportAttempt_anUnreadablePublicationReportsTheOpenLease: a delivery
// older than the attempt identity — no readable publication — reports the
// way a Report did, against the open lease.
func TestReportAttempt_anUnreadablePublicationReportsTheOpenLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	L := leaseOfRun(t, h, "run-1")
	if err := h.broker.ReportAttempt(ctx, "run-1", time.Time{}, Outcome{CostUSD: 2}); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, err := h.leases.Get(ctx, L.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Closed || got.Outcome != "ok" || got.CostUSD != 2 {
		t.Fatalf("the lease after the report: %+v, want closed ok with $2", got)
	}
}

// testLeaseStampAndList is the LeaseStore contract of ListByRun and
// StampSupersededReport, shared by both stores.
func testLeaseStampAndList(t *testing.T, ctx context.Context, s LeaseStore) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Millisecond)
	put := func(id string, acquired time.Time) {
		t.Helper()
		if err := s.Put(ctx, Lease{ID: id, RunID: "run-1", PledgeID: "p", AcquiredAt: acquired, ExpiresAt: acquired.Add(time.Hour)}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	put("newer", at)
	put("older", at.Add(-time.Minute))

	all, err := s.ListByRun(ctx, "run-1")
	if err != nil || len(all) != 2 || all[0].ID != "newer" || all[1].ID != "older" {
		t.Fatalf("ListByRun = %+v %v, want newest acquired first", all, err)
	}

	// An open lease is not stamped; the superseded close stamps once.
	if won, err := s.StampSupersededReport(ctx, "newer", 1, at); err != nil || won {
		t.Fatalf("stamp of an open lease = (%t, %v), want (false, nil)", won, err)
	}
	if won, err := s.Close(ctx, "older", 0, OutcomeSuperseded, at); err != nil || !won {
		t.Fatalf("supersede = (%t, %v), want (true, nil)", won, err)
	}
	if won, err := s.StampSupersededReport(ctx, "older", 3, at); err != nil || !won {
		t.Fatalf("stamp = (%t, %v), want (true, nil)", won, err)
	}
	if won, err := s.StampSupersededReport(ctx, "older", 3, at); err != nil || won {
		t.Fatalf("second stamp = (%t, %v), want (false, nil)", won, err)
	}
	l, err := s.Get(ctx, "older")
	if err != nil {
		t.Fatal(err)
	}
	if l.Outcome != OutcomeSupersededReported || l.CostUSD != 3 || !l.ClosedAt.Equal(at) {
		t.Fatalf("stamped lease = %+v, want %q with $3 at the stamp's instant", l, OutcomeSupersededReported)
	}
	// A stamped lease is no longer a supersede: Reopen misses it.
	if won, err := s.Reopen(ctx, "older", at); err != nil || won {
		t.Fatalf("reopen of a stamped lease = (%t, %v), want (false, nil)", won, err)
	}
	if _, err := s.StampSupersededReport(ctx, "lease-absent", 1, at); err != ErrNotFound {
		t.Fatalf("stamp of an absent lease = %v, want ErrNotFound", err)
	}
}

func TestLeaseStampAndList_Memory(t *testing.T) {
	testLeaseStampAndList(t, context.Background(), NewMemoryLeaseStore())
}

func TestLeaseStampAndList_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo lease store")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_credpool_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	testLeaseStampAndList(t, ctx, NewMongoLeaseStore(db))
}

// TestReportAttempt_aGrantlessTakeoverReportsNothingAtTheProductionOrder:
// the production order is PUBLISH first, supersede after — the takeover's
// delivery carries a published_at stamped before the grantless close, and
// that exact publication is what the close carries back: the takeover's
// own report (and its interim reports) is a no-op, while the SUPERSEDED
// attempt's report stamps through it once.
func TestReportAttempt_aGrantlessTakeoverReportsNothingAtTheProductionOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})

	T_A := store.PublishAt(h.now, h.now)
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("attempt A acquire: %v", err)
	}
	L := leaseOfRun(t, h, "run-1")

	// The grantless takeover, in the production order: its delivery is
	// published (this is the report's identity, stable across
	// redeliveries and replays), and only THEN is the run's lease
	// superseded — strictly after the publication, the way the publisher
	// orders it — carrying that publication back on the close.
	h.now = h.now.Add(5 * time.Millisecond)
	T_B := store.PublishAt(h.now, h.now)
	h.now = h.now.Add(time.Millisecond)
	if err := h.broker.SupersedeRun(ctx, "run-1", "team-1", T_B); err != nil {
		t.Fatalf("SupersedeRun: %v", err)
	}
	if open := leaseOfRun(t, h, "run-1"); open != nil {
		t.Fatalf("after SupersedeRun a lease is open: %+v", open)
	}

	if closed, err := h.leases.Get(ctx, L.ID); err != nil || closed.Outcome != OutcomeSupersededGrantless || closed.SupersededByPublishedAt == nil || !closed.SupersededByPublishedAt.Truncate(time.Millisecond).Equal(T_B.Truncate(time.Millisecond)) {
		t.Fatalf("the grantless close = (%+v, %v), want %q carrying the takeover's publication %v", closed, err, OutcomeSupersededGrantless, T_B)
	}

	// The takeover's attempt spends and reports — an auth failure, whose
	// condition must not reach the donor either: they never served it.
	h.now = h.now.Add(5 * time.Millisecond)
	if err := h.broker.ReportAttempt(ctx, "run-1", T_B, Outcome{CostUSD: 4, Condition: ConditionAuthFailed}); err != nil {
		t.Fatalf("the takeover's report: %v", err)
	}
	day, _, err := h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if err != nil {
		t.Fatal(err)
	}
	if day.CostUSD != 0 {
		t.Fatalf("the donor was charged $%.2f for an attempt that held no lease", day.CostUSD)
	}
	pledge, err := h.pledges.Get(ctx, PledgeID("alice", SourceOAuth, "claude_code"))
	if err != nil {
		t.Fatal(err)
	}
	if pledge.ConsecutiveAuthFailures != 0 {
		t.Fatalf("the takeover's auth failure reached the donor (%d consecutive failures): two of those evict them", pledge.ConsecutiveAuthFailures)
	}
	still, err := h.leases.Get(ctx, L.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.Outcome != OutcomeSupersededGrantless || still.CostUSD != 0 {
		t.Fatalf("A's lease after the takeover = outcome %q cost %.2f, want the grantless supersede the takeover did not charge", still.Outcome, still.CostUSD)
	}

	// The takeover's interim reports are its own, and no-op the same way.
	if err := h.broker.ReportAttempt(ctx, "run-1", T_B, Outcome{CostUSD: 1, Interim: true}); err != nil {
		t.Fatalf("the takeover's interim report: %v", err)
	}
	still, err = h.leases.Get(ctx, L.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.CostUSD != 0 {
		t.Fatalf("the takeover's interim report charged the lease $%.2f", still.CostUSD)
	}

	// A's own report, published before the supersede: stamps through.
	if err := h.broker.ReportAttempt(ctx, "run-1", T_A, Outcome{CostUSD: 3}); err != nil {
		t.Fatalf("attempt A report: %v", err)
	}
	stamped, err := h.leases.Get(ctx, L.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stamped.Closed || stamped.Outcome != OutcomeSupersededReported || stamped.CostUSD != 3 {
		t.Fatalf("A's lease after its report: closed=%v outcome=%q cost=%.2f closedAt=%v, want closed %q with $3", stamped.Closed, stamped.Outcome, stamped.CostUSD, stamped.ClosedAt, OutcomeSupersededReported)
	}
	day, _, err = h.ledger.Usage(ctx, PledgeID("alice", SourceOAuth, "claude_code"), h.now)
	if err != nil {
		t.Fatal(err)
	}
	if day.CostUSD != 3 {
		t.Fatalf("the donor's ledger records $%.2f, want A's 3", day.CostUSD)
	}
}

// TestAcquire_aRunLeasesAreStrictlyOrdered: on a fixed clock, a second
// acquisition of the same run still stamps its lease in a LATER millisecond
// than the first — the acquisition floor — so the report's pin never has to
// guess between them.
func TestAcquire_aRunLeasesAreStrictlyOrdered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	// A fixed clock: a double resume inside one millisecond — real on a
	// store that keeps milliseconds. The acquisition floor is what keeps
	// the two leases apart.
	h.now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	firstLease, err := h.leases.GetOpenByRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = firstLease
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	all, err := h.leases.ListByRun(ctx, "run-1")
	if err != nil || len(all) != 2 {
		t.Fatalf("ListByRun = (%d, %v), want 2", len(all), err)
	}
	if !all[0].AcquiredAt.After(all[1].AcquiredAt) {
		t.Fatalf("two leases of one run share the acquisition millisecond (%v, %v): the report's pin cannot tell its own lease from a successor's", all[0].AcquiredAt, all[1].AcquiredAt)
	}
}

// testLeaseTieAndPrecision is the store contract that keeps the pin
// deterministic and honest about precision, shared by both stores: a tie
// (two leases of one run inside one millisecond, rows written before the
// acquisition floor) is ordered by lease id, descending; a lease recorded
// inside the publication's own millisecond is the attempt's — a memory
// store that keeps nanoseconds and a Mongo store that keeps milliseconds
// must read it the same way — and a lease recorded in a later millisecond
// is a successor's.
func testLeaseTieAndPrecision(t *testing.T, ctx context.Context, s LeaseStore) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Millisecond)
	put := func(id string, acquired time.Time) {
		t.Helper()
		if err := s.Put(ctx, Lease{ID: id, RunID: "run-tie", PledgeID: "p", AcquiredAt: acquired, ExpiresAt: acquired.Add(time.Hour)}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	// A tie: two acquisitions inside one millisecond (rows written before
	// the acquisition floor). The order is fixed, and the pin — reported at
	// a later millisecond, when both are eligible — takes the id-greater
	// lease, every time.
	put("aaa", at)
	put("zzz", at)
	if won, err := s.Close(ctx, "aaa", 0, OutcomeSuperseded, at.Add(time.Hour)); err != nil || !won {
		t.Fatalf("close aaa: %v %v", won, err)
	}
	if won, err := s.Close(ctx, "zzz", 0, OutcomeSuperseded, at.Add(time.Hour)); err != nil || !won {
		t.Fatalf("close zzz: %v %v", won, err)
	}
	for range 10 {
		all, err := s.ListByRun(ctx, "run-tie")
		if err != nil || len(all) != 2 || all[0].ID != "zzz" || all[1].ID != "aaa" {
			t.Fatalf("ListByRun = (%+v, %v), want zzz before aaa, every time", all, err)
		}
		picked, err := attemptLease(ctx, s, "run-tie", at.Add(time.Hour))
		if err != nil || picked == nil || picked.ID != "zzz" {
			t.Fatalf("the pin in a tie = (%+v, %v), want zzz, every time", picked, err)
		}
	}

	// A publication inside the attempt's own lease's acquisition
	// millisecond — what the publication floor (store.PublishAt) exists to
	// prevent — hands the report to the predecessor, identically in both
	// stores.
	put("pred", at.Add(-time.Millisecond))
	put("own", at)
	if won, err := s.Close(ctx, "pred", 0, OutcomeSuperseded, at.Add(time.Hour)); err != nil || !won {
		t.Fatalf("close pred: %v %v", won, err)
	}
	if won, err := s.Close(ctx, "own", 0, OutcomeSuperseded, at.Add(time.Hour)); err != nil || !won {
		t.Fatalf("close own: %v %v", won, err)
	}
	if picked, err := attemptLease(ctx, s, "run-tie", at); err != nil || picked == nil || picked.ID != "pred" {
		t.Fatalf("a publication inside its lease's millisecond = (%+v, %v), want the predecessor, identically in both stores", picked, err)
	}

	// Precision, the reviewer's own numbers: a predecessor at .099 and a
	// successor at .100 of the same second, a publication at .1005 — the
	// pin takes the predecessor, identically in a store that keeps
	// nanoseconds and one that keeps milliseconds. The successor's own
	// publication, a millisecond later, takes the successor.
	runID := "run-precision"
	put2 := func(id string, acquired time.Time) {
		t.Helper()
		if err := s.Put(ctx, Lease{ID: id, RunID: runID, PledgeID: "p", AcquiredAt: acquired, ExpiresAt: acquired.Add(time.Hour)}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	put2("predecessor", at.Add(-time.Millisecond))
	put2("successor", at)
	picked, err := attemptLease(ctx, s, runID, at.Add(500*time.Microsecond))
	if err != nil || picked == nil || picked.ID != "predecessor" {
		t.Fatalf("the pin at .1005 = (%+v, %v), want the predecessor at .099", picked, err)
	}
	if picked, err = attemptLease(ctx, s, runID, at.Add(time.Millisecond)); err != nil || picked == nil || picked.ID != "successor" {
		t.Fatalf("the pin at .101 = (%+v, %v), want the successor at .100", picked, err)
	}
}

func TestLeaseTieAndPrecision_Memory(t *testing.T) {
	testLeaseTieAndPrecision(t, context.Background(), NewMemoryLeaseStore())
}

func TestLeaseTieAndPrecision_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo lease store")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_credpool_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	testLeaseTieAndPrecision(t, ctx, NewMongoLeaseStore(db))

	// The reviewer's own numbers, on the store that keeps ONLY
	// milliseconds: a predecessor at .099, a successor at .100, a
	// publication at .1005 — the pin takes the predecessor. Comparing at
	// the lease's millisecond is what makes a store that keeps
	// nanoseconds read the same facts the same way.
	pin := NewMongoLeaseStore(client.Database(db.Name() + "_pin"))
	pat := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, l := range []struct {
		id string
		at time.Time
	}{{"predecessor", pat.Add(-time.Millisecond)}, {"successor", pat}} {
		if err := pin.Put(ctx, Lease{ID: l.id, RunID: "run-pin", PledgeID: "p", AcquiredAt: l.at, ExpiresAt: l.at.Add(time.Hour)}); err != nil {
			t.Fatalf("put %s: %v", l.id, err)
		}
	}
	picked, err := attemptLease(ctx, pin, "run-pin", pat.Add(500*time.Microsecond))
	if err != nil || picked == nil || picked.ID != "predecessor" {
		t.Fatalf("the pin at .1005 = (%+v, %v), want the predecessor at .099 — the two stores must read these facts the same way", picked, err)
	}
	if picked, err = attemptLease(ctx, pin, "run-pin", pat.Add(time.Millisecond)); err != nil || picked == nil || picked.ID != "successor" {
		t.Fatalf("the pin at .101 = (%+v, %v), want the successor at .100", picked, err)
	}
}

// listByRunBlips is a store whose ListByRun fails — a Mongo blip on the
// pin's one read.
type listByRunBlips struct{ LeaseStore }

func (s listByRunBlips) ListByRun(context.Context, string) ([]Lease, error) {
	return nil, errors.New("mongo blip")
}

// TestReportAttempt_aStoreBlipIsPropagated: the pin's read failing must
// reach the caller (the runner logs a WARN) — a report that dies quietly
// would drop the donor's charge with no trace.
func TestReportAttempt_aStoreBlipIsPropagated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// The blip starts after the lease exists: the acquisition floor's read
	// failing fails the acquisition (propagated); the report's read failing
	// must propagate the same way.
	h.broker.leases = listByRunBlips{h.leases}
	err := h.broker.ReportAttempt(ctx, "run-1", time.Now().UTC(), Outcome{CostUSD: 2})
	if err == nil || !strings.Contains(err.Error(), "mongo blip") {
		t.Fatalf("ReportAttempt = %v, want the store's error — a silent report drops the donor's charge", err)
	}
}

// TestAcquire_theFloorReadErrorIsPropagated: the acquisition floor's read
// failing must fail the acquisition — a floor silently skipped hands the
// pin a tie the next millisecond would have prevented.
func TestAcquire_theFloorReadErrorIsPropagated(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10})
	h.broker.leases = listByRunBlips{h.leases}
	// The acquisition FAILS rather than proceeding on an un-floored lease
	// (the pledge's skip carries the floor read's error, logged), so the
	// pin is never handed a tie a retried acquisition would have prevented.
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err == nil {
		t.Fatal("an acquisition whose floor read failed proceeded quietly: the pin is handed a tie the retry would have prevented")
	}
	if _, err := h.leases.GetOpenByRun(ctx, "run-1"); err == nil {
		t.Fatal("a lease was created on a floor read that failed")
	}
}

// floorBlipWrap toggles the acquisition floor's read so a test can fail it
// and heal it.
type floorBlipWrap struct {
	LeaseStore
	pass bool
}

func (s *floorBlipWrap) ListByRun(ctx context.Context, runID string) ([]Lease, error) {
	if !s.pass {
		return nil, errors.New("floor blip")
	}
	return s.LeaseStore.ListByRun(ctx, runID)
}

// TestAcquire_aFloorBlipGivesTheReservedUnitBack: a non-readmitting
// acquisition whose floor read fails fails loudly — and gives the donor's
// reserved daily unit back, or the same-day next acquisition is denied by
// the leaked unit.
func TestAcquire_aFloorBlipGivesTheReservedUnitBack(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.donor(t, "alice", Limits{MaxUSDPerDay: 10, MaxRunsPerDay: 1})

	blip := &floorBlipWrap{LeaseStore: h.leases}
	h.broker.leases = blip
	if _, err := h.broker.Acquire(ctx, h.request("run-1")); err == nil {
		t.Fatal("premise: the acquisition should fail on the floor blip")
	}
	blip.pass = true
	// Healed: the same day, the donor's only run unit.
	if _, err := h.broker.Acquire(ctx, h.request("run-2")); err != nil {
		t.Fatalf("the donor's daily unit was not given back after the failed acquisition (%v) — the floor read's error return skips release()", err)
	}
}
