package trigger

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// onSubscriptionTwins runs one contract against the memory, file and Mongo
// twins; the Mongo half skips without ITERION_TEST_MONGO_URI.
func onSubscriptionTwins(t *testing.T, run func(*testing.T, SubscriptionStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { run(t, NewMemorySubscriptionStore()) })
	t.Run("file", func(t *testing.T) {
		run(t, NewFileSubscriptionStore(filepath.Join(t.TempDir(), "trigger-subscriptions.json")))
	})
	t.Run("mongo", func(t *testing.T) {
		uri := os.Getenv("ITERION_TEST_MONGO_URI")
		if uri == "" {
			t.Skip("ITERION_TEST_MONGO_URI not set; requires Mongo conformance harness")
		}
		ctx, cancel := mongotest.Ctx(t)
		defer cancel()
		client, err := mongo.Connect(options.Client().ApplyURI(uri))
		if err != nil {
			t.Fatal(err)
		}
		db := client.Database("iterion_sub_connector_" + bson.NewObjectID().Hex())
		t.Cleanup(func() {
			ctx, cancel := mongotest.TeardownCtx()
			defer cancel()
			_ = db.Drop(ctx)
			_ = client.Disconnect(ctx)
		})
		st := NewMongoSubscriptionStore(db)
		if err := st.EnsureSchema(ctx); err != nil {
			t.Fatal(err)
		}
		run(t, st)
	})
}

func candidateIDs(t *testing.T, st SubscriptionStore, ev Event) []string {
	t.Helper()
	subs, err := st.ListCandidates(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, s := range subs {
		ids = append(ids, s.ID)
	}
	return ids
}

// Two connections to one vendor in one tenant: an intake of each reaches only
// its own connection's subscriptions, in id order; a poller-bound one is no
// candidate for an ingress intake; no connector subscription is a candidate
// for any other source's event; and the key is the store's to derive.
func TestConnectorCandidatesAreTheirConnectionsOnly(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		// Created in an order that is NOT the id order, so the answer's
		// order is the store's sort and never the insertion's.
		born := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		conn := func(id, botID, connID, trig, poller string) Subscription {
			born = born.Add(time.Second)
			return Subscription{ID: id, TenantID: "t1", BotID: botID, Invocation: bundle.InvocationKindConnector, Enabled: true,
				ConnectionID: connID, TriggerID: trig, Poller: poller, ConnectorKey: "client-chosen", CreatedAt: born}
		}
		// s1 and s2 are two BOTS on one connection and trigger — the shape
		// two candidates share (one bot cannot hold the binding twice:
		// Store.Create refuses the duplicate).
		for _, sub := range []Subscription{
			conn("s2", "b1", "conn-a", "slack.message.posted", ""),
			conn("s1", "b2", "conn-a", "slack.message.posted", ""),
			conn("s3", "b", "conn-b", "slack.message.posted", ""),
			conn("s4", "b", "conn-a", "slack.message.posted", "poller-1"),
			conn("s5", "b", "conn-a", "slack.reaction.added", ""),
			{ID: "board", TenantID: "t1", BotID: "b", Enabled: true, Match: Matcher{Sources: []Source{SourceBoard}}},
		} {
			if err := st.Create(ctx, sub); err != nil {
				t.Fatal(err)
			}
		}
		ingress := Event{Source: SourceConnector, TenantID: "t1", Kind: "slack.message.posted", ConnectionID: "conn-a", IngressID: "ing-1"}
		if got := candidateIDs(t, st, ingress); !reflect.DeepEqual(got, []string{"s1", "s2"}) {
			t.Fatalf("conn-a's ingress intake reached %v, want [s1 s2] in id order", got)
		}
		ingress.ConnectionID = "conn-b"
		if got := candidateIDs(t, st, ingress); !reflect.DeepEqual(got, []string{"s3"}) {
			t.Fatalf("conn-b's ingress intake reached %v, want [s3]", got)
		}
		polled := Event{Source: SourceConnector, TenantID: "t1", Kind: "slack.message.posted", ConnectionID: "conn-a", PollerID: "poller-1"}
		if got := candidateIDs(t, st, polled); !reflect.DeepEqual(got, []string{"s4"}) {
			t.Fatalf("the poller's intake reached %v, want [s4]", got)
		}
		board := Event{Source: SourceBoard, TenantID: "t1", Kind: KindCardMoved}
		if got := candidateIDs(t, st, board); !reflect.DeepEqual(got, []string{"board"}) {
			t.Fatalf("a board event's candidates = %v, want no connector subscription", got)
		}
		got, err := st.Get(ctx, "s1")
		if err != nil {
			t.Fatal(err)
		}
		if got.ConnectorKey != "conn-a/slack.message.posted" {
			t.Fatalf("the stored key = %q, want the one derived from its connection and trigger", got.ConnectorKey)
		}
		// An edit re-derives it: moved to conn-b, s1 answers conn-b's intake.
		got.ConnectionID = "conn-b"
		got.ConnectorKey = "client-chosen"
		if err := st.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.Get(ctx, "s1"); got.ConnectorKey != "conn-b/slack.message.posted" {
			t.Fatalf("the key after an edit = %q, want it re-derived", got.ConnectorKey)
		}
		if got := candidateIDs(t, st, ingress); !reflect.DeepEqual(got, []string{"s1", "s3"}) {
			t.Fatalf("conn-b's ingress intake after the edit reached %v, want [s1 s3]", got)
		}
	})
}

// The connector candidate query has its index: tenant, enabled, connector
// key, in that order, on the Mongo twin.
func TestConnectorCandidateIndexIsDeclared(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; requires Mongo conformance harness")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("iterion_sub_index_" + bson.NewObjectID().Hex())
	t.Cleanup(func() {
		ctx, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	if err := NewMongoSubscriptionStore(db).EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cur, err := db.Collection(SubscriptionsCollectionName).Indexes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var idx []struct {
		Name string `bson:"name"`
		Key  bson.D `bson:"key"`
	}
	if err := cur.All(ctx, &idx); err != nil {
		t.Fatal(err)
	}
	for _, ix := range idx {
		if ix.Name != "tenant_enabled_connector_key" {
			continue
		}
		var keys []string
		for _, e := range ix.Key {
			keys = append(keys, e.Key)
		}
		if !reflect.DeepEqual(keys, []string{"tenant_id", "enabled", "connector_key"}) {
			t.Fatalf("index keys = %v, want [tenant_id enabled connector_key]", keys)
		}
		return
	}
	t.Fatalf("no tenant_enabled_connector_key index among %+v", idx)
}
