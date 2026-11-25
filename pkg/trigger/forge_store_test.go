package trigger

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

func sortedCandidateIDs(t *testing.T, st SubscriptionStore, ev Event) []string {
	t.Helper()
	ids := candidateIDs(t, st, ev)
	slices.Sort(ids)
	return ids
}

func boundIDs(t *testing.T, st SubscriptionStore, tenantID, binding string) []string {
	t.Helper()
	subs, err := st.ListByForgeBinding(context.Background(), tenantID, binding)
	if err != nil {
		t.Fatal(err)
	}
	return subscriptionIDs(subs)
}

// A subscription bound to a forge binding is no bus candidate: not for an
// event of its own repository, not as a tenant-wide row, not for a
// connector intake whose key it carries — whichever binding it holds and
// whoever wrote it.
func TestListCandidates_ExcludesForgeBound(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		scoped := forgeRow("forge-scoped", "t1", "nestor", "int-1", "nestor")
		scoped.Repo = "acme/api"
		wide := forgeRow("forge-wide", "t1", "saboteur", "int-1", "saboteur")
		// An operator's own row against a hand-created webhook's binding.
		hook := Subscription{ID: "hook-bound", TenantID: "t1", Repo: "acme/api", BotID: "hook-bot", Enabled: true,
			Origin: "operator", ForgeBinding: ForgeBindingForWebhook("wh-1")}
		bus := Subscription{ID: "bus", TenantID: "t1", Repo: "acme/api", BotID: "bus-bot", Enabled: true}
		conn := Subscription{ID: "conn", TenantID: "t1", BotID: "conn-bot", Invocation: bundle.InvocationKindConnector, Enabled: true,
			ConnectionID: "conn-a", TriggerID: "slack.message.posted"}
		connBound := Subscription{ID: "conn-bound", TenantID: "t1", BotID: "conn-bound-bot", Invocation: bundle.InvocationKindConnector, Enabled: true,
			ConnectionID: "conn-a", TriggerID: "slack.message.posted", ForgeBinding: ForgeBindingForWebhook("wh-1")}
		for _, sub := range []Subscription{scoped, wide, hook, bus, conn, connBound} {
			if err := st.Create(ctx, sub); err != nil {
				t.Fatal(err)
			}
		}
		// The bound rows are stored, enabled, and listed by their binding:
		// their absence below is the candidate query's, not the fixture's.
		if got := boundIDs(t, st, "t1", ForgeBindingForIntegration("int-1")); !reflect.DeepEqual(got, []string{"forge-scoped", "forge-wide"}) {
			t.Fatalf("the integration's binding holds %v, want both forge rows", got)
		}
		if got := boundIDs(t, st, "t1", ForgeBindingForWebhook("wh-1")); !reflect.DeepEqual(got, []string{"conn-bound", "hook-bound"}) {
			t.Fatalf("the webhook's binding holds %v, want both of its rows", got)
		}

		forge := Event{Source: SourceForge, TenantID: "t1", Repo: "acme/api", Kind: bundle.ForgeEventIssueLabeled, Action: ForgeIssueAction, AddedLabels: []string{"nestor"}}
		if got := sortedCandidateIDs(t, st, forge); !reflect.DeepEqual(got, []string{"bus"}) {
			t.Errorf("a forge event's bus candidates = %v, want [bus]: a bound row is selected by its binding only", got)
		}
		elsewhere := Event{Source: SourceBoard, TenantID: "t1", Repo: "acme/web", Kind: KindCardMoved}
		if got := sortedCandidateIDs(t, st, elsewhere); len(got) != 0 {
			t.Errorf("an event of another repository reached %v, want no candidate: a tenant-wide bound row is no candidate either", got)
		}
		intake := Event{Source: SourceConnector, TenantID: "t1", Kind: "slack.message.posted", ConnectionID: "conn-a", IngressID: "ing-1"}
		if got := sortedCandidateIDs(t, st, intake); !reflect.DeepEqual(got, []string{"conn"}) {
			t.Errorf("a connector intake's candidates = %v, want [conn]: a bound row is no connector candidate", got)
		}
	})
}

