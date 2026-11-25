package forge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The spine fields of an integration (ADR-172) and the two targeted stamps
// that write them are a CONTRACT, run against both stores: the memory twin
// here, the Mongo one under ITERION_TEST_MONGO_URI (./pkg/forge/... is in the
// mongo-conformance job's package list).

func spineInstant(h int) time.Time { return time.Date(2026, 11, 23, h, 0, 0, 0, time.UTC) }

func boolPtr(b bool) *bool { return &b }

func mustStore(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func getIntegration(t *testing.T, store RepoIntegrationStore, id string) RepoIntegration {
	t.Helper()
	ri, err := store.GetProvision(context.Background(), id)
	if err != nil {
		t.Fatalf("read integration %s: %v", id, err)
	}
	return ri
}

func sameInstantPtr(got *time.Time, want time.Time) bool {
	return got != nil && got.Equal(want)
}

// A BotTrigger's Labels has three states and the store keeps all three: nil
// follows the manifest, an empty list claims any label. A store that folded
// the empty list into nil would turn "any label" into "the manifest's" on the
// next read, narrowing a bot the operator had opened.
func spineFieldsRoundTrip(t *testing.T, store RepoIntegrationStore) {
	t.Helper()
	ctx := context.Background()
	since, adopted, synced := spineInstant(8), spineInstant(9), spineInstant(10)
	want := RepoIntegration{
		ID: "rt", TenantID: "rt-tenant", ConnectionID: "conn", RepoFullName: "o/rt", WebhookID: "wh",
		ForgeRouting: webhooks.ForgeRouting{Issues: webhooks.RoutingSpine, Comments: webhooks.RoutingLegacy},
		SpineSince:   &since,
		BotTriggers: map[string]BotTrigger{
			"manifest": {},
			"any":      {Labels: []string{}},
			"pinned":   {Labels: []string{"urgent", "*-chaos"}, Enabled: boolPtr(false), Residual: true, LaunchVars: map[string]string{"force": "true"}},
		},
		SubscriptionsAdoptedAt: &adopted,
		SubscriptionsSyncedAt:  &synced,
		SubscriptionsSyncError: "overlapping rows",
	}
	mustStore(t, store.Create(ctx, want))

	check := func(stage string) RepoIntegration {
		t.Helper()
		got := getIntegration(t, store, "rt")
		if got.ForgeRouting != want.ForgeRouting {
			t.Fatalf("%s: forge routing = %+v, want %+v", stage, got.ForgeRouting, want.ForgeRouting)
		}
		if !sameInstantPtr(got.SpineSince, since) || !sameInstantPtr(got.SubscriptionsAdoptedAt, adopted) || !sameInstantPtr(got.SubscriptionsSyncedAt, synced) {
			t.Fatalf("%s: instants = spine_since %v, adopted %v, synced %v; want %v, %v, %v",
				stage, got.SpineSince, got.SubscriptionsAdoptedAt, got.SubscriptionsSyncedAt, since, adopted, synced)
		}
		if got.SubscriptionsSyncError != "overlapping rows" {
			t.Fatalf("%s: sync error = %q", stage, got.SubscriptionsSyncError)
		}
		if !botTriggersEqual(got.BotTriggers, want.BotTriggers) {
			t.Fatalf("%s: bot triggers = %+v, want %+v", stage, got.BotTriggers, want.BotTriggers)
		}
		if got.BotTriggers["manifest"].Labels != nil {
			t.Fatalf("%s: a nil labels list came back as %#v: \"follow the manifest\" now reads as an explicit list", stage, got.BotTriggers["manifest"].Labels)
		}
		if l := got.BotTriggers["any"].Labels; l == nil || len(l) != 0 {
			t.Fatalf("%s: an empty labels list came back as %#v: \"any label\" now reads as \"follow the manifest\"", stage, l)
		}
		return got
	}
	got := check("after Create")
	// A whole-document Update of what was read is the round trip every
	// read-modify-write caller makes.
	got.MinAuthorRole = "maintainer"
	mustStore(t, store.Update(ctx, got))
	check("after Update")
}

// StampSubscriptionSync writes the outcome of one projection and nothing else.
func stampSubscriptionSyncContract(t *testing.T, store RepoIntegrationStore) {
	t.Helper()
	ctx := context.Background()
	const id = "stamp-sync"
	mustStore(t, store.Create(ctx, RepoIntegration{
		ID: id, TenantID: "stamp-tenant", ConnectionID: "conn", RepoFullName: "o/stamp", WebhookID: "wh",
		LaunchVars:  map[string]string{"gate_context": "revi/review"},
		HoldLabels:  []string{"hold"},
		BotTriggers: map[string]BotTrigger{"operator": {Labels: []string{"mine"}}},
	}))
	before := getIntegration(t, store, id)

	// A refused projection: the refusal and its instant, nothing adopted.
	mustStore(t, store.StampSubscriptionSync(ctx, id, nil, spineInstant(1), "$bot_triggers: rows overlap"))
	got := getIntegration(t, store, id)
	if got.SubscriptionsSyncError != "$bot_triggers: rows overlap" || !sameInstantPtr(got.SubscriptionsSyncedAt, spineInstant(1)) {
		t.Fatalf("refused stamp: error %q at %v", got.SubscriptionsSyncError, got.SubscriptionsSyncedAt)
	}
	if got.SubscriptionsAdoptedAt != nil {
		t.Fatalf("a REFUSED projection stamped the adoption (%v): the next projection would no longer pin the bots' derived state", got.SubscriptionsAdoptedAt)
	}

	// The first successful projection: adoption pins land for the bots that
	// hold no entry, never over the operator's.
	pins := map[string]BotTrigger{
		"winner":   {Residual: true},
		"loser":    {Enabled: boolPtr(false)},
		"operator": {Enabled: boolPtr(false)},
	}
	mustStore(t, store.StampSubscriptionSync(ctx, id, pins, spineInstant(2), ""))
	got = getIntegration(t, store, id)
	wantTriggers := map[string]BotTrigger{
		"winner":   {Residual: true},
		"loser":    {Enabled: boolPtr(false)},
		"operator": {Labels: []string{"mine"}},
	}
	if !botTriggersEqual(got.BotTriggers, wantTriggers) {
		t.Fatalf("after the adoption stamp, bot triggers = %+v, want %+v (a pin lands only where the bot has no entry)", got.BotTriggers, wantTriggers)
	}
	if got.SubscriptionsSyncError != "" {
		t.Fatalf("a successful stamp left the refusal %q standing", got.SubscriptionsSyncError)
	}
	if !sameInstantPtr(got.SubscriptionsAdoptedAt, spineInstant(2)) || !sameInstantPtr(got.SubscriptionsSyncedAt, spineInstant(2)) {
		t.Fatalf("adoption %v, synced %v; want both at %v", got.SubscriptionsAdoptedAt, got.SubscriptionsSyncedAt, spineInstant(2))
	}

	// A later projection: the adoption instant does not move, and a pin for
	// an adopted bot does not replace what it holds.
	mustStore(t, store.StampSubscriptionSync(ctx, id, map[string]BotTrigger{"winner": {Enabled: boolPtr(false)}, "late": {Residual: true}}, spineInstant(3), ""))
	got = getIntegration(t, store, id)
	if !sameInstantPtr(got.SubscriptionsAdoptedAt, spineInstant(2)) {
		t.Fatalf("the adoption instant moved to %v, want it kept at %v", got.SubscriptionsAdoptedAt, spineInstant(2))
	}
	if !sameInstantPtr(got.SubscriptionsSyncedAt, spineInstant(3)) {
		t.Fatalf("synced = %v, want %v", got.SubscriptionsSyncedAt, spineInstant(3))
	}
	wantTriggers["late"] = BotTrigger{Residual: true}
	if !botTriggersEqual(got.BotTriggers, wantTriggers) {
		t.Fatalf("after a later stamp, bot triggers = %+v, want %+v", got.BotTriggers, wantTriggers)
	}

	// Nothing else moved: the stamp compares no version and advances none, and
	// it carries no snapshot of the rest of the document.
	if got.Version != before.Version {
		t.Fatalf("the stamp moved the version %d → %d: the fence it runs in already counted its one advance", before.Version, got.Version)
	}
	if !reflect.DeepEqual(got.LaunchVars, before.LaunchVars) || !reflect.DeepEqual(got.HoldLabels, before.HoldLabels) || got.WebhookID != before.WebhookID {
		t.Fatalf("the stamp rewrote operator settings: %+v", got)
	}

	if err := store.StampSubscriptionSync(ctx, "no-such-integration", nil, spineInstant(4), ""); !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("stamp on a missing integration: err = %v, want ErrIntegrationNotFound", err)
	}
}

