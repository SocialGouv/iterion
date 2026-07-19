package wsticket

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

	"github.com/SocialGouv/iterion/pkg/auth"
)

// TestMongoStore runs the ticket flow against a real Mongo (same gating
// as the pkg/store/mongo conformance harness), pinning the wire format:
// docs keyed by ticket with the Identity under "identity" and an
// absolute "expires_at".
func TestMongoStore(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo wsticket suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_wsticket_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})

	store := NewMongoStore(db, time.Minute)
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema (second): %v", err)
	}

	id := auth.Identity{UserID: "u1", Email: "a@b.io", TeamID: "team-1", IsSuperAdmin: true}
	ticket, err := store.Mint(ctx, id)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := store.Redeem(ctx, ticket)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got.UserID != "u1" || got.TeamID != "team-1" || !got.IsSuperAdmin {
		t.Errorf("redeemed identity mismatch: %+v", got)
	}
	if _, err := store.Redeem(ctx, ticket); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("second redeem err = %v, want ErrTicketNotFound", err)
	}

	// A doc in the at-rest shape (payload under "identity") written by
	// any prior replica must redeem — the migration-safety contract for a
	// rolling deploy.
	legacy := struct {
		Ticket    string        `bson:"_id"`
		Identity  auth.Identity `bson:"identity"`
		ExpiresAt time.Time     `bson:"expires_at"`
	}{Ticket: "legacy-1", Identity: id, ExpiresAt: time.Now().Add(time.Minute)}
	if _, err := db.Collection(TicketsCollectionName).InsertOne(ctx, legacy); err != nil {
		t.Fatalf("insert legacy doc: %v", err)
	}
	got2, err := store.Redeem(ctx, "legacy-1")
	if err != nil || got2.Email != "a@b.io" {
		t.Fatalf("legacy redeem = %+v, %v", got2, err)
	}

	// Expired rows are refused (and consumed) even before the TTL reaper runs.
	expired := legacy
	expired.Ticket = "legacy-expired"
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := db.Collection(TicketsCollectionName).InsertOne(ctx, expired); err != nil {
		t.Fatalf("insert expired doc: %v", err)
	}
	if _, err := store.Redeem(ctx, "legacy-expired"); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("expired redeem err = %v, want ErrTicketNotFound", err)
	}
}
