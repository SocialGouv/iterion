package trigger

import (
	"os"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestMongoSubscriptionStoreRepoQueriesIncludeWorkspaceWide(t *testing.T) {
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
	db := client.Database("iterion_sub_queries_" + bson.NewObjectID().Hex())
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
	for _, sub := range []Subscription{
		{ID: "repo", TenantID: "t1", Repo: "acme/api", Enabled: true},
		{ID: "wide", TenantID: "t1", Enabled: true},
		{ID: "disabled", TenantID: "t1"},
		{ID: "other-repo", TenantID: "t1", Repo: "acme/web", Enabled: true},
		{ID: "other-tenant", TenantID: "t2", Enabled: true},
	} {
		if err := st.Create(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	coll := db.Collection(SubscriptionsCollectionName)
	var stored bson.M
	if err := coll.FindOne(ctx, bson.M{"_id": "wide"}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if _, present := stored["repo"]; present {
		t.Fatal("fixture must exercise the omitted repo written by Create")
	}
	// Older documents can carry the empty field explicitly. Both spellings,
	// plus BSON null's zero string value, describe the same workspace scope.
	if _, err := coll.InsertMany(ctx, []any{
		bson.M{"_id": "empty", "tenant_id": "t1", "repo": "", "enabled": true},
		bson.M{"_id": "null", "tenant_id": "t1", "repo": nil, "enabled": true},
	}); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, got []Subscription, err error, want []string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(got))
		for _, sub := range got {
			ids = append(ids, sub.ID)
		}
		slices.Sort(ids)
		slices.Sort(want)
		if !slices.Equal(ids, want) {
			t.Fatalf("subscription IDs = %v, want %v", ids, want)
		}
	}
	for _, repo := range []string{"acme/api", "unknown/repo", ""} {
		t.Run("repo="+repo, func(t *testing.T) {
			want := []string{"empty", "null", "wide"}
			if repo == "acme/api" {
				want = append(want, "repo")
			}
			got, err := st.ListCandidates(ctx, Event{TenantID: "t1", Repo: repo})
			check(t, got, err, slices.Clone(want))
			got, err = st.ListByRepo(ctx, "t1", repo)
			check(t, got, err, append(want, "disabled"))
		})
	}
	got, err := st.ListCandidates(ctx, Event{TenantID: "t2", Repo: "acme/api"})
	check(t, got, err, []string{"other-tenant"})
}
