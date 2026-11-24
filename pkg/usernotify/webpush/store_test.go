package webpush

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// runDeleteByUserSuite pins the erasure's reach into the push store on
// both twins: every subscription of the account goes, in every tenant it
// enrolled from, a neighbour's stays, and a second sweep removes nothing.
func runDeleteByUserSuite(t *testing.T, s SubscriptionStore) {
	ctx := context.Background()
	for _, sub := range []*Subscription{
		{TenantID: "ta", UserID: "erased", Endpoint: "https://push.example/a", P256dh: "k", Auth: "a"},
		{TenantID: "tb", UserID: "erased", Endpoint: "https://push.example/b", P256dh: "k", Auth: "a"},
		{TenantID: "ta", UserID: "neighbour", Endpoint: "https://push.example/c", P256dh: "k", Auth: "a"},
	} {
		if err := s.Upsert(ctx, sub); err != nil {
			t.Fatalf("seed %s: %v", sub.Endpoint, err)
		}
	}
	n, err := s.DeleteByUser(ctx, "erased")
	if err != nil || n != 2 {
		t.Fatalf("DeleteByUser = %d (%v), want 2", n, err)
	}
	if left, err := s.ListForUsers(ctx, []string{"erased"}); err != nil || len(left) != 0 {
		t.Fatalf("subscriptions of the erased account left: %d (%v)", len(left), err)
	}
	if kept, err := s.ListForUsers(ctx, []string{"neighbour"}); err != nil || len(kept) != 1 {
		t.Fatalf("the neighbour's subscriptions: %d (%v), want 1", len(kept), err)
	}
	if n, err := s.DeleteByUser(ctx, "erased"); err != nil || n != 0 {
		t.Fatalf("a second sweep = %d (%v), want 0", n, err)
	}
	if _, err := s.DeleteByUser(ctx, ""); err == nil {
		t.Fatal("a sweep naming no account answered no error")
	}
}

func TestMemSubscriptionStore_DeleteByUser(t *testing.T) {
	runDeleteByUserSuite(t, NewMemSubscriptionStore())
}

// TestMongoSubscriptionStore_DeleteByUser runs the suite against a real
// Mongo, gated on ITERION_TEST_MONGO_URI like the sibling store suites.
func TestMongoSubscriptionStore_DeleteByUser(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo push subscription suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_webpush_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	st := NewMongoSubscriptionStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	runDeleteByUserSuite(t, st)
}
