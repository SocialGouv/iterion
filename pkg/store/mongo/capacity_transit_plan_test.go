package mongo

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// explainFilter explains a find of filter on the runs collection and returns
// its winning plan.
func explainFilter(t *testing.T, s *Store, filter bson.M) string {
	t.Helper()
	find := bson.D{
		{Key: "find", Value: s.runs.Name()},
		{Key: "filter", Value: filter},
		{Key: "projection", Value: bson.M{"_id": 1}},
	}
	raw, err := s.db.RunCommand(context.Background(), bson.D{{Key: "explain", Value: find}, {Key: "verbosity", Value: "queryPlanner"}}).Raw()
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	winning, err := raw.LookupErr("queryPlanner", "winningPlan")
	if err != nil {
		t.Fatalf("explain reply carries no queryPlanner.winningPlan: %v\n%s", err, raw)
	}
	return winning.String()
}

// The transit arm of every ledger reading is served by its own partial index:
// the count, the adoption and the alive listing are rooted $or filters whose
// transit branch reads capacity_transit and whose status branch reads
// status_created — no collection scan. The population is shaped like
// production: many runs parked out of transit, a few alive, a few in transit.
// Red when the index is dropped, or when the holder branches are nested under
// a conjunction instead of rooted.
func TestCapacityTransit_TransitArmUsesItsIndex(t *testing.T) {
	s := watchersTestStore(t)
	ctx := store.WithIdentity(context.Background(), "_test", "_test")
	const fp = "fp-plan"
	now := time.Now().UTC()
	var docs []any
	for i := range 600 {
		docs = append(docs, bson.M{"_id": "parked-" + itoa(i), "status": string(store.RunStatusFailedResumable),
			"continuation_state": string(store.ContinuationFinal), "cred_fingerprints": bson.A{fp}, "created_at": now})
	}
	for i := range 300 {
		docs = append(docs, bson.M{"_id": "finished-" + itoa(i), "status": string(store.RunStatusFinished),
			"cred_fingerprints": bson.A{fp}, "created_at": now})
	}
	for i := range 5 {
		docs = append(docs, bson.M{"_id": "alive-" + itoa(i), "status": string(store.RunStatusRunning),
			"cred_fingerprints": bson.A{fp}, "created_at": now})
		docs = append(docs, bson.M{"_id": "transit-" + itoa(i), "status": string(store.RunStatusFailedResumable),
			"continuation_state": string(store.ContinuationRedeliveryPending), "continuation_until": now.Add(5 * time.Minute),
			"cred_fingerprints": bson.A{fp}, "created_at": now})
	}
	if _, err := s.runs.InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n, err := s.CountAliveRunsWithCredFingerprint(ctx, fp, ""); err != nil || n != 10 {
		t.Fatalf("alive(%s) = %d/%v, want the 5 alive and the 5 in transit", fp, n, err)
	}
	for _, c := range []struct {
		name   string
		filter bson.M
	}{
		{"count", spendsCredentialFilter(fp, "", now)},
		{"adoption", credSlotAdoptableFilter(fp, "asker", now)},
		{"listing", rootedHolderFilter([]bson.M{{}}, now, notDeleted(bson.M{}))},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := explainFilter(t, s, c.filter)
			if strings.Contains(plan, `"COLLSCAN"`) {
				t.Fatalf("%s: the winning plan scans the collection:\n%s", c.name, plan)
			}
			if !strings.Contains(plan, `"indexName": "capacity_transit"`) {
				t.Fatalf("%s: the transit branch does not read capacity_transit:\n%s", c.name, plan)
			}
		})
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{digits[i%10]}, out...)
		i /= 10
	}
	return string(out)
}

