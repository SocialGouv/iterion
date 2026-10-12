package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// The webhook PATCH distinguishes an ABSENT hold/allowlist (keep) from a
// non-nil EMPTY one (clear/widen) — a deliberate gesture that must survive a
// Mongo round-trip as EXPLICIT-empty ([]), not collapse into never-set:
// omitempty on the bson tag drops an empty slice on write, and a read-back
// nil is indistinguishable from "never set", so the provision-time adopt
// would resurrect the cleared value from the integration. These fields
// therefore carry NO omitempty on their bson tags (the JSON tags keep it —
// the API shape is unchanged).
//
// The third field's failure mode differs only in the consequence: a cleared
// AuthorAllowlist reading back nil is still allow-all to every reader (the
// provider handlers' MatchAuthor gate and author_trust — nil and [] are the
// same empty set), so nothing resurrects a narrowing — but the
// explicit-empty property is what the store contract owes, and the round-trip
// proof lives in TestConfigStoreAuthorAllowlistClearRoundTrips below.
//
// This test is pure reflection — the Mongo proofs are the two round-trip
// tests further down, which is why its name does not say Mongo.
//
// Mutation that reddens this test: re-adding omitempty to any bson tag,
// or renaming either key of any field below.
func TestConfigHoldAndAllowlistTagsStayPinned(t *testing.T) {
	// ONE table owns the contract: the bson key must be exact (a renamed
	// key passes a write/read-through-the-same-struct round-trip while
	// orphaning every production document's field), omitempty must be OFF
	// the bson tag, and the json tag must be exact WITH omitempty (the API
	// shape — the studio type reads these keys).
	for _, c := range []struct {
		field, wantBson, wantJSON string
	}{
		{"HoldLabels", "hold_labels", "hold_labels,omitempty"},
		{"LabelAllowlist", "label_allowlist", "label_allowlist,omitempty"},
		{"AuthorAllowlist", "author_allowlist", "author_allowlist,omitempty"},
	} {
		f, ok := reflect.TypeOf(Config{}).FieldByName(c.field)
		if !ok {
			t.Fatalf("Config.%s is gone — the adopt in pkg/forge depends on it", c.field)
		}
		if f.Tag.Get("bson") != c.wantBson {
			t.Errorf("Config.%s bson tag is %q, want %q — a renamed key orphans every stored document's field; omitempty on it collapses a deliberate [] into never-set", c.field, f.Tag.Get("bson"), c.wantBson)
		}
		if f.Tag.Get("json") != c.wantJSON {
			t.Errorf("Config.%s json tag is %q, want %q — the API shape must stay as it was", c.field, f.Tag.Get("json"), c.wantJSON)
		}
	}
}

// onConfigStoreTwins runs one config-store contract against both twins.
// The Mongo half skips without ITERION_TEST_MONGO_URI (the CI
// mongo-conformance job supplies it).
func onConfigStoreTwins(t *testing.T, run func(*testing.T, ConfigStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, NewMemoryConfigStore()) })

	t.Run("mongo", func(t *testing.T) {
		uri := os.Getenv("ITERION_TEST_MONGO_URI")
		if uri == "" {
			t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo config suite")
		}
		ctx, cancel := mongotest.TeardownCtx()
		defer cancel()
		client, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err != nil {
			t.Fatalf("mongo connect: %v", err)
		}
		nonce := make([]byte, 4)
		_, _ = rand.Read(nonce)
		db := client.Database("iterion_webhooks_" + hex.EncodeToString(nonce))
		t.Cleanup(func() {
			drop, dropCancel := mongotest.TeardownCtx()
			defer dropCancel()
			_ = db.Drop(drop)
			_ = client.Disconnect(drop)
		})
		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		run(t, NewMongoStores(db).Configs)
	})
}

