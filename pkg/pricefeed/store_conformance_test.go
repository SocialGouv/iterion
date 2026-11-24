package pricefeed

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func priceFeedMongoDB(t *testing.T) (context.Context, *mongo.Database) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping real Mongo pricefeed conformance")
	}
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("iterion_pricefeed_" + bson.NewObjectID().Hex())
	t.Cleanup(func() {
		cleanup, done := mongotest.TeardownCtx()
		defer done()
		if err := db.Drop(cleanup); err != nil {
			t.Errorf("drop fixture: %v", err)
		}
		if err := client.Disconnect(cleanup); err != nil {
			t.Errorf("disconnect fixture: %v", err)
		}
	})
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("Mongo fixture must answer: %v", err)
	}
	return ctx, db
}

// Every row runs unchanged on both implementations. BSON's time precision
// is milliseconds; fixtures use that precision, and UpdatedAt is a store
// stamp read back from Get, never a caller-owned timestamp.
func TestPriceFeedStoreConformance(t *testing.T) {
	for _, backend := range []string{"memory", "mongo"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var configs ConfigStore = NewMemoryConfigStore()
			var states StateStore = NewMemoryStateStore()
			if backend == "mongo" {
				var db *mongo.Database
				ctx, db = priceFeedMongoDB(t)
				configs, states = NewMongoConfigStore(db), NewMongoStateStore(db)
			}
			runPriceFeedStoreConformance(t, ctx, configs, states)
			runPriceFeedConfigStamps(t, ctx, configs)
		})
	}
}

