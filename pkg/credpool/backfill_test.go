package credpool

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// TestEnsureSchema_backfillAPIKeyPledgeIDs pins the identity migration: a
// legacy api_key pledge stored under the three-segment id must move to the
// key-carrying id TOGETHER with every reference that points at it — leases
// (closed ones included: they are the lender's history) and ledger rows —
// while a row that merely LOOKS like a child of the old id, and every oauth
// pledge, is left untouched. The pass is idempotent: a second boot changes
// nothing and never merges counters.
func TestEnsureSchema_backfillAPIKeyPledgeIDs(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the api_key pledge identity backfill")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	base := "iterion_credpool_backfill_" + hex.EncodeToString(nonce)
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = client.Disconnect(drop)
	})
	fresh := func(t *testing.T, suffix string) *mongo.Database {
		t.Helper()
		db := client.Database(base + "_" + suffix)
		t.Cleanup(func() {
			drop, dropCancel := mongotest.TeardownCtx()
			defer dropCancel()
			_ = db.Drop(drop)
		})
		return db
	}

	t.Run("a legacy pledge moves with its references", func(t *testing.T) {
		db := fresh(t, "legacy")
		pledges, leases, ledger := db.Collection(colPledges), db.Collection(colLeases), db.Collection(colLedger)
		now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
		const (
			old  = "alice|api_key|anthropic"
			want = "alice|api_key|anthropic|key-1"
		)
		mustInsert := func(col *mongo.Collection, doc bson.M) {
			t.Helper()
			if _, err := col.InsertOne(ctx, doc); err != nil {
				t.Fatalf("seed %v: %v", doc["_id"], err)
			}
		}

		// The legacy pledge document, raw: a field this binary does not know
		// must survive (the pass rewrites the id, never decodes-re-encodes).
		mustInsert(pledges, bson.M{
			"_id": old, "pool_id": "pool-1", "user_id": "alice",
			"source": "api_key", "ref": "anthropic", "key_id": "key-1",
			"enabled": true, "health": "ok", "limits": bson.M{"max_usd_per_day": 5},
			"unknown_field": "must survive",
		})
		// An oauth pledge lives outside the change entirely.
		mustInsert(pledges, bson.M{
			"_id": "alice|oauth|claude_code", "pool_id": "pool-1", "user_id": "alice",
			"source": "oauth", "ref": "claude_code", "enabled": true, "health": "ok",
		})
		// Leases: an open one and a closed one. The closed one is the
		// lender's history and the source of HeldPledge — both move.
		mustInsert(leases, bson.M{
			"_id": "lease-open", "run_id": "run-1", "pledge_id": old, "donor_id": "alice",
			"source": "api_key", "ref": "anthropic", "key_id": "key-1",
			"closed": false, "acquired_at": now.Add(-time.Hour), "expires_at": now.Add(time.Hour),
		})
		mustInsert(leases, bson.M{
			"_id": "lease-closed", "run_id": "run-0", "pledge_id": old, "donor_id": "alice",
			"source": "api_key", "ref": "anthropic", "key_id": "key-1",
			"closed": true, "cost_usd": 1.5, "outcome": "ok",
			"acquired_at": now.Add(-48 * time.Hour), "expires_at": now.Add(-36 * time.Hour),
		})
		// Ledger rows under the inherited id, plus two traps:
		//   - `want|d|2026-11-17` is the CANONICAL pledge's own day row, and
		//     it also reads as `old|key-1|d|…`: the anchor `[dw]` right after
		//     the old id is what keeps it out of the inherited walk (the new
		//     id HAS the old one as prefix);
		//   - a pre-existing row already at the destination must be kept
		//     as-is (never summed — every retry would double-count).
		mustInsert(ledger, bson.M{"_id": old + "|d|2026-11-18", "runs": 3, "cost_usd_millis": 1200, "period_start": now})
		mustInsert(ledger, bson.M{"_id": old + "|w|2026-W47", "runs": 2, "cost_usd_millis": 900, "period_start": now})
		mustInsert(ledger, bson.M{"_id": want + "|d|2026-11-17", "runs": 9, "cost_usd_millis": 4000, "period_start": now})
		mustInsert(ledger, bson.M{"_id": old + "|d|2026-11-16", "runs": 4, "cost_usd_millis": 100, "period_start": now})
		mustInsert(ledger, bson.M{"_id": want + "|d|2026-11-16", "runs": 1, "cost_usd_millis": 10, "period_start": now})

		find := func(col *mongo.Collection, id string) bson.M {
			t.Helper()
			var doc bson.M
			if err := col.FindOne(ctx, bson.M{"_id": id}).Decode(&doc); err != nil {
				t.Fatalf("read %s: %v", id, err)
			}
			return doc
		}
		absent := func(col *mongo.Collection, id string) {
			t.Helper()
			if err := col.FindOne(ctx, bson.M{"_id": id}).Err(); err != mongo.ErrNoDocuments {
				t.Errorf("%s still exists (or read failed: %v), want it gone after the move", id, err)
			}
		}
		// The driver decodes a small integer as int32; a hand-written
		// expectation may be int or int64.
		isInt := func(v any, want int64) bool {
			switch n := v.(type) {
			case int:
				return int64(n) == want
			case int32:
				return int64(n) == want
			case int64:
				return n == want
			}
			return false
		}
		assertMigrated := func(t *testing.T) {
			t.Helper()
			// The pledge moved, every field intact.
			p := find(pledges, want)
			if p["key_id"] != "key-1" || p["user_id"] != "alice" {
				t.Errorf("migrated pledge = %v, want alice's key-1", p)
			}
			if p["unknown_field"] != "must survive" {
				t.Errorf("migrated pledge lost the field it never decoded (%v)", p["unknown_field"])
			}
			absent(pledges, old)
			// The oauth pledge is untouched.
			if o := find(pledges, "alice|oauth|claude_code"); o["ref"] != "claude_code" {
				t.Errorf("the oauth pledge was rewritten: %v", o)
			}
			// Both leases — the closed one included — now name the new id.
			for _, id := range []string{"lease-open", "lease-closed"} {
				if l := find(leases, id); l["pledge_id"] != want {
					t.Errorf("lease %s has pledge_id %v, want %q", id, l["pledge_id"], want)
				}
			}
			// Day and week rows moved under the new prefix, counters intact.
			if d := find(ledger, want+"|d|2026-11-18"); !isInt(d["runs"], 3) {
				t.Errorf("migrated day row runs = %v (%T), want 3", d["runs"], d["runs"])
			}
			if w := find(ledger, want+"|w|2026-W47"); !isInt(w["runs"], 2) {
				t.Errorf("migrated week row runs = %v (%T), want 2", w["runs"], w["runs"])
			}
			absent(ledger, old+"|d|2026-11-18")
			absent(ledger, old+"|w|2026-W47")
			// The canonical row that also reads as a child of the old id
			// survived with its own counters.
			if decoy := find(ledger, want+"|d|2026-11-17"); !isInt(decoy["runs"], 9) {
				t.Errorf("the canonical `old|key-1|d|…` ledger row was rewritten: runs = %v, want 9", decoy["runs"])
			}
			// A destination row that already existed keeps its own counters;
			// the migrated one is dropped, never summed (4+1 would mint spend).
			if c := find(ledger, want+"|d|2026-11-16"); !isInt(c["runs"], 1) {
				t.Errorf("destination row runs = %v, want the existing 1 (counters must never be merged)", c["runs"])
			}
			absent(ledger, old+"|d|2026-11-16")
		}

		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		assertMigrated(t)
		// Idempotent: a second boot changes nothing and doubles nothing.
		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("second EnsureSchema: %v", err)
		}
		assertMigrated(t)
		if n, err := ledger.CountDocuments(ctx, bson.M{}); err != nil || n != 4 {
			t.Errorf("ledger holds %d document(s) (%v), want the 4 seeds and no extra row", n, err)
		}
	})

	t.Run("a legacy pledge of a second key keeps its own references", func(t *testing.T) {
		db := fresh(t, "second-key")
		pledges, leases, ledger := db.Collection(colPledges), db.Collection(colLeases), db.Collection(colLedger)
		now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
		const (
			old  = "alice|api_key|anthropic"
			keyA = "alice|api_key|anthropic|key-1"
			keyB = "alice|api_key|anthropic|key-2"
		)
		// key-1 is already canonical; the legacy document is the SECOND key's.
		// Its lease and ledger rows must follow the document to key-2 — a
		// self-repair pass that only knows "the inherited id belongs to a
		// canonical pledge" would hand them to key-1, so the per-document
		// move is what keeps a two-key lender's history on the right key.
		for _, doc := range []bson.M{
			{"_id": keyA, "pool_id": "pool-1", "user_id": "alice", "source": "api_key", "ref": "anthropic", "key_id": "key-1", "enabled": true, "health": "ok"},
			{"_id": old, "pool_id": "pool-1", "user_id": "alice", "source": "api_key", "ref": "anthropic", "key_id": "key-2", "enabled": true, "health": "ok"},
		} {
			if _, err := pledges.InsertOne(ctx, doc); err != nil {
				t.Fatalf("seed %v: %v", doc["_id"], err)
			}
		}
		if _, err := leases.InsertOne(ctx, bson.M{
			"_id": "lease-key2", "run_id": "run-k2", "pledge_id": old, "donor_id": "alice",
			"source": "api_key", "ref": "anthropic", "key_id": "key-2",
			"closed": true, "acquired_at": now.Add(-time.Hour), "expires_at": now}); err != nil {
			t.Fatalf("seed lease: %v", err)
		}
		// A lease that carries no credential stamp of its own: the DOCUMENT it
		// hangs off is the only thing that says whose it is. The self-repair
		// pass, which attributes by the lease's key, cannot place it — the
		// per-document move must.
		if _, err := leases.InsertOne(ctx, bson.M{
			"_id": "lease-unstamped", "run_id": "run-old", "pledge_id": old, "donor_id": "alice",
			"closed": true, "acquired_at": now.Add(-48 * time.Hour), "expires_at": now.Add(-36 * time.Hour)}); err != nil {
			t.Fatalf("seed un-stamped lease: %v", err)
		}
		if _, err := ledger.InsertOne(ctx, bson.M{
			"_id": old + "|d|2026-11-18", "runs": 5, "period_start": now}); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}

		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		var l bson.M
		if err := leases.FindOne(ctx, bson.M{"_id": "lease-key2"}).Decode(&l); err != nil {
			t.Fatalf("read lease: %v", err)
		}
		if l["pledge_id"] != keyB {
			t.Errorf("key-2's lease has pledge_id %v, want %q", l["pledge_id"], keyB)
		}
		var un bson.M
		if err := leases.FindOne(ctx, bson.M{"_id": "lease-unstamped"}).Decode(&un); err != nil {
			t.Fatalf("read un-stamped lease: %v", err)
		}
		if un["pledge_id"] != keyB {
			t.Errorf("the un-stamped lease has pledge_id %v, want the document's own key %q", un["pledge_id"], keyB)
		}
		if err := ledger.FindOne(ctx, bson.M{"_id": keyB + "|d|2026-11-18"}).Err(); err != nil {
			t.Errorf("key-2's ledger row did not follow its pledge: %v", err)
		}
		if err := ledger.FindOne(ctx, bson.M{"_id": keyA + "|d|2026-11-18"}).Err(); err != mongo.ErrNoDocuments {
			t.Errorf("key-2's ledger row landed on key-1 (%v)", err)
		}
		if err := pledges.FindOne(ctx, bson.M{"_id": old}).Err(); err != mongo.ErrNoDocuments {
			t.Errorf("the legacy document is still there after the walk (%v)", err)
		}
	})

	t.Run("self repair attributes a stray lease by its own key", func(t *testing.T) {
		db := fresh(t, "repair")
		pledges, leases, ledger := db.Collection(colPledges), db.Collection(colLeases), db.Collection(colLedger)
		now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
		const (
			old  = "alice|api_key|anthropic"
			keyA = "alice|api_key|anthropic|key-1"
			keyB = "alice|api_key|anthropic|key-2"
		)
		// Two canonical pledges of one lender — the shape this change exists
		// for — and leases an older replica wrote under the inherited id
		// AFTER the migration ran. Each lease carries its own key id, which
		// is what earns it its owner: a coin flip between the two keys would
		// be a donor charged another key's run.
		for _, doc := range []bson.M{
			{"_id": keyA, "pool_id": "pool-1", "user_id": "alice", "source": "api_key", "ref": "anthropic", "key_id": "key-1", "enabled": true, "health": "ok"},
			{"_id": keyB, "pool_id": "pool-1", "user_id": "alice", "source": "api_key", "ref": "anthropic", "key_id": "key-2", "enabled": true, "health": "ok"},
		} {
			if _, err := pledges.InsertOne(ctx, doc); err != nil {
				t.Fatalf("seed %v: %v", doc["_id"], err)
			}
		}
		for _, doc := range []bson.M{
			{"_id": "stray-b", "run_id": "run-b", "pledge_id": old, "donor_id": "alice", "source": "api_key", "ref": "anthropic", "key_id": "key-2", "closed": false, "acquired_at": now, "expires_at": now.Add(time.Hour)},
			{"_id": "stray-a", "run_id": "run-a", "pledge_id": old, "donor_id": "alice", "source": "api_key", "ref": "anthropic", "key_id": "key-1", "closed": true, "acquired_at": now.Add(-time.Hour), "expires_at": now},
		} {
			if _, err := leases.InsertOne(ctx, doc); err != nil {
				t.Fatalf("seed %v: %v", doc["_id"], err)
			}
		}
		// A ledger row under the inherited id names NO key: with two keys
		// claiming the same (lender, source, ref) it cannot be attributed,
		// so the pass must leave it where it is rather than hand it to a
		// coin flip.
		if _, err := ledger.InsertOne(ctx, bson.M{"_id": old + "|d|2026-11-18", "runs": 2, "period_start": now}); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}

		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		for id, want := range map[string]string{"stray-a": keyA, "stray-b": keyB} {
			var l bson.M
			if err := leases.FindOne(ctx, bson.M{"_id": id}).Decode(&l); err != nil {
				t.Fatalf("read lease %s: %v", id, err)
			}
			if l["pledge_id"] != want {
				t.Errorf("stray lease %s has pledge_id %v, want its own key's %q", id, l["pledge_id"], want)
			}
		}
		if err := ledger.FindOne(ctx, bson.M{"_id": old + "|d|2026-11-18"}).Err(); err != nil {
			t.Errorf("an unattributable ledger row was moved or dropped: %v", err)
		}
		for _, id := range []string{keyA + "|d|2026-11-18", keyB + "|d|2026-11-18"} {
			if err := ledger.FindOne(ctx, bson.M{"_id": id}).Err(); err != mongo.ErrNoDocuments {
				t.Errorf("the unattributable row was handed to %q (%v)", id, err)
			}
		}
	})

	t.Run("self repair converges a single canonical pledge's strays", func(t *testing.T) {
		db := fresh(t, "repair-single")
		pledges, leases, ledger := db.Collection(colPledges), db.Collection(colLeases), db.Collection(colLedger)
		now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
		const (
			old  = "carol|api_key|openai"
			want = "carol|api_key|openai|key-c"
		)
		if _, err := pledges.InsertOne(ctx, bson.M{
			"_id": want, "pool_id": "pool-1", "user_id": "carol", "source": "api_key",
			"ref": "openai", "key_id": "key-c", "enabled": true, "health": "ok"}); err != nil {
			t.Fatalf("seed pledge: %v", err)
		}
		// One canonical pledge for the triple: a reference under the inherited
		// id can only belong to it.
		if _, err := leases.InsertOne(ctx, bson.M{
			"_id": "stray-c", "run_id": "run-c", "pledge_id": old, "donor_id": "carol",
			"closed": false, "acquired_at": now, "expires_at": now.Add(time.Hour)}); err != nil {
			t.Fatalf("seed lease: %v", err)
		}
		if _, err := ledger.InsertOne(ctx, bson.M{
			"_id": old + "|d|2026-11-18", "runs": 2, "period_start": now}); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}

		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		var l bson.M
		if err := leases.FindOne(ctx, bson.M{"_id": "stray-c"}).Decode(&l); err != nil {
			t.Fatalf("read lease stray-c: %v", err)
		}
		if l["pledge_id"] != want {
			t.Errorf("stray lease has pledge_id %v, want %q", l["pledge_id"], want)
		}
		if err := ledger.FindOne(ctx, bson.M{"_id": want + "|d|2026-11-18"}).Err(); err != nil {
			t.Errorf("the inherited ledger row did not converge onto %q: %v", want, err)
		}
		if err := ledger.FindOne(ctx, bson.M{"_id": old + "|d|2026-11-18"}).Err(); err != mongo.ErrNoDocuments {
			t.Errorf("the inherited ledger row is still there after the move (%v)", err)
		}
	})

	t.Run("a resurrected legacy pledge converges on the key it names", func(t *testing.T) {
		db := fresh(t, "resurrected")
		pledges, leases := db.Collection(colPledges), db.Collection(colLeases)
		now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
		const (
			old  = "alice|api_key|anthropic"
			keyA = "alice|api_key|anthropic|key-1"
			keyB = "alice|api_key|anthropic|key-2"
		)
		for _, doc := range []bson.M{
			{"_id": keyA, "pool_id": "pool-1", "user_id": "alice", "source": "api_key", "ref": "anthropic", "key_id": "key-1", "enabled": true, "health": "ok"},
			// An older replica resurrected the legacy document for key-2.
			{"_id": old, "pool_id": "pool-1", "user_id": "alice", "source": "api_key", "ref": "anthropic", "key_id": "key-2", "enabled": true, "health": "ok"},
		} {
			if _, err := pledges.InsertOne(ctx, doc); err != nil {
				t.Fatalf("seed %v: %v", doc["_id"], err)
			}
		}
		if _, err := leases.InsertOne(ctx, bson.M{
			"_id": "key2-lease", "run_id": "run-2", "pledge_id": old, "donor_id": "alice",
			"source": "api_key", "ref": "anthropic", "key_id": "key-2",
			"closed": false, "acquired_at": now, "expires_at": now.Add(time.Hour)}); err != nil {
			t.Fatalf("seed lease: %v", err)
		}

		if err := EnsureSchema(ctx, db); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		var l bson.M
		if err := leases.FindOne(ctx, bson.M{"_id": "key2-lease"}).Decode(&l); err != nil {
			t.Fatalf("read lease: %v", err)
		}
		if l["pledge_id"] != keyB {
			t.Errorf("key-2's lease landed on %v, want its own pledge %q — key-1 stole it", l["pledge_id"], keyB)
		}
		if err := pledges.FindOne(ctx, bson.M{"_id": old}).Err(); err != mongo.ErrNoDocuments {
			t.Errorf("the legacy document is still there after the walk (%v)", err)
		}
		if err := pledges.FindOne(ctx, bson.M{"_id": keyB}).Err(); err != nil {
			t.Errorf("key-2's pledge is not at its key-carrying id: %v", err)
		}
	})
}