// A deliberate clear of AuthorAllowlist — a non-nil EMPTY slice through the
// webhook PATCH (pkg/server/webhooks_routes.go distinguishes absent = keep
// from [] = clear) — must survive the store: MongoConfigStore.Update is a
// full-document replace, so the bson omitempty #2175 removed dropped the
// empty slice on write and the read-back nil was indistinguishable from
// never-set. The read-back must be EXPLICIT-empty, not nil.
func TestConfigStoreAuthorAllowlistClearRoundTrips(t *testing.T) {
	onConfigStoreTwins(t, func(t *testing.T, st ConfigStore) {
		ctx := context.Background()
		cfg := Config{ID: "wh-author-clear", TenantID: "t1", TokenHash: "hash-clear", AuthorAllowlist: []string{"dependabot[bot]"}}
		if err := st.Create(ctx, cfg); err != nil {
			t.Fatalf("Create %s: %v", cfg.ID, err)
		}
		cfg.AuthorAllowlist = []string{}
		if err := st.Update(ctx, cfg); err != nil {
			t.Fatalf("Update (the clear): %v", err)
		}
		got, err := st.Get(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.AuthorAllowlist == nil || len(got.AuthorAllowlist) != 0 {
			t.Fatalf("the cleared AuthorAllowlist read back %#v — the deliberate [] collapsed into never-set (did bson omitempty come back?)", got.AuthorAllowlist)
		}
	})
}

// Zero-value guard-rail: never-set (nil) and cleared ([]) must stay
// indistinguishable to every READER. The zero-value semantics are allow-all
// either way — MatchAuthor's empty-allowlist path, which the provider
// handlers (webhooks_github/webhooks_gitlab) and author_trust all go through.
// Dropping the bson omitempty makes [] DURABLE; it must never make nil-vs-[]
// a meaningful distinction downstream.
func TestConfigStoreAuthorAllowlistZeroValueIsAllowAll(t *testing.T) {
	onConfigStoreTwins(t, func(t *testing.T, st ConfigStore) {
		ctx := context.Background()
		// Distinct token hashes: token_hash carries a non-sparse UNIQUE
		// index, so two fixtures sharing the zero value collide on the
		// second Create — the contract would die before its assertions.
		neverSet := Config{ID: "wh-author-never", TenantID: "t1", TokenHash: "hash-never"}
		if err := st.Create(ctx, neverSet); err != nil {
			t.Fatalf("Create %s: %v", neverSet.ID, err)
		}
		gotNever, err := st.Get(ctx, neverSet.ID)
		if err != nil {
			t.Fatalf("Get (never-set): %v", err)
		}
		if len(gotNever.AuthorAllowlist) != 0 {
			t.Fatalf("never-set AuthorAllowlist read back %#v, want empty", gotNever.AuthorAllowlist)
		}
		if !MatchAuthor(gotNever.AuthorAllowlist, "a-human-login") {
			t.Fatal("a never-set AuthorAllowlist must read as allow-all")
		}

		cleared := Config{ID: "wh-author-cleared", TenantID: "t1", TokenHash: "hash-cleared", AuthorAllowlist: []string{"dependabot[bot]"}}
		if err := st.Create(ctx, cleared); err != nil {
			t.Fatalf("Create %s: %v", cleared.ID, err)
		}
		cleared.AuthorAllowlist = []string{}
		if err := st.Update(ctx, cleared); err != nil {
			t.Fatalf("Update (the clear): %v", err)
		}
		gotCleared, err := st.Get(ctx, cleared.ID)
		if err != nil {
			t.Fatalf("Get (cleared): %v", err)
		}
		if len(gotCleared.AuthorAllowlist) != 0 {
			t.Fatalf("cleared AuthorAllowlist read back %#v, want empty", gotCleared.AuthorAllowlist)
		}
		if !MatchAuthor(gotCleared.AuthorAllowlist, "a-human-login") {
			t.Fatal("a cleared AuthorAllowlist must read as allow-all, exactly like never-set")
		}
	})
}
