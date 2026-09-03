package secrets

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
)

// The memory store replaces the whole record on Upsert, so a cleared label
// clears in every in-memory test whatever the bson tags say. The Mongo
// store writes through $set, where an omitted key keeps the OLD value —
// the shape of bug only a real Mongo can show. Same gating as the other
// conformance suites (CI's mongo-conformance job sets ITERION_TEST_MONGO_URI).
func mongoOAuthStore(t *testing.T) (*MongoOAuthStore, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo oauth suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_oauth_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	s := NewMongoOAuthStore(db)
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return s, ctx
}

// Clearing the label must clear it on the wire the production store uses:
// with a bson omitempty on the field, the $set body carried no key for an
// empty label and the API reported a clear that never happened.
func TestMongoOAuth_EmptyLabelClearsThroughUpsert(t *testing.T) {
	s, ctx := mongoOAuthStore(t)
	rec := OAuthRecord{UserID: "alice", Kind: OAuthKindClaudeCode, SealedPayload: []byte("sealed"), Fingerprint: "fp-1", AccountLabel: "old name"}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec.AccountLabel = ""
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "" {
		t.Fatalf("label after an empty-label upsert = %q, want cleared (bson omitempty would keep the stale name)", got.AccountLabel)
	}
}

// A rename touches two keys and nothing else: the sealed payload and the
// fingerprint stay whatever the last connect/refresh wrote, even when the
// caller's copy of the record is stale.
func TestMongoOAuth_SetAccountLabelIsMetadataOnly(t *testing.T) {
	s, ctx := mongoOAuthStore(t)
	if err := s.SetAccountLabel(ctx, "alice", OAuthKindCodex, "x"); !errors.Is(err, ErrOAuthNotFound) {
		t.Fatalf("SetAccountLabel on a missing record = %v, want ErrOAuthNotFound", err)
	}
	if err := s.Upsert(ctx, OAuthRecord{UserID: "alice", Kind: OAuthKindCodex, SealedPayload: []byte("sealed-v1"), Fingerprint: "fp-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A refresh lands a new payload before the rename is written.
	if err := s.Upsert(ctx, OAuthRecord{UserID: "alice", Kind: OAuthKindCodex, SealedPayload: []byte("sealed-v2"), Fingerprint: "fp-1"}); err != nil {
		t.Fatalf("refresh upsert: %v", err)
	}
	if err := s.SetAccountLabel(ctx, "alice", OAuthKindCodex, "alice@openai"); err != nil {
		t.Fatalf("SetAccountLabel: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindCodex)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "alice@openai" {
		t.Fatalf("label = %q", got.AccountLabel)
	}
	if string(got.SealedPayload) != "sealed-v2" || got.Fingerprint != "fp-1" {
		t.Fatalf("rename disturbed the credential: payload=%q fp=%q", got.SealedPayload, got.Fingerprint)
	}
	if err := s.SetAccountLabel(ctx, "alice", OAuthKindCodex, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ = s.Get(ctx, "alice", OAuthKindCodex); got.AccountLabel != "" {
		t.Fatalf("label after clear = %q, want empty", got.AccountLabel)
	}
}
