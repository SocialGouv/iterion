package storekit

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// pinRow is the smallest pin-bearing row: the two pin maps and the version
// fence, with the tags the webhooks config and the trigger subscription use.
type pinRow struct {
	ID      string            `bson:"_id"`
	Keys    map[string]string `bson:"key_overrides,omitempty"`
	Secrets map[string]string `bson:"secret_overrides,omitempty"`
	Version int64             `bson:"version"`
}

var errPinRowNotFound = errors.New("pin row: not found")

// The rewrite arm exists for legacy pin names — a '.', a '$' or an empty
// name, stored before the routes' name guard — and those live on rows
// written before the version field existed too (webhook configs gained it
// on 2026-10-27). Such a row decodes as version 0 and stores none, so a CAS
// on the bare 0 never matches it: the clear exhausts its attempts and every
// delete's sweep of that row fails, forever. The fence reads 0 as "0 or
// absent", as the stores' full-document Update always has.
func TestMongoPinClear_RewritesALegacyNameOnARowThatPredatesTheVersion(t *testing.T) {
	db := testDB(t)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()

	coll := db.Collection("pin_rows")
	kit := NewMongo[pinRow](coll, errPinRowNotFound, "pin row")
	pinClear := MongoPinClear[pinRow]{
		Coll:    coll,
		Get:     func(ctx context.Context, id string) (pinRow, error) { return kit.GetByID(ctx, id, "get") },
		Version: func(r pinRow) int64 { return r.Version },
		Fields: []PinField[pinRow]{
			{Path: "key_overrides", Of: func(r pinRow) map[string]string { return r.Keys }},
			{Path: "secret_overrides", Of: func(r pinRow) map[string]string { return r.Secrets }},
		},
		Op: "pin row: clear",
	}

	// No "version" field at all: the document an earlier release wrote.
	if _, err := coll.InsertOne(ctx, bson.M{
		"_id":              "legacy",
		"key_overrides":    bson.M{"anthropic": "key-alive"},
		"secret_overrides": bson.M{"dead.name": "rec-dead", "forge_token": "rec-dead", "live": "rec-alive"},
	}); err != nil {
		t.Fatal(err)
	}

	cleared, err := pinClear.Clear(ctx, "legacy", "rec-dead")
	if err != nil {
		t.Fatalf("clearing a row that predates the version field = %v — a CAS on the bare 0 never matches it", err)
	}
	if !cleared {
		t.Fatal("the clear reported nothing removed — the legacy-named dead pin still stands")
	}
	got, err := pinClear.Get(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dead.name", "forge_token"} {
		if _, standing := got.Secrets[name]; standing {
			t.Fatalf("pin %q naming the dead record survives: %v", name, got.Secrets)
		}
	}
	if got.Secrets["live"] != "rec-alive" || got.Keys["anthropic"] != "key-alive" {
		t.Fatalf("the clear moved pins naming another record: %v / %v", got.Keys, got.Secrets)
	}
	if got.Version == 0 {
		t.Fatal("the clear removed pins without moving the version — a pre-sweep snapshot would write them back")
	}

	// An absent row answers the store's own not-found, not a silent no-op.
	if cleared, err := pinClear.Clear(ctx, "absent", "rec-dead"); !errors.Is(err, errPinRowNotFound) || cleared {
		t.Fatalf("clearing an absent row = (%v, %v), want (false, the store's not-found)", cleared, err)
	}
}

// The fence itself, without a store: a read version of 0 also matches a
// document with no version field; any other version matches only itself.
func TestVersionCASReadsZeroAsZeroOrAbsent(t *testing.T) {
	zero := VersionCAS("id", 0)
	if _, bare := zero["version"]; bare {
		t.Fatalf("VersionCAS(0) = %v filters on the bare version — a row that predates the field never matches", zero)
	}
	or, ok := zero["$or"].(bson.A)
	if !ok || len(or) != 2 {
		t.Fatalf("VersionCAS(0) = %v, want an $or of version 0 and an absent version", zero)
	}
	if zero["_id"] != "id" {
		t.Fatalf("VersionCAS(0) = %v, want the row's id", zero)
	}
	seven := VersionCAS("id", 7)
	if seven["version"] != int64(7) || seven["_id"] != "id" || seven["$or"] != nil {
		t.Fatalf("VersionCAS(7) = %v, want exactly {_id: id, version: 7}", seven)
	}
}
