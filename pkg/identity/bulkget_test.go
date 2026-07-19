package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// runBulkGetSuite exercises the Get*ByIDs contract shared by both Store
// implementations: found entries keyed by id, missing ids absent from
// the map (never an error), duplicate ids collapsed, empty input → an
// empty non-nil map.
func runBulkGetSuite(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	for i, u := range []User{
		{ID: "u1", Email: "u1@example.com", Name: "One", Status: UserStatusActive},
		{ID: "u2", Email: "u2@example.com", Name: "Two", Status: UserStatusActive},
	} {
		u.CreatedAt = now.Add(time.Duration(i) * time.Second)
		u.UpdatedAt = u.CreatedAt
		if _, err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("seed user %s: %v", u.ID, err)
		}
	}
	for i, o := range []Org{
		{ID: "o1", Name: "Org One", Slug: "org-one"},
		{ID: "o2", Name: "Org Two", Slug: "org-two"},
	} {
		o.CreatedAt = now.Add(time.Duration(i) * time.Second)
		o.UpdatedAt = o.CreatedAt
		if _, err := s.CreateOrg(ctx, o); err != nil {
			t.Fatalf("seed org %s: %v", o.ID, err)
		}
	}
	for i, tm := range []Team{
		{ID: "t1", OrgID: "o1", Name: "Team One", Slug: "team-one"},
		{ID: "t2", OrgID: "o2", Name: "Team Two", Slug: "team-two"},
	} {
		tm.CreatedAt = now.Add(time.Duration(i) * time.Second)
		tm.UpdatedAt = tm.CreatedAt
		if _, err := s.CreateTeam(ctx, tm); err != nil {
			t.Fatalf("seed team %s: %v", tm.ID, err)
		}
	}

	t.Run("users", func(t *testing.T) {
		got, err := s.GetUsersByIDs(ctx, []string{"u2", "missing", "u1", "u2"})
		if err != nil {
			t.Fatalf("GetUsersByIDs: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d users, want 2: %v", len(got), got)
		}
		if got["u1"].Name != "One" || got["u1"].Email != "u1@example.com" {
			t.Fatalf("u1 mismatch: %+v", got["u1"])
		}
		if got["u2"].Name != "Two" {
			t.Fatalf("u2 mismatch: %+v", got["u2"])
		}
		if _, ok := got["missing"]; ok {
			t.Fatalf("missing id must be absent, got %+v", got["missing"])
		}
	})

	t.Run("teams", func(t *testing.T) {
		got, err := s.GetTeamsByIDs(ctx, []string{"t1", "t2", "gone"})
		if err != nil {
			t.Fatalf("GetTeamsByIDs: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d teams, want 2: %v", len(got), got)
		}
		if got["t1"].Slug != "team-one" || got["t2"].OrgID != "o2" {
			t.Fatalf("team mismatch: %+v", got)
		}
	})

	t.Run("orgs", func(t *testing.T) {
		got, err := s.GetOrgsByIDs(ctx, []string{"o2", "nope"})
		if err != nil {
			t.Fatalf("GetOrgsByIDs: %v", err)
		}
		if len(got) != 1 || got["o2"].Slug != "org-two" {
			t.Fatalf("org mismatch: %v", got)
		}
	})

	t.Run("empty ids", func(t *testing.T) {
		got, err := s.GetUsersByIDs(ctx, nil)
		if err != nil {
			t.Fatalf("GetUsersByIDs(nil): %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("want empty non-nil map, got %v", got)
		}
	})

	t.Run("all missing", func(t *testing.T) {
		got, err := s.GetOrgsByIDs(ctx, []string{"x", "y"})
		if err != nil {
			t.Fatalf("GetOrgsByIDs: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("want empty map, got %v", got)
		}
	})
}

func TestMemoryStore_BulkGets(t *testing.T) {
	runBulkGetSuite(t, NewMemoryStore())
}

// TestMongoStore_BulkGets runs the shared bulk-get suite against a real
// Mongo (same ITERION_TEST_MONGO_URI gating as the pkg/store/mongo
// conformance harness and the sibling pat/audit/orgusage suites).
func TestMongoStore_BulkGets(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo identity bulk-get suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_identity_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	st := NewMongoStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	runBulkGetSuite(t, st)
}