func runPriceFeedStoreConformance(t *testing.T, ctx context.Context, configs ConfigStore, states StateStore) {
	t.Helper()
	config := func(name string) *ConfigRecord {
		return &ConfigRecord{UpdatedBy: "operator", Feeds: []FeedConfig{{
			Name: name, Type: TypeLiteLLM, Enabled: true,
			Interval: D(time.Hour), Timeout: D(10 * time.Second),
			Params: map[string]string{"url": "https://gateway.example/model/info", "token": "fixture-token"},
		}}}
	}
	getConfig := func(t *testing.T, id string) *ConfigRecord {
		t.Helper()
		r, err := configs.Get(ctx, id)
		if err != nil || r == nil || r.UpdatedAt.IsZero() {
			t.Fatalf("Get(%s): %+v, %v", id, r, err)
		}
		return r
	}
	t.Run("config-round-trip", func(t *testing.T) {
		id := TeamConfigDocID("round-trip")
		if got, err := configs.Get(ctx, id); err != nil || got != nil {
			t.Fatalf("missing config: %+v, %v", got, err)
		}
		written := config("prices")
		want := cloneConfigRecord(written)
		if err := configs.Put(ctx, id, written); err != nil {
			t.Fatal(err)
		}
		written.Feeds[0].Params["token"] = "writer-mutated"
		got := getConfig(t, id)
		want.UpdatedAt = got.UpdatedAt
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("config lost fields: got %+v, want %+v", got, want)
		}
		got.Feeds[0].Params["token"] = "reader-mutated"
		peerID := TeamConfigDocID("round-trip-peer")
		peer := config("other-prices")
		peer.UpdatedBy = "another-operator"
		if err := configs.Put(ctx, peerID, peer); err != nil {
			t.Fatal(err)
		}
		peer.UpdatedAt = getConfig(t, peerID).UpdatedAt
		listed, err := configs.List(ctx)
		if err != nil || len(listed) != 2 || !reflect.DeepEqual(listed[id], want) || !reflect.DeepEqual(listed[peerID], peer) {
			t.Fatalf("listed configs incomplete or changed: %+v, %v", listed, err)
		}
		listed[id].Feeds[0].Params["token"] = "list-mutated"
		if got := getConfig(t, id); !reflect.DeepEqual(got, want) {
			t.Fatalf("listed config aliases store: %+v", got)
		}
		// Replacement clears removed optional fields, rather than merging.
		if err := configs.Put(ctx, id, &ConfigRecord{Feeds: []FeedConfig{{Name: "bare"}}}); err != nil {
			t.Fatal(err)
		}
		got = getConfig(t, id)
		if got.UpdatedBy != "" || len(got.Feeds) != 1 || len(got.Feeds[0].Params) != 0 || got.Feeds[0].Enabled {
			t.Fatalf("replacement retained old config fields: %+v", got)
		}
	})
	t.Run("config-cas", func(t *testing.T) {
		id := OrgConfigDocID("cas")
		stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if ok, err := configs.PutIfUnchanged(ctx, id, config("missing"), stamp); err != nil || ok {
			t.Fatalf("CAS on missing document with nonzero stamp: %v, %v", ok, err)
		}
		if ok, err := configs.DeleteIfUnchanged(ctx, id, time.Time{}); err != nil || ok {
			t.Fatalf("delete missing document: %v, %v", ok, err)
		}
		if ok, err := configs.PutIfUnchanged(ctx, id, config("first"), time.Time{}); err != nil || !ok {
			t.Fatalf("first CAS: %v, %v", ok, err)
		}
		old := getConfig(t, id)
		if ok, err := configs.PutIfUnchanged(ctx, id, config("duplicate"), time.Time{}); err != nil || ok {
			t.Fatalf("create CAS replaced existing document: %v, %v", ok, err)
		}
		if ok, err := configs.PutIfUnchanged(ctx, id, config("second"), old.UpdatedAt); err != nil || !ok {
			t.Fatalf("current-stamp CAS: %v, %v", ok, err)
		}
		current := getConfig(t, id)
		if current.Feeds[0].Name != "second" || current.UpdatedAt.Equal(old.UpdatedAt) {
			t.Fatalf("CAS did not change value and stamp: old %+v, current %+v", old, current)
		}
		if ok, err := configs.PutIfUnchanged(ctx, id, config("stale"), old.UpdatedAt); err != nil || ok {
			t.Fatalf("stale CAS overwrote newer config: %v, %v", ok, err)
		}
		for _, stale := range []time.Time{time.Time{}, old.UpdatedAt} {
			if ok, err := configs.DeleteIfUnchanged(ctx, id, stale); err != nil || ok {
				t.Fatalf("stale delete removed newer config: %v, %v", ok, err)
			}
		}
		if got := getConfig(t, id); !reflect.DeepEqual(got, current) {
			t.Fatalf("failed CAS/delete altered current config: %+v", got)
		}
		if ok, err := configs.DeleteIfUnchanged(ctx, id, current.UpdatedAt); err != nil || !ok {
			t.Fatalf("current-stamp delete: %v, %v", ok, err)
		}
		if got, err := configs.Get(ctx, id); err != nil || got != nil {
			t.Fatalf("deleted config still exists: %+v, %v", got, err)
		}
	})
	t.Run("concurrent-first-write", func(t *testing.T) {
		requireSingleConfigCASWinner(t, ctx, configs, TeamConfigDocID("race"), time.Time{})
	})
	t.Run("state-round-trip", func(t *testing.T) {
		id := StateDocID(TeamConfigDocID("state"), "prices")
		if got, err := states.Get(ctx, id); err != nil || got != nil {
			t.Fatalf("missing state: %+v, %v", got, err)
		}
		yes, no := true, false
		at := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
		written := &StateRecord{
			Feed: "prices", Type: TypeLiteLLM, Status: StateStale,
			RefreshedAt: at, FreshFor: D(2 * time.Hour), LastAttempt: at.Add(time.Hour), LastError: "upstream unavailable",
			Entries: map[string]modelspecs.Spec{
				"full": {ContextWindow: 262144, MaxOutputTokens: 32768, InputCostPerM: 0.61, OutputCostPerM: 1.8,
					CacheReadCostPerM: 0.12, Reasoning: &yes, ToolCall: &yes, Temperature: &no, ReasoningEfforts: []string{"low", "high"}},
				"unstated":      {},
				"empty-efforts": {ReasoningEfforts: []string{}},
			},
		}
		want := cloneStateRecord(written)
		if err := states.Put(ctx, id, written); err != nil {
			t.Fatal(err)
		}
		written.Entries["full"].ReasoningEfforts[0] = "writer-mutated"
		*written.Entries["full"].Reasoning = false
		got, err := states.Get(ctx, id)
		if err != nil || got == nil || got.UpdatedAt.IsZero() {
			t.Fatalf("state Get: %+v, %v", got, err)
		}
		want.UpdatedAt = got.UpdatedAt
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("state lost fields: got %+v, want %+v", got, want)
		}
		got.Entries["full"].ReasoningEfforts[1] = "reader-mutated"
		*got.Entries["full"].Temperature = true
		peerID := StateDocID(TeamConfigDocID("state"), "other-prices")
		peer := &StateRecord{Feed: "other-prices", Type: TypeExec, Status: StateOK,
			Entries:     map[string]modelspecs.Spec{"peer": {InputCostPerM: 2, OutputCostPerM: 6}},
			RefreshedAt: at.Add(-time.Hour), FreshFor: D(4 * time.Hour)}
		if err := states.Put(ctx, peerID, peer); err != nil {
			t.Fatal(err)
		}
		peerBack, err := states.Get(ctx, peerID)
		if err != nil || peerBack == nil {
			t.Fatalf("peer state: %+v, %v", peerBack, err)
		}
		peer.UpdatedAt = peerBack.UpdatedAt
		listed, err := states.List(ctx)
		if err != nil || len(listed) != 2 || !reflect.DeepEqual(listed[id], want) || !reflect.DeepEqual(listed[peerID], peer) {
			t.Fatalf("listed states incomplete or changed: %+v, %v", listed, err)
		}
		listed[id].Entries["full"].ReasoningEfforts[0] = "list-mutated"
		if again, err := states.Get(ctx, id); err != nil || !reflect.DeepEqual(again, want) {
			t.Fatalf("listed state aliases store: %+v, %v", again, err)
		}
		// No source stamp means legacy, not a different source. An empty
		// replacement must clear every optional value of the old state.
		if err := states.Put(ctx, id, &StateRecord{Feed: "prices", Status: StateNever}); err != nil {
			t.Fatal(err)
		}
		got, err = states.Get(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("empty state: %+v, %v", got, err)
		}
		if len(got.Entries) != 0 || got.Type != "" || !got.RefreshedAt.IsZero() || got.FreshFor != 0 || !got.LastAttempt.IsZero() || got.LastError != "" || got.Status != StateNever {
			t.Fatalf("replacement retained old state fields: %+v", got)
		}
		if got.Of(TypeLiteLLM) != got || got.Fresh(at) {
			t.Fatalf("legacy empty state has wrong meaning: %+v", got)
		}
	})
	t.Run("caller-record-untouched", func(t *testing.T) {
		// The stamp is the store's answer to a later Get, never a write
		// through the caller's pointer: a caller reusing its record would
		// otherwise carry a stale-looking token into its next CAS.
		id := TeamConfigDocID("caller-record")
		rec := config("caller")
		if err := configs.Put(ctx, id, rec); err != nil {
			t.Fatal(err)
		}
		if !rec.UpdatedAt.IsZero() {
			t.Fatalf("Put stamped the caller's record: %+v", rec)
		}
		cas := config("caller-cas")
		if ok, err := configs.PutIfUnchanged(ctx, id, cas, getConfig(t, id).UpdatedAt); err != nil || !ok {
			t.Fatalf("current CAS: %v, %v", ok, err)
		}
		if !cas.UpdatedAt.IsZero() {
			t.Fatalf("PutIfUnchanged stamped the caller's record: %+v", cas)
		}
		sid := StateDocID(id, "prices")
		srec := &StateRecord{Feed: "prices", Status: StateOK}
		if err := states.Put(ctx, sid, srec); err != nil {
			t.Fatal(err)
		}
		if !srec.UpdatedAt.IsZero() {
			t.Fatalf("state Put stamped the caller's record: %+v", srec)
		}
	})
}