// StampForgeRouting writes one lane and the first-spine instant.
func stampForgeRoutingContract(t *testing.T, store RepoIntegrationStore) {
	t.Helper()
	ctx := context.Background()
	const id = "stamp-routing"
	mustStore(t, store.Create(ctx, RepoIntegration{
		ID: id, TenantID: "routing-tenant", ConnectionID: "conn", RepoFullName: "o/routing", WebhookID: "wh",
		LabelAllowlist: []string{"implement"},
	}))
	before := getIntegration(t, store, id)

	// Legacy on a never-flipped integration stamps no spine instant.
	mustStore(t, store.StampForgeRouting(ctx, id, webhooks.LanePullRequests, webhooks.RoutingLegacy, spineInstant(1)))
	got := getIntegration(t, store, id)
	if got.ForgeRouting != (webhooks.ForgeRouting{PullRequests: webhooks.RoutingLegacy}) || got.SpineSince != nil {
		t.Fatalf("legacy stamp: routing %+v, spine_since %v", got.ForgeRouting, got.SpineSince)
	}

	// The first spine stamps the instant and keeps the other lane.
	mustStore(t, store.StampForgeRouting(ctx, id, webhooks.LaneIssues, webhooks.RoutingSpine, spineInstant(2)))
	got = getIntegration(t, store, id)
	if got.ForgeRouting != (webhooks.ForgeRouting{Issues: webhooks.RoutingSpine, PullRequests: webhooks.RoutingLegacy}) {
		t.Fatalf("spine stamp: routing %+v — the lane write replaced the other lanes", got.ForgeRouting)
	}
	if !sameInstantPtr(got.SpineSince, spineInstant(2)) {
		t.Fatalf("first spine: spine_since = %v, want %v", got.SpineSince, spineInstant(2))
	}

	// A rollback keeps the instant; a second spine does not move it.
	mustStore(t, store.StampForgeRouting(ctx, id, webhooks.LaneIssues, webhooks.RoutingLegacy, spineInstant(3)))
	got = getIntegration(t, store, id)
	if got.ForgeRouting.Issues != webhooks.RoutingLegacy || !sameInstantPtr(got.SpineSince, spineInstant(2)) {
		t.Fatalf("rollback: issues %q, spine_since %v; want legacy and the first instant %v", got.ForgeRouting.Issues, got.SpineSince, spineInstant(2))
	}
	mustStore(t, store.StampForgeRouting(ctx, id, webhooks.LaneComments, webhooks.RoutingSpine, spineInstant(4)))
	got = getIntegration(t, store, id)
	if !sameInstantPtr(got.SpineSince, spineInstant(2)) {
		t.Fatalf("second spine moved spine_since to %v, want it kept at %v", got.SpineSince, spineInstant(2))
	}
	if got.ForgeRouting != (webhooks.ForgeRouting{Issues: webhooks.RoutingLegacy, PullRequests: webhooks.RoutingLegacy, Comments: webhooks.RoutingSpine}) {
		t.Fatalf("routing after four stamps = %+v", got.ForgeRouting)
	}

	if got.Version != before.Version {
		t.Fatalf("the stamp moved the version %d → %d", before.Version, got.Version)
	}
	if !reflect.DeepEqual(got.LabelAllowlist, before.LabelAllowlist) {
		t.Fatalf("the stamp rewrote operator settings: %+v", got.LabelAllowlist)
	}
	if err := store.StampForgeRouting(ctx, id, "branches", webhooks.RoutingSpine, spineInstant(5)); err == nil || !strings.Contains(err.Error(), "branches") {
		t.Fatalf("an unknown lane was written: err = %v", err)
	}
	if err := store.StampForgeRouting(ctx, "no-such-integration", webhooks.LaneIssues, webhooks.RoutingSpine, spineInstant(5)); !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("stamp on a missing integration: err = %v, want ErrIntegrationNotFound", err)
	}
}

