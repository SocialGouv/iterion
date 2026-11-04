package configshare

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

// runScrubActorSuite pins what an account's erasure asks of the delivery
// trail on both twins: the IP and user agent of the account's own edits go,
// in every tenant; the rest of the row stays; a share token's edits and
// another account's keep theirs; twice is once.
func runScrubActorSuite(t *testing.T, ctx context.Context, s Store) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Millisecond)
	gone := ActorUser("gone")
	for _, d := range []Delivery{
		{ID: "d1", ShareID: "sh1", TenantID: "ta", At: at, SourceIP: "10.0.0.1", UserAgent: "ua-1", Method: "PATCH", Actor: gone, Status: 200, AfterSHA: "abc"},
		{ID: "d2", ShareID: "sh2", TenantID: "tb", At: at, SourceIP: "10.0.0.2", UserAgent: "ua-2", Method: "PATCH", Actor: gone, Status: 200},
		{ID: "d3", ShareID: "sh1", TenantID: "ta", At: at, SourceIP: "10.0.0.3", UserAgent: "ua-3", Method: "PATCH", Actor: "share:sh1", Status: 200},
		{ID: "d4", ShareID: "sh1", TenantID: "ta", At: at, SourceIP: "10.0.0.4", UserAgent: "ua-4", Method: "PATCH", Actor: ActorUser("stays"), Status: 200},
	} {
		if err := s.RecordDelivery(ctx, &d); err != nil {
			t.Fatalf("record %s: %v", d.ID, err)
		}
	}
	if _, err := s.ScrubActor(ctx, ""); err == nil {
		t.Fatal("a scrub naming no actor answered no error")
	}
	if n, err := s.ScrubActor(ctx, gone); err != nil || n != 2 {
		t.Fatalf("ScrubActor = %d (%v), want 2: one per tenant", n, err)
	}
	rows := map[string]*Delivery{}
	for _, share := range []string{"sh1", "sh2"} {
		ds, err := s.ListDeliveries(ctx, share, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range ds {
			rows[d.ID] = d
		}
	}
	for _, id := range []string{"d1", "d2"} {
		if d := rows[id]; d == nil || d.SourceIP != "" || d.UserAgent != "" || d.Actor != gone {
			t.Fatalf("%s after the scrub: %+v, want no IP nor agent and the pseudonym kept", id, d)
		}
	}
	if d := rows["d1"]; d.AfterSHA != "abc" || d.Method != "PATCH" {
		t.Fatalf("the scrub took more than the identifiers: %+v", d)
	}
	if d := rows["d3"]; d == nil || d.SourceIP != "10.0.0.3" || d.UserAgent != "ua-3" {
		t.Fatalf("a share token's edit was scrubbed: %+v", d)
	}
	if d := rows["d4"]; d == nil || d.SourceIP != "10.0.0.4" {
		t.Fatalf("another account's edit was scrubbed: %+v", d)
	}
	if n, err := s.ScrubActor(ctx, gone); err != nil || n != 0 {
		t.Fatalf("a second scrub = %d (%v), want 0", n, err)
	}
}

func TestMemoryStore_ScrubActor(t *testing.T) {
	runScrubActorSuite(t, context.Background(), NewMemoryStore())
}

// TestMongoStore_ScrubActor is the Mongo leg, gated on ITERION_TEST_MONGO_URI
// like the other conformance suites.
func TestMongoStore_ScrubActor(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo config-share scrub suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_configshare_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	runScrubActorSuite(t, ctx, NewMongoStore(db))
}
