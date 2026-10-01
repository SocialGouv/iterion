package credpool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
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
	T_A := h.now
	if _, err := h.broker.Acquire(ctx, h.request(runID)); err != nil {
		t.Fatalf("attempt A acquire: %v", err)
	}
	L := leaseOfRun(t, h, runID)
	if L == nil {
		t.Fatal("attempt A holds no lease")
	}
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