// The holder filter is the query form of store.Run.HoldsCapacity: on raw
// documents of every shape — including shapes no writer produces (a transit
// instant beside another continuation) — the count and the Go predicate
// agree, document by document.
func TestCapacityTransit_HolderFilterMirrorsThePredicate(t *testing.T) {
	s := watchersTestStore(t)
	ctx := store.WithIdentity(context.Background(), "_test", "_test")
	now := time.Now().UTC().Truncate(time.Millisecond)
	ahead, past := now.Add(time.Minute), now.Add(-time.Minute)
	type shape struct {
		status       store.RunStatus
		continuation store.ContinuationState
		until        *time.Time
	}
	shapes := []shape{
		{store.RunStatusQueued, "", nil},
		{store.RunStatusRunning, "", nil},
		{store.RunStatusFailedResumable, store.ContinuationRedeliveryPending, &ahead},
		{store.RunStatusFailedResumable, store.ContinuationRedeliveryPending, &past},
		{store.RunStatusFailedResumable, store.ContinuationRedeliveryPending, &now},
		{store.RunStatusFailedResumable, store.ContinuationRedeliveryPending, nil},
		{store.RunStatusFailedResumable, store.ContinuationRetryArmed, &ahead},
		{store.RunStatusFailedResumable, store.ContinuationFinal, &ahead},
		{store.RunStatusFailedResumable, "", &ahead},
		{store.RunStatusPausedOperator, store.ContinuationRedeliveryPending, &ahead},
		{store.RunStatusCancelled, store.ContinuationRedeliveryPending, &ahead},
		{store.RunStatusPausedWaitingHuman, "", nil},
		{store.RunStatusFinished, "", nil},
	}
	for i, sh := range shapes {
		fp := "fp-mirror-" + itoa(i)
		doc := bson.M{"_id": "mirror-" + itoa(i), "status": string(sh.status), "cred_fingerprints": bson.A{fp}, "created_at": now}
		if sh.continuation != "" {
			doc["continuation_state"] = string(sh.continuation)
		}
		if sh.until != nil {
			doc["continuation_until"] = *sh.until
		}
		if _, err := s.runs.InsertOne(ctx, doc); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		run := &store.Run{Status: sh.status, ContinuationState: sh.continuation, ContinuationUntil: sh.until, CredFingerprints: []string{fp}}
		want := 0
		if run.SpendsCredential(fp, now) {
			want = 1
		}
		n, err := s.runs.CountDocuments(ctx, spendsCredentialFilter(fp, "", now))
		if err != nil {
			t.Fatalf("count %d: %v", i, err)
		}
		if int(n) != want {
			t.Errorf("shape %+v (until %v): the filter counts %d, the predicate %d", sh, sh.until, n, want)
		}
	}
}

// The ledger's judgments read the primary whatever the URI prefers: a client
// configured to read secondaries — on a replica set with none — still admits,
// adopts, evicts and re-asserts. Red when a judgment follows the URI's
// preference: the read finds no secondary to serve it.
func TestCapacityTransit_LedgerReadsArePrimary(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	sep := "?"
	if strings.Contains(uri, "?") {
		sep = "&"
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	db := "iterion_ledger_primary_" + bsonNonce(t)
	primary, err := New(ctx, Config{URI: uri, Database: db, Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	secondary, err := New(ctx, Config{URI: uri + sep + "readPreference=secondary&serverSelectionTimeoutMS=1500", Database: db, Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New (secondary preference): %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = primary.db.Drop(drop)
		_ = secondary.Close(drop)
		_ = primary.Close(drop)
	})
	const fp = "fp-primary"
	rctx := transitCtx()
	stampedRun(t, primary, "holder", store.RunStatusRunning, fp)
	stampedRun(t, primary, "parked", store.RunStatusPausedOperator, fp)
	if adm, err := primary.ReserveCredSlot(rctx, fp, "parked", 2); err != nil || !adm.Granted {
		t.Fatalf("admit parked: %+v %v", adm, err)
	}
	stampedRun(t, primary, "asker", store.RunStatusPausedOperator, fp)
	// The asker's admission adopts the alive holder (a Find), reads the row,
	// and its refusal's reconcile loads each holder to judge it: every read a
	// judgment of the cap.
	adm, err := secondary.ReserveCredSlot(rctx, fp, "asker", 2)
	if err != nil {
		t.Fatalf("ReserveCredSlot through a secondary-preferring client: %v", err)
	}
	if !adm.Granted {
		t.Fatalf("asker: %+v, want granted once the paused holder is evicted", adm)
	}
	if re, err := secondary.ReassertCredSlot(rctx, fp, "asker", adm.At); err != nil || !re.Held {
		t.Fatalf("ReassertCredSlot through a secondary-preferring client: %+v %v", re, err)
	}
}