// A binding's rows are its tenant's and its own, enabled and disabled
// alike, in id order; an empty binding is refused, never read as the rows
// that carry none.
func TestListByForgeBinding(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		born := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
		at := func(sub Subscription) Subscription {
			born = born.Add(time.Second)
			sub.CreatedAt = born
			return sub
		}
		disabled := forgeRow("drv-a", "t1", "feature-dev", "int-1")
		disabled.Enabled = false
		disabled.DisabledReason = "declares issue_labeled with no label"
		// Created in an order that is not the id order, so the answer's order
		// is the store's sort and never the insertion's.
		for _, sub := range []Subscription{
			at(forgeRow("drv-c", "t1", "saboteur", "int-1", "saboteur")),
			at(forgeRow("drv-b", "t1", "nestor", "int-1", "nestor")),
			at(disabled),
			at(forgeRow("drv-d", "t1", "nestor", "int-2", "nestor")),
			at(forgeRow("drv-e", "t2", "nestor", "int-1", "nestor")),
			at(Subscription{ID: "hook", TenantID: "t1", BotID: "hook-bot", Enabled: true, Origin: "operator", ForgeBinding: ForgeBindingForWebhook("int-1")}),
			at(Subscription{ID: "bus", TenantID: "t1", BotID: "bus-bot", Enabled: true}),
		} {
			if err := st.Create(ctx, sub); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			name, tenant, binding string
			want                  []string
		}{
			{name: "an integration's rows, the disabled one included, in id order", tenant: "t1", binding: ForgeBindingForIntegration("int-1"), want: []string{"drv-a", "drv-b", "drv-c"}},
			{name: "another integration of the tenant", tenant: "t1", binding: ForgeBindingForIntegration("int-2"), want: []string{"drv-d"}},
			{name: "the same integration id in another tenant", tenant: "t2", binding: ForgeBindingForIntegration("int-1"), want: []string{"drv-e"}},
			{name: "a webhook sharing the integration's id", tenant: "t1", binding: ForgeBindingForWebhook("int-1"), want: []string{"hook"}},
			{name: "a binding nothing is bound to", tenant: "t1", binding: ForgeBindingForIntegration("int-9"), want: []string{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if got := boundIDs(t, st, tc.tenant, tc.binding); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("ListByForgeBinding(%s, %s) = %v, want %v", tc.tenant, tc.binding, got, tc.want)
				}
			})
		}
		t.Run("the rows come back whole", func(t *testing.T) {
			subs, err := st.ListByForgeBinding(ctx, "t1", ForgeBindingForIntegration("int-1"))
			if err != nil || len(subs) != 3 {
				t.Fatalf("ListByForgeBinding = %+v, %v", subs, err)
			}
			got := subs[0]
			if got.Enabled || got.DisabledReason != disabled.DisabledReason || got.ForgeBinding != disabled.ForgeBinding || got.Origin != disabled.Origin {
				t.Fatalf("the disabled row came back as %+v", got)
			}
			if !reflect.DeepEqual(subs[1].Match.AddedLabels, []string{"nestor"}) {
				t.Fatalf("nestor's matcher came back as %+v", subs[1].Match)
			}
		})
		t.Run("an empty binding is refused", func(t *testing.T) {
			subs, err := st.ListByForgeBinding(ctx, "t1", "")
			if !errors.Is(err, ErrEmptyForgeBinding) {
				t.Fatalf("ListByForgeBinding(t1, \"\") = %v, %v, want ErrEmptyForgeBinding", subscriptionIDs(subs), err)
			}
			if len(subs) != 0 {
				t.Fatalf("an empty binding selected %v: the rows that carry no binding are the bus's", subscriptionIDs(subs))
			}
		})
	})
}

