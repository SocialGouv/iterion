package credpool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// testLeaseReopen is the LeaseStore contract of Reopen: a superseded close —
// which charged nothing — is undone at exactly its instant; a lease reported
// since, closed another way, or superseded at another instant is not.
func testLeaseReopen(t *testing.T, ctx context.Context, s LeaseStore) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Millisecond)
	put := func(id string) {
		t.Helper()
		if err := s.Put(ctx, Lease{ID: id, RunID: "run-" + id, PledgeID: "p", AcquiredAt: at.Add(-time.Minute), ExpiresAt: at.Add(time.Hour)}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	closeAs := func(id string, cost float64, outcome string, when time.Time) {
		t.Helper()
		if won, err := s.Close(ctx, id, cost, outcome, when); err != nil || !won {
			t.Fatalf("close %s: %v %v", id, won, err)
		}
	}

	put("superseded")
	if err := s.AddCost(ctx, "superseded", 1); err != nil {
		t.Fatal(err)
	}
	closeAs("superseded", 0, OutcomeSuperseded, at)
	if ok, err := s.Reopen(ctx, "superseded", at); err != nil || !ok {
		t.Fatalf("reopen a superseded lease at its instant = (%v, %v), want (true, nil)", ok, err)
	}
	l, err := s.Get(ctx, "superseded")
	if err != nil {
		t.Fatal(err)
	}
	if l.Closed || l.Outcome != "" || l.ClosedAt != nil || l.CostUSD != 1 {
		t.Fatalf("reopened lease = %+v, want open again with its interim $1 and no outcome", l)
	}
	if open, err := s.GetOpenByRun(ctx, "run-superseded"); err != nil || open.ID != "superseded" {
		t.Fatalf("the reopened lease is not the run's open lease: %+v %v", open, err)
	}
	// Its attempt reports through the re-armed CAS, once.
	closeAs("superseded", 2, "ok", at.Add(time.Second))
	if won, err := s.Close(ctx, "superseded", 2, "ok", at.Add(2*time.Second)); err != nil || won {
		t.Fatalf("a redelivered report after the reopen = (%v, %v), want it to lose the CAS", won, err)
	}

	// A lease reported since its supersede was undone is never reopened.
	if ok, err := s.Reopen(ctx, "superseded", at); err != nil || ok {
		t.Fatalf("reopen a REPORTED lease = (%v, %v), want (false, nil): it would erase the charge and re-arm the report's CAS", ok, err)
	}
	// Superseded at another instant: another acquisition's close.
	put("other-instant")
	closeAs("other-instant", 0, OutcomeSuperseded, at)
	if ok, err := s.Reopen(ctx, "other-instant", at.Add(time.Millisecond)); err != nil || ok {
		t.Fatalf("reopen at another instant = (%v, %v), want (false, nil)", ok, err)
	}
	// Closed any other way.
	put("released")
	closeAs("released", 0, OutcomeNotLaunched, at)
	if ok, err := s.Reopen(ctx, "released", at); err != nil || ok {
		t.Fatalf("reopen a released lease = (%v, %v), want (false, nil)", ok, err)
	}
	// An open lease has nothing to undo.
	put("open")
	if ok, err := s.Reopen(ctx, "open", at); err != nil || ok {
		t.Fatalf("reopen an open lease = (%v, %v), want (false, nil)", ok, err)
	}
	if _, err := s.Reopen(ctx, "missing", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reopen a missing lease: %v, want ErrNotFound", err)
	}
}

// testAcquireUndoesItsSupersedeWithoutAGrant: an acquisition that grants
// nothing — no enabled pool here — took over nothing, so the open lease of
// the run's previous attempt it superseded is open again when it returns.
// The broker's clock has sub-millisecond precision: the supersede is stamped
// to the millisecond the store keeps, or the undo would miss it.
func testAcquireUndoesItsSupersedeWithoutAGrant(t *testing.T, ctx context.Context, leases LeaseStore) {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	b := NewBroker(BrokerConfig{
		Pools: NewMemoryPoolStore(), Pledges: NewMemoryPledgeStore(), Leases: leases, Ledger: NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), Sealer: sealer, Logger: iterlog.New(iterlog.LevelError, nil),
		Now: func() time.Time { return time.Date(2026, 10, 1, 9, 0, 0, 123456789, time.UTC) },
	})
	if err := leases.Put(ctx, Lease{ID: "previous", RunID: "run-1", PledgeID: "p", TenantID: "team-1",
		AcquiredAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if g, err := b.Acquire(ctx, Request{RunID: "run-1", OrgID: "org-1", TenantID: "team-1", UserID: "u", Wants: []Credential{{Source: SourceOAuth, Ref: "claude_code"}}}); err == nil {
		t.Fatalf("an acquisition with no pool granted %+v", g)
	}
	if l, err := leases.GetOpenByRun(ctx, "run-1"); err != nil || l.ID != "previous" {
		t.Fatalf("after an acquisition that granted nothing, the previous attempt's lease is not open (%+v, %v): its redelivery reports against nothing", l, err)
	}
}

func TestLeaseReopen_Memory(t *testing.T) {
	testLeaseReopen(t, context.Background(), NewMemoryLeaseStore())
	testAcquireUndoesItsSupersedeWithoutAGrant(t, context.Background(), NewMemoryLeaseStore())
}

func TestLeaseReopen_Mongo(t *testing.T) {
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
	testLeaseReopen(t, ctx, NewMongoLeaseStore(db))
	t.Run("an acquisition without a grant", func(t *testing.T) {
		testAcquireUndoesItsSupersedeWithoutAGrant(t, ctx, NewMongoLeaseStore(client.Database(db.Name()+"_acq")))
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = client.Database(db.Name() + "_acq").Drop(drop)
	})
}