func requireSingleConfigCASWinner(t *testing.T, ctx context.Context, configs ConfigStore, id string, previous time.Time) {
	t.Helper()
	start := make(chan struct{})
	type result struct {
		name string
		ok   bool
		err  error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for n := range cap(results) {
		wg.Go(func() {
			name := fmt.Sprintf("writer-%d", n)
			<-start
			ok, err := configs.PutIfUnchanged(ctx, id, &ConfigRecord{UpdatedBy: name}, previous)
			results <- result{name, ok, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var winners []string
	for r := range results {
		if r.err != nil {
			t.Errorf("CAS %s: %v", r.name, r.err)
		}
		if r.ok {
			winners = append(winners, r.name)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("CAS winners = %v, want exactly one", winners)
	}
	got, err := configs.Get(ctx, id)
	if err != nil || got == nil || got.UpdatedBy != winners[0] {
		t.Fatalf("loser overwrote winner %s: %+v, %v", winners[0], got, err)
	}
}

// Seed storage rather than sleeping for a clock tick. A future token models
// a previous writer with a faster clock; zero/null/absent are historical
// encodings that all decode to time.Time{}. None may strand an unconditional
// Put in its retry loop, or let an old token overwrite a successful write.
func runPriceFeedConfigStamps(t *testing.T, ctx context.Context, configs ConfigStore) {
	t.Helper()
	quantum := time.Nanosecond
	if _, isMongo := configs.(*MongoConfigStore); isMongo {
		quantum = time.Millisecond
	}
	t.Run("config-stamps", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			value any
		}{
			{"future", time.Now().UTC().Add(24 * time.Hour).Truncate(time.Millisecond)},
			{"absent", "absent"}, {"null", nil}, {"zero", time.Time{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				seed := func(t *testing.T, id string) {
					t.Helper()
					switch s := configs.(type) {
					case *MemoryConfigStore:
						stamp, _ := tc.value.(time.Time)
						s.mu.Lock()
						s.configs[id] = &ConfigRecord{UpdatedAt: stamp, UpdatedBy: "seed"}
						s.mu.Unlock()
					case *MongoConfigStore:
						doc := bson.M{"_id": id, "updated_at": tc.value, "updated_by": "seed"}
						if tc.name == "absent" {
							delete(doc, "updated_at")
						}
						if _, err := s.col.InsertOne(ctx, doc); err != nil {
							t.Fatal(err)
						}
					default:
						t.Fatalf("missing stamp fixture for %T", configs)
					}
				}
				read := func(t *testing.T, id string) *ConfigRecord {
					t.Helper()
					r, err := configs.Get(ctx, id)
					if err != nil || r == nil {
						t.Fatalf("Get(%s): %+v, %v", id, r, err)
					}
					return r
				}
				if tc.name == "future" {
					t.Run("concurrent-CAS", func(t *testing.T) {
						id := TeamConfigDocID("concurrent-cas")
						seed(t, id)
						requireSingleConfigCASWinner(t, ctx, configs, id, read(t, id).UpdatedAt)
					})
					t.Run("concurrent-Put", func(t *testing.T) {
						id := TeamConfigDocID("concurrent-put")
						seed(t, id)
						previous := read(t, id).UpdatedAt
						start := make(chan struct{})
						putErrs := make(chan error, 8)
						var wg sync.WaitGroup
						for range cap(putErrs) {
							wg.Go(func() {
								<-start
								putErrs <- configs.Put(ctx, id, &ConfigRecord{UpdatedBy: "writer"})
							})
						}
						close(start)
						wg.Wait()
						close(putErrs)
						for err := range putErrs {
							if err != nil {
								t.Error(err)
							}
						}
						// Each Put advances the future token by one native tick,
						// including competing unconditional writers.
						if got := read(t, id); !got.UpdatedAt.Equal(previous.Add(time.Duration(cap(putErrs)) * quantum)) {
							t.Fatalf("concurrent Put token progression: before %v, after %v, want %d ticks of %v", previous, got.UpdatedAt, cap(putErrs), quantum)
						}
					})
				}
				for _, method := range []string{"Put", "PutIfUnchanged", "DeleteIfUnchanged"} {
					t.Run(method, func(t *testing.T) {
						id := TeamConfigDocID(tc.name + "-" + method)
						seed(t, id)
						old := read(t, id)
						rec := &ConfigRecord{UpdatedBy: "writer"}
						switch method {
						case "Put":
							if err := configs.Put(ctx, id, rec); err != nil {
								t.Fatal(err)
							}
						case "PutIfUnchanged":
							if ok, err := configs.PutIfUnchanged(ctx, id, rec, old.UpdatedAt); err != nil || !ok {
								t.Fatalf("current CAS: %v, %v", ok, err)
							}
						case "DeleteIfUnchanged":
							if ok, err := configs.DeleteIfUnchanged(ctx, id, old.UpdatedAt); err != nil || !ok {
								t.Fatalf("current delete: %v, %v", ok, err)
							}
							if r, err := configs.Get(ctx, id); err != nil || r != nil {
								t.Fatalf("deleted config: %+v, %v", r, err)
							}
							return
						}
						current := read(t, id)
						if !current.UpdatedAt.After(old.UpdatedAt) || current.UpdatedAt.Nanosecond()%int(quantum) != 0 || current.UpdatedBy != "writer" {
							t.Fatalf("write did not advance the native-precision token: old %+v, current %+v", old, current)
						}
						if tc.name == "future" && !current.UpdatedAt.Equal(old.UpdatedAt.Add(quantum)) {
							t.Fatalf("write rounded away native precision: old %v, current %v, want one tick of %v", old.UpdatedAt, current.UpdatedAt, quantum)
						}
						if ok, err := configs.PutIfUnchanged(ctx, id, &ConfigRecord{UpdatedBy: "stale"}, old.UpdatedAt); err != nil || ok {
							t.Fatalf("old token replayed after %s: %v, %v", method, ok, err)
						}
						if ok, err := configs.DeleteIfUnchanged(ctx, id, old.UpdatedAt); err != nil || ok {
							t.Fatalf("old token deleted after %s: %v, %v", method, ok, err)
						}
					})
				}
			})
		}
	})
}

// An old document is inserted independently of today's encoder: a matching
// encoder/decoder rename must not make a backwards-incompatible change green.
func TestPriceFeedMongoReadsLegacyState(t *testing.T) {
	ctx, db := priceFeedMongoDB(t)
	id := StateDocID(OrgConfigDocID("legacy"), "prices")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err := db.Collection(ColStates).InsertOne(ctx, bson.M{
		"_id": id, "feed": "prices", "status": StateOK, "refreshed_at": at, "fresh_for": int64(D(time.Hour)),
		"entries": bson.M{"model": bson.M{"contextwindow": 131072, "inputcostperm": 0.6, "outputcostperm": 1.8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	states := NewMongoStateStore(db)
	got, err := states.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("legacy read: %+v, %v", got, err)
	}
	spec := got.Entries["model"]
	if spec.ContextWindow != 131072 || spec.InputCostPerM != 0.6 || spec.OutputCostPerM != 1.8 || spec.ReasoningEfforts != nil || spec.Reasoning != nil || spec.CacheReadCostPerM != 0 {
		t.Fatalf("legacy spec changed: %+v", spec)
	}
	if !got.UpdatedAt.IsZero() || got.Type != "" || got.Of(TypeLiteLLM) != got || !got.Fresh(at) || got.Fresh(at.Add(time.Hour+time.Millisecond)) {
		t.Fatalf("legacy dates/source changed: %+v", got)
	}
	listed, err := states.List(ctx)
	if err != nil || !reflect.DeepEqual(listed[id], got) {
		t.Fatalf("legacy List differs: %+v, %v", listed[id], err)
	}
}