// Every store write holds the invariant of a projected forge row: an origin
// of a forge integration requires the binding of that same integration. The
// refusal is typed, and a refused write leaves the store as it was.
func TestForgeOriginRequiresItsBinding(t *testing.T) {
	onSubscriptionTwins(t, func(t *testing.T, st SubscriptionStore) {
		ctx := context.Background()
		refused := func(t *testing.T, err error, field string) {
			t.Helper()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			var inv *InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("err = %T %v, want an *InvalidError", err, err)
			}
			if inv.Field != field {
				t.Fatalf("the refusal names field %q, want %q (%v)", inv.Field, field, err)
			}
		}
		absent := func(t *testing.T, id string) {
			t.Helper()
			if got, err := st.Get(ctx, id); !errors.Is(err, ErrSubscriptionNotFound) {
				t.Fatalf("a refused write left %+v, %v", got, err)
			}
		}

		for _, tc := range []struct {
			name, origin, binding, field string
		}{
			{name: "no binding", origin: ForgeOrigin("int-1"), field: "forge_binding"},
			{name: "another integration's binding", origin: ForgeOrigin("int-1"), binding: ForgeBindingForIntegration("int-2"), field: "forge_binding"},
			{name: "a webhook's binding of the same id", origin: ForgeOrigin("int-1"), binding: ForgeBindingForWebhook("int-1"), field: "forge_binding"},
			{name: "the integration id without its prefix", origin: ForgeOrigin("int-1"), binding: "int-1", field: "forge_binding"},
			{name: "an origin naming no integration", origin: ForgeOrigin(""), binding: ForgeBindingForIntegration(""), field: "origin"},
		} {
			t.Run("create: "+tc.name, func(t *testing.T) {
				err := st.Create(ctx, Subscription{ID: "refused", TenantID: "t1", BotID: "nestor", Enabled: true, Origin: tc.origin, ForgeBinding: tc.binding})
				refused(t, err, tc.field)
				absent(t, "refused")
			})
			t.Run("ensure derived: "+tc.name, func(t *testing.T) {
				id := DerivedForgeSubscriptionID("t1", "nestor", "refused", "int-1")
				created, err := st.EnsureDerived(ctx, Subscription{ID: id, TenantID: "t1", BotID: "nestor", Enabled: true, Origin: tc.origin, ForgeBinding: tc.binding})
				refused(t, err, tc.field)
				if created {
					t.Fatal("a refused row was reported created")
				}
				absent(t, id)
			})
		}

		// The matching pair is written, by both creating writes.
		if err := st.Create(ctx, forgeRow("kept", "t1", "nestor", "int-1", "nestor")); err != nil {
			t.Fatalf("a row bound to its origin's integration was refused: %v", err)
		}
		derivedID := DerivedForgeSubscriptionID("t1", "saboteur", "forge:issue_labeled|a:|l:0", "int-1")
		created, err := st.EnsureDerived(ctx, forgeRow(derivedID, "t1", "saboteur", "int-1", "saboteur"))
		if err != nil || !created {
			t.Fatalf("EnsureDerived = %t, %v, want the row created", created, err)
		}
		if got := boundIDs(t, st, "t1", ForgeBindingForIntegration("int-1")); len(got) != 2 {
			t.Fatalf("the binding holds %v, want both rows", got)
		}

		t.Run("an update cannot unbind the row", func(t *testing.T) {
			cur, err := st.Get(ctx, "kept")
			if err != nil {
				t.Fatal(err)
			}
			for name, edit := range map[string]func(*Subscription){
				"the binding blanked":              func(s *Subscription) { s.ForgeBinding = "" },
				"the binding moved":                func(s *Subscription) { s.ForgeBinding = ForgeBindingForIntegration("int-2") },
				"the origin moved without binding": func(s *Subscription) { s.Origin = ForgeOrigin("int-2") },
			} {
				next := cur
				edit(&next)
				refused(t, st.Update(ctx, next), "forge_binding")
				got, err := st.Get(ctx, "kept")
				if err != nil {
					t.Fatal(err)
				}
				if got.ForgeBinding != cur.ForgeBinding || got.Origin != cur.Origin || got.Version != cur.Version {
					t.Fatalf("%s: the refused update left binding %q origin %q version %d, want %q %q %d",
						name, got.ForgeBinding, got.Origin, got.Version, cur.ForgeBinding, cur.Origin, cur.Version)
				}
			}
			// The control: an update that keeps the pair lands.
			cur.Enabled = false
			cur.DisabledReason = "parked by its operator"
			if err := st.Update(ctx, cur); err != nil {
				t.Fatalf("an update keeping the pair was refused: %v", err)
			}
			got, _ := st.Get(ctx, "kept")
			if got.Enabled || got.DisabledReason != "parked by its operator" || got.ForgeBinding != ForgeBindingForIntegration("int-1") {
				t.Fatalf("the update landed as %+v", got)
			}
		})

		t.Run("a binding needs no forge origin", func(t *testing.T) {
			// An operator's row against an integration's binding, and a row
			// of a hand-created webhook: bound, and not the projector's.
			for _, sub := range []Subscription{
				{ID: "operator-row", TenantID: "t1", BotID: "nestor", Enabled: true, Origin: "operator", ForgeBinding: ForgeBindingForIntegration("int-1")},
				{ID: "webhook-row", TenantID: "t1", BotID: "nestor", Enabled: true, Origin: ForgeBindingForWebhook("wh-1"), ForgeBinding: ForgeBindingForWebhook("wh-1")},
				{ID: "plain-row", TenantID: "t1", BotID: "nestor", Enabled: true, Origin: "operator"},
			} {
				if err := st.Create(ctx, sub); err != nil {
					t.Fatalf("create %s: %v", sub.ID, err)
				}
			}
		})

		t.Run("ensuring an existing row rewrites nothing", func(t *testing.T) {
			before, err := st.Get(ctx, derivedID)
			if err != nil {
				t.Fatal(err)
			}
			again := forgeRow(derivedID, "t1", "saboteur", "int-1", "other-label")
			again.ProjectionDigest = "another-digest"
			created, err := st.EnsureDerived(ctx, again)
			if err != nil || created {
				t.Fatalf("EnsureDerived on an existing row = %t, %v", created, err)
			}
			after, err := st.Get(ctx, derivedID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("EnsureDerived rewrote the row:\n%+v\nwas\n%+v", after, before)
			}
		})
	})
}

