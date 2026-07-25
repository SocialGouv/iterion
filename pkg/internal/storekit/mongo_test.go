package storekit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type rec struct {
	ID     string    `bson:"_id"`
	Tenant string    `bson:"tenant_id"`
	Name   string    `bson:"name"`
	At     time.Time `bson:"at"`
}

// testDB connects to the gated live Mongo (same gating as the
// pkg/store/mongo conformance harness) and hands back a throwaway
// database dropped on cleanup.
func testDB(t *testing.T) *mongo.Database {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo storekit suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_storekit_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	return db
}

func TestMongo_CRUD(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	notFound := errors.New("kit: not found")
	dup := errors.New("kit: duplicate")
	kit := NewMongo[rec](db.Collection("recs"), notFound, "kit")
	base := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	if err := kit.Insert(ctx, rec{ID: "a", Tenant: "t1", Name: "one", At: base}, dup, "insert"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := kit.Insert(ctx, rec{ID: "a"}, dup, "insert"); !errors.Is(err, dup) {
		t.Fatalf("duplicate Insert err = %v, want dup sentinel", err)
	}
	if err := kit.Insert(ctx, rec{ID: "a"}, nil, "insert"); err == nil || errors.Is(err, dup) {
		t.Fatalf("nil-dup Insert err = %v, want wrapped raw error", err)
	}
	for _, r := range []rec{
		{ID: "b", Tenant: "t1", Name: "two", At: base.Add(time.Minute)},
		{ID: "c", Tenant: "t2", Name: "three", At: base.Add(2 * time.Minute)},
	} {
		if err := kit.Insert(ctx, r, dup, "insert"); err != nil {
			t.Fatalf("Insert %s: %v", r.ID, err)
		}
	}

	t.Run("get + findone", func(t *testing.T) {
		got, err := kit.GetByID(ctx, "a", "get")
		if err != nil || got.Name != "one" {
			t.Fatalf("GetByID = %+v, %v", got, err)
		}
		if _, err := kit.GetByID(ctx, "ghost", "get"); !errors.Is(err, notFound) {
			t.Fatalf("GetByID(ghost) err = %v, want notFound", err)
		}
		byName, err := kit.FindOne(ctx, bson.M{"name": "two"}, "get by name")
		if err != nil || byName.ID != "b" {
			t.Fatalf("FindOne = %+v, %v", byName, err)
		}
		if _, err := kit.FindOne(ctx, bson.M{"name": "nope"}, "get by name"); !errors.Is(err, notFound) {
			t.Fatalf("FindOne miss err = %v, want notFound", err)
		}
	})

	t.Run("list with options", func(t *testing.T) {
		out, err := kit.List(ctx, bson.M{"tenant_id": "t1"}, "list", "decode",
			options.Find().SetSort(bson.M{"at": -1}))
		if err != nil || len(out) != 2 || out[0].ID != "b" || out[1].ID != "a" {
			t.Fatalf("List desc = %+v, %v", out, err)
		}
		limited, err := kit.List(ctx, bson.M{"tenant_id": "t1"}, "list", "decode",
			options.Find().SetSort(bson.M{"at": -1}).SetSkip(1).SetLimit(1))
		if err != nil || len(limited) != 1 || limited[0].ID != "a" {
			t.Fatalf("List skip+limit = %+v, %v", limited, err)
		}
		if empty, err := kit.List(ctx, bson.M{"tenant_id": "none"}, "list", "decode"); err != nil || len(empty) != 0 {
			t.Fatalf("empty List = %+v, %v", empty, err)
		}
	})

	t.Run("replace + set", func(t *testing.T) {
		if err := kit.Replace(ctx, "a", rec{ID: "a", Tenant: "t1", Name: "uno", At: base}, nil, "update"); err != nil {
			t.Fatalf("Replace: %v", err)
		}
		if err := kit.Replace(ctx, "ghost", rec{ID: "ghost"}, nil, "update"); !errors.Is(err, notFound) {
			t.Fatalf("Replace(ghost) err = %v, want notFound", err)
		}
		if err := kit.Set(ctx, "a", bson.M{"name": "ein"}, "set"); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if err := kit.Set(ctx, "ghost", bson.M{"name": "x"}, "set"); !errors.Is(err, notFound) {
			t.Fatalf("Set(ghost) err = %v, want notFound", err)
		}
		if err := kit.SetAny(ctx, "ghost", bson.M{"name": "x"}, "stamp"); err != nil {
			t.Fatalf("SetAny(ghost) must be a silent no-op, got %v", err)
		}
		got, _ := kit.GetByID(ctx, "a", "get")
		if got.Name != "ein" {
			t.Fatalf("post-Set = %+v", got)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := kit.Delete(ctx, "c", "delete"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := kit.Delete(ctx, "c", "delete"); !errors.Is(err, notFound) {
			t.Fatalf("second Delete err = %v, want notFound", err)
		}
		if err := kit.DeleteWhere(ctx, bson.M{"tenant_id": "t1"}, "delete where"); err != nil {
			t.Fatalf("DeleteWhere: %v", err)
		}
		if left, _ := kit.List(ctx, bson.M{}, "list", "decode"); len(left) != 0 {
			t.Fatalf("post-DeleteWhere leftovers: %+v", left)
		}
		// Deleting nothing is not an error.
		if err := kit.DeleteWhere(ctx, bson.M{"tenant_id": "t1"}, "delete where"); err != nil {
			t.Fatalf("empty DeleteWhere: %v", err)
		}
	})
}

func TestTicketMongo(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	notFound := errors.New("kit: ticket not found")
	kit := NewTicketMongo[payload](db.Collection("tickets"), "result")
	if err := kit.EnsureSchema(ctx, "kit: ensure indexes"); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := kit.EnsureSchema(ctx, "kit: ensure indexes"); err != nil {
		t.Fatalf("EnsureSchema (second): %v", err)
	}

	ticket, err := kit.Mint(ctx, payload{V: "hello"}, time.Minute, "kit: mint")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	t.Run("wire format keeps the configured payload field", func(t *testing.T) {
		raw, err := db.Collection("tickets").FindOne(ctx, bson.M{"_id": ticket}).Raw()
		if err != nil {
			t.Fatalf("raw find: %v", err)
		}
		if _, lookupErr := raw.LookupErr("result"); lookupErr != nil {
			t.Fatalf("payload not stored under %q: %v (doc %v)", "result", lookupErr, raw)
		}
		if _, lookupErr := raw.LookupErr("expires_at"); lookupErr != nil {
			t.Fatalf("expires_at missing: %v", lookupErr)
		}
	})

	t.Run("single use", func(t *testing.T) {
		got, err := kit.Redeem(ctx, ticket, notFound, "kit: redeem")
		if err != nil || got.V != "hello" {
			t.Fatalf("Redeem = %+v, %v", got, err)
		}
		if _, err := kit.Redeem(ctx, ticket, notFound, "kit: redeem"); !errors.Is(err, notFound) {
			t.Fatalf("second Redeem err = %v, want notFound", err)
		}
		if _, err := kit.Redeem(ctx, "nope", notFound, "kit: redeem"); !errors.Is(err, notFound) {
			t.Fatalf("unknown Redeem err = %v, want notFound", err)
		}
	})

	t.Run("expired is consumed and refused", func(t *testing.T) {
		expired, err := kit.Mint(ctx, payload{V: "old"}, -time.Second, "kit: mint")
		if err != nil {
			t.Fatalf("Mint expired: %v", err)
		}
		if _, err := kit.Redeem(ctx, expired, notFound, "kit: redeem"); !errors.Is(err, notFound) {
			t.Fatalf("expired Redeem err = %v, want notFound", err)
		}
		// Consumed: the row is gone even though redemption was refused.
		if n, err := db.Collection("tickets").CountDocuments(ctx, bson.M{"_id": expired}); err != nil || n != 0 {
			t.Fatalf("expired ticket row still present (n=%d, err=%v)", n, err)
		}
	})
}
