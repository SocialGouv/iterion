package desktopsso

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// TestMongoStore runs the ticket flow against a real Mongo (same gating
// as the pkg/store/mongo conformance harness), pinning the wire format:
// docs keyed by the ticket's SHA-256 with the deferred login under "pending"
// and an absolute "expires_at" — and the witness #2635 asks for: no stored
// document carries a credential, and the ticket itself is nowhere in it.
func TestMongoStore(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo desktopsso suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_desktopsso_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := mongotest.TeardownCtx()
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

	d := auth.DesktopLogin{
		UserID:          "u1",
		Root:            auth.SessionRoot{ID: "lineage-1", Generation: 2},
		PreferredTeamID: "team-1",
	}
	ticket, err := store.Mint(ctx, d)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// The wire format, asserted on the raw document: keyed by the ticket's
	// SHA-256, never the ticket; the payload is the deferred login and
	// nothing credential-shaped.
	raw, err := db.Collection(TicketsCollectionName).FindOne(ctx, bson.M{"_id": ticketKey(ticket)}).Raw()
	if err != nil {
		t.Fatalf("read the stored doc by the ticket's hash: %v", err)
	}
	if _, err := db.Collection(TicketsCollectionName).FindOne(ctx, bson.M{"_id": ticket}).Raw(); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatalf("a doc keyed by the ticket itself exists (%v) — the ticket must never be stored", err)
	}
	keys := map[string]bool{}
	elements, err := raw.Elements()
	if err != nil {
		t.Fatalf("walk the stored doc: %v", err)
	}
	for _, e := range elements {
		keys[e.Key()] = true
	}
	for _, want := range []string{"_id", "pending", "expires_at"} {
		if !keys[want] {
			t.Errorf("the stored doc has no %q (keys: %v)", want, keys)
		}
	}
	for k := range keys {
		if strings.Contains(k, "token") || strings.Contains(k, "secret") || k == "result" {
			t.Errorf("the stored doc carries %q — no credential field may rest here", k)
		}
	}
	if rawJSON, _ := json.Marshal(keys); strings.Contains(string(rawJSON), ticket) {
		t.Errorf("the stored doc names the ticket: %s", rawJSON)
	}
	if v, ok := raw.Lookup("expires_at").TimeOK(); !ok || v.Before(time.Now()) {
		t.Errorf("expires_at = %v (ok=%v), want a future absolute time", v, ok)
	}

	got, err := store.Redeem(ctx, ticket)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got.UserID != "u1" || got.Root.ID != "lineage-1" || got.Root.Generation != 2 || got.PreferredTeamID != "team-1" {
		t.Errorf("redeemed login mismatch: %+v", got)
	}
	if _, err := store.Redeem(ctx, ticket); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("second redeem err = %v, want ErrTicketNotFound", err)
	}

	// Expired rows are refused (and consumed) even before the TTL reaper runs.
	expired := ticketDoc{ID: ticketKey("some-ticket"), Pending: d, ExpiresAt: time.Now().Add(-time.Second)}
	if _, err := db.Collection(TicketsCollectionName).InsertOne(ctx, expired); err != nil {
		t.Fatalf("insert expired doc: %v", err)
	}
	if _, err := store.Redeem(ctx, "some-ticket"); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("expired redeem err = %v, want ErrTicketNotFound", err)
	}

	// A doc in the PRE-#2635 shape (the ticket in plaintext as _id, a whole
	// LoginResult with its refresh token as "result") answers no redemption:
	// its _id is no hash a redeem computes, and the TTL reaper takes it.
	legacy := struct {
		Ticket    string           `bson:"_id"`
		Result    auth.LoginResult `bson:"result"`
		ExpiresAt time.Time        `bson:"expires_at"`
	}{Ticket: "legacy-plaintext-ticket", Result: auth.LoginResult{RefreshToken: "ref-legacy"}, ExpiresAt: time.Now().Add(time.Minute)}
	if _, err := db.Collection(TicketsCollectionName).InsertOne(ctx, legacy); err != nil {
		t.Fatalf("insert legacy doc: %v", err)
	}
	if _, err := store.Redeem(ctx, "legacy-plaintext-ticket"); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("legacy-shape redeem err = %v, want ErrTicketNotFound", err)
	}
}
