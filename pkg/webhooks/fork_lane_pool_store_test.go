package webhooks

import (
	"context"
	"reflect"
	"testing"
)

// The fork lane's pool (ADR-162 Q3) rides the config document in BOTH store
// twins: written at creation, read back as written, and kept across an
// Update that carries it — the PATCH route writes back the row it read, and
// the immutability it enforces is only as good as a store that does not drop
// the field on that write. The Mongo half runs in the mongo-conformance CI
// job (ITERION_TEST_MONGO_URI); locally it skips and says so.
//
// Mutations: the bson key renamed or the field dropped from Config → the
// tag pin and the round-trip go red; MongoConfigStore.Update building its
// document from a field list that omits it → the Mongo twin's read-back.
func TestConfigStoreForkLanePoolRoundTrips(t *testing.T) {
	f, ok := reflect.TypeOf(Config{}).FieldByName("ForkLanePool")
	if !ok {
		t.Fatal("Config.ForkLanePool is gone — the fork lane's runs would execute on their team's mapping")
	}
	if got := f.Tag.Get("bson"); got != "fork_lane_pool,omitempty" {
		t.Fatalf("Config.ForkLanePool bson tag = %q, want \"fork_lane_pool,omitempty\" — a renamed key orphans every stored fork-lane config's pool", got)
	}
	if got := f.Tag.Get("json"); got != "fork_lane_pool,omitempty" {
		t.Fatalf("Config.ForkLanePool json tag = %q, want \"fork_lane_pool,omitempty\" — the API shape", got)
	}
	onConfigStoreTwins(t, func(t *testing.T, st ConfigStore) {
		ctx := context.Background()
		cfg := Config{ID: "wh-fork-pool", TenantID: "t1", TokenHash: "hash-fork-pool", Provider: ProviderGitHub, ForkLane: true, ForkLanePool: "forks"}
		if err := st.Create(ctx, cfg); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := st.Get(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !got.ForkLane || got.ForkLanePool != "forks" {
			t.Fatalf("read back ForkLane=%v ForkLanePool=%q, want the lane and its pool", got.ForkLane, got.ForkLanePool)
		}
		got.Name = "renamed"
		if err := st.Update(ctx, got); err != nil {
			t.Fatalf("Update: %v", err)
		}
		after, err := st.Get(ctx, cfg.ID)
		if err != nil {
			t.Fatalf("Get after Update: %v", err)
		}
		if after.ForkLanePool != "forks" || after.Name != "renamed" {
			t.Fatalf("after Update ForkLanePool=%q Name=%q, want the pool kept beside the patched field", after.ForkLanePool, after.Name)
		}
	})
}