// The provision fence is single-use, and the two stamps are the writes it
// admits. Both stores must agree on that, or a caller proven on one conflicts
// on the other.
func provisionFenceContract(t *testing.T, store RepoIntegrationStore) {
	t.Helper()
	ctx := context.Background()
	const id = "fence"
	mustStore(t, store.Create(ctx, RepoIntegration{ID: id, TenantID: "fence-tenant", ConnectionID: "conn", RepoFullName: "o/fence", WebhookID: "wh"}))
	snapshot := getIntegration(t, store, id)

	// A stamp made with the fence's context lands, and the fence advances the
	// version exactly once.
	var updateInFence error
	var seenInFence RepoIntegration
	err := store.WithProvisionVersion(ctx, id, snapshot.Version, false, func(tx context.Context) error {
		if err := store.StampSubscriptionSync(tx, id, map[string]BotTrigger{"bot": {Residual: true}}, spineInstant(1), ""); err != nil {
			return err
		}
		// A read with the fence's context sees the fence's own writes: the
		// version already advanced, the stamp already there.
		var err error
		if seenInFence, err = store.GetProvision(tx, id); err != nil {
			return err
		}
		if err := store.StampForgeRouting(tx, id, webhooks.LaneIssues, webhooks.RoutingSpine, spineInstant(1)); err != nil {
			return err
		}
		// The whole-document write a caller must NOT make here: its snapshot
		// predates the fence, whose advance it conflicts with.
		stale := snapshot
		stale.MinAuthorRole = "maintainer"
		updateInFence = store.Update(tx, stale)
		return nil
	})
	if err != nil {
		t.Fatalf("a fence whose callback only stamps failed: %v", err)
	}
	if seenInFence.Version != snapshot.Version+1 || !seenInFence.BotTriggers["bot"].Residual {
		t.Fatalf("a read inside the fence saw version %d (want %d) and bot triggers %+v: the fence advances the version before its callback runs", seenInFence.Version, snapshot.Version+1, seenInFence.BotTriggers)
	}
	if !errors.Is(updateInFence, ErrIntegrationConflict) {
		t.Fatalf("a whole-document Update of a pre-fence snapshot, inside the fence: err = %v, want ErrIntegrationConflict", updateInFence)
	}
	got := getIntegration(t, store, id)
	if got.Version != snapshot.Version+1 {
		t.Fatalf("version after one fence = %d, want %d", got.Version, snapshot.Version+1)
	}
	if !got.BotTriggers["bot"].Residual || got.ForgeRouting.Issues != webhooks.RoutingSpine || !sameInstantPtr(got.SubscriptionsSyncedAt, spineInstant(1)) {
		t.Fatalf("the stamps made inside the fence did not land: %+v", got)
	}
	if got.MinAuthorRole != "" {
		t.Fatalf("the refused Update still wrote: min_author_role = %q", got.MinAuthorRole)
	}

	// The same version cannot be fenced twice.
	ran := false
	err = store.WithProvisionVersion(ctx, id, snapshot.Version, false, func(context.Context) error { ran = true; return nil })
	if !errors.Is(err, ErrIntegrationConflict) || ran {
		t.Fatalf("a second fence on the version the first one consumed: err = %v, callback ran = %v; want ErrIntegrationConflict and no callback", err, ran)
	}

	// A callback that fails leaves nothing behind: neither its stamps nor the
	// version advance.
	boom := errors.New("projection refused")
	err = store.WithProvisionVersion(ctx, id, got.Version, false, func(tx context.Context) error {
		if err := store.StampSubscriptionSync(tx, id, map[string]BotTrigger{"other": {Residual: true}}, spineInstant(2), "half-written"); err != nil {
			return err
		}
		if err := store.StampForgeRouting(tx, id, webhooks.LaneIssues, webhooks.RoutingLegacy, spineInstant(2)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("a failing callback: err = %v, want its own error", err)
	}
	after := getIntegration(t, store, id)
	if after.Version != got.Version {
		t.Fatalf("a failed fence moved the version %d → %d", got.Version, after.Version)
	}
	if _, pinned := after.BotTriggers["other"]; pinned || after.SubscriptionsSyncError != "" || after.ForgeRouting.Issues != webhooks.RoutingSpine || !sameInstantPtr(after.SubscriptionsSyncedAt, spineInstant(1)) {
		t.Fatalf("a failed fence kept what its callback stamped: %+v", after)
	}
	// And the version it did not consume is still fenceable.
	mustStore(t, store.WithProvisionVersion(ctx, id, after.Version, false, func(context.Context) error { return nil }))
}

func TestMemoryRepoIntegrationStore_SpineFieldsRoundTrip(t *testing.T) {
	spineFieldsRoundTrip(t, NewMemoryRepoIntegrationStore())
}

func TestMemoryRepoIntegrationStore_StampSubscriptionSync(t *testing.T) {
	stampSubscriptionSyncContract(t, NewMemoryRepoIntegrationStore())
}

func TestMemoryRepoIntegrationStore_StampForgeRouting(t *testing.T) {
	stampForgeRoutingContract(t, NewMemoryRepoIntegrationStore())
}

func TestMemoryRepoIntegrationStore_ProvisionFenceAdmitsStampsAndRefusesUpdate(t *testing.T) {
	provisionFenceContract(t, NewMemoryRepoIntegrationStore())
}

// The twin that runs in production: the stamps are aggregation-pipeline
// updates there and the fence is a transaction, so nothing the memory rows
// prove says they hold on it.
func TestMongoRepoIntegrationStore_SpineContract(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo repo-integration spine suite")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo.Connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_repo_integration_spine_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	store := NewMongoRepoIntegrationStore(db)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	t.Run("spine fields round trip", func(t *testing.T) { spineFieldsRoundTrip(t, store) })
	t.Run("stamp subscription sync", func(t *testing.T) { stampSubscriptionSyncContract(t, store) })
	t.Run("stamp forge routing", func(t *testing.T) { stampForgeRoutingContract(t, store) })
	t.Run("fence admits stamps and refuses update", func(t *testing.T) { provisionFenceContract(t, store) })
	t.Run("a successful stamp removes the refusal field", func(t *testing.T) {
		// "" stored by a pipeline $set is a present field; the stamp removes it.
		var doc bson.M
		if err := db.Collection(RepoIntegrationsCollectionName).FindOne(ctx, bson.M{"_id": "stamp-sync"}).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		if v, present := doc["subscriptions_sync_error"]; present {
			t.Fatalf("subscriptions_sync_error is still stored (%#v) after a successful stamp", v)
		}
	})
}

// The codec half of the round trip, which needs no server: what the Mongo
// store inserts and decodes is exactly these bytes.
func TestRepoIntegrationBSON_SpineFields(t *testing.T) {
	encode := func(ri RepoIntegration) bson.M {
		t.Helper()
		raw, err := bson.Marshal(ri)
		if err != nil {
			t.Fatal(err)
		}
		var doc bson.M
		if err := bson.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	// An integration nobody flipped or projected stores none of the keys: a
	// zero routing written as {} would make every unflipped document differ
	// from what it holds today.
	plain := encode(RepoIntegration{ID: "plain"})
	for _, key := range []string{"forge_routing", "spine_since", "bot_triggers", "subscriptions_adopted_at", "subscriptions_synced_at", "subscriptions_sync_error"} {
		if v, present := plain[key]; present {
			t.Errorf("an integration that holds no %s still encodes the key (%#v)", key, v)
		}
	}

	in := RepoIntegration{ID: "codec", BotTriggers: map[string]BotTrigger{"manifest": {}, "any": {Labels: []string{}}}}
	raw, err := bson.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out RepoIntegration
	if err := bson.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.BotTriggers["manifest"].Labels != nil {
		t.Errorf("nil labels decode as %#v", out.BotTriggers["manifest"].Labels)
	}
	if l := out.BotTriggers["any"].Labels; l == nil || len(l) != 0 {
		t.Errorf("empty labels decode as %#v: the any-label state is lost in the codec", l)
	}

	// StampForgeRouting's Mongo twin merges {<lane>: mode} into forge_routing:
	// a lane name is the key ForgeRouting stores that lane under.
	for _, lane := range routingLanes {
		var r webhooks.ForgeRouting
		if !setRoutingLane(&r, lane, webhooks.RoutingSpine) {
			t.Fatalf("lane %q is not settable", lane)
		}
		doc := encode(RepoIntegration{ID: "lane", ForgeRouting: r})
		routing, ok := doc["forge_routing"].(bson.D)
		if !ok {
			t.Fatalf("lane %q: forge_routing encodes as %#v", lane, doc["forge_routing"])
		}
		if len(routing) != 1 || routing[0].Key != lane || routing[0].Value != webhooks.RoutingSpine {
			t.Errorf("lane %q is stored as %v: the targeted lane write would land beside the field the struct reads", lane, routing)
		}
	}
}

// The integration is served as JSON by the studio's Integrations tab.
func TestRepoIntegrationJSON_SpineFields(t *testing.T) {
	plain, err := json.Marshal(RepoIntegration{ID: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"forge_routing", "spine_since", "bot_triggers", "subscriptions_adopted_at", "subscriptions_synced_at", "subscriptions_sync_error"} {
		if strings.Contains(string(plain), `"`+key+`"`) {
			t.Errorf("an integration that holds no %s still prints the key: %s", key, plain)
		}
	}
	adopted := spineInstant(1)
	full, err := json.Marshal(RepoIntegration{
		ID:                     "full",
		ForgeRouting:           webhooks.ForgeRouting{Issues: webhooks.RoutingSpine},
		SubscriptionsAdoptedAt: &adopted,
		BotTriggers:            map[string]BotTrigger{"manifest": {}, "any": {Labels: []string{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(full)
	for _, want := range []string{`"forge_routing":{"issues":"spine"}`, `"any":{"labels":[]}`, `"manifest":{"labels":null}`} {
		if !strings.Contains(body, want) {
			t.Errorf("the integration's JSON lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "adopted") {
		t.Errorf("the adoption stamp is internal state and must not be served: %s", body)
	}
}