// The forge router's query has its index: tenant, binding, enabled, in that
// order, on the Mongo twin.
func TestForgeBindingIndexIsDeclared(t *testing.T) {
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
	db := client.Database("iterion_sub_forge_index_" + bson.NewObjectID().Hex())
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
		if ix.Name != "tenant_forge_binding" {
			continue
		}
		var keys []string
		for _, e := range ix.Key {
			keys = append(keys, e.Key)
		}
		if !reflect.DeepEqual(keys, []string{"tenant_id", "forge_binding", "enabled"}) {
			t.Fatalf("index keys = %v, want [tenant_id forge_binding enabled]", keys)
		}
		return
	}
	t.Fatalf("no tenant_forge_binding index among %+v", idx)
}

// The unbound rows a Mongo candidate query keeps are the ones whose binding
// is absent, empty or null: a document written before the field existed
// stays a bus candidate.
func TestMongoListCandidates_KeepsRowsWrittenBeforeTheBinding(t *testing.T) {
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
	db := client.Database("iterion_sub_forge_legacy_" + bson.NewObjectID().Hex())
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
	if _, err := db.Collection(SubscriptionsCollectionName).InsertMany(ctx, []any{
		bson.M{"_id": "absent", "tenant_id": "t1", "enabled": true},
		bson.M{"_id": "empty", "tenant_id": "t1", "enabled": true, "forge_binding": ""},
		bson.M{"_id": "null", "tenant_id": "t1", "enabled": true, "forge_binding": nil},
		bson.M{"_id": "bound", "tenant_id": "t1", "enabled": true, "forge_binding": ForgeBindingForIntegration("int-1")},
	}); err != nil {
		t.Fatal(err)
	}
	got := sortedCandidateIDs(t, st, Event{Source: SourceForge, TenantID: "t1", Repo: "acme/api"})
	if !reflect.DeepEqual(got, []string{"absent", "empty", "null"}) {
		t.Fatalf("candidates = %v, want the three unbound spellings and never the bound row", got)
	}
}
