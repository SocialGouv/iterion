package orgsweep

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/identity"
)

func TestNextRun(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	// Later today → same day at the hour.
	if got := nextRun(now, 14); got.Hour() != 14 || got.Day() != 29 {
		t.Errorf("nextRun(10:00, 14) = %v, want 2026-06-29T14:00", got)
	}
	// Earlier today → tomorrow at the hour.
	if got := nextRun(now, 2); got.Hour() != 2 || got.Day() != 30 {
		t.Errorf("nextRun(10:00, 2) = %v, want 2026-06-30T02:00", got)
	}
	// Out-of-range hour falls back to 2.
	if got := nextRun(now, 99); got.Hour() != 2 {
		t.Errorf("nextRun(_, 99) hour = %d, want 2", got.Hour())
	}
}

// TestPurgeOrg_Mongo proves PurgeOrg removes the target org's data across the
// cloud collections AND the identity records, while leaving a SECOND org's data
// fully intact (no over-deletion). Gated on a real Mongo like the other cloud
// suites (CI mongo-conformance sets ITERION_TEST_MONGO_URI; local: task cloud:up:deps).
func TestPurgeOrg_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo orgsweep suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_orgsweep_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})

	st := identity.NewMongoStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	// Two orgs, one team each. We'll purge A and assert B survives untouched.
	seedOrgTeam := func(tag string) (orgID, teamID string) {
		orgID, teamID = "org-"+tag, "team-"+tag
		if _, err := st.CreateOrg(ctx, identity.Org{ID: orgID, Name: "Org " + tag, Slug: "org-" + tag}); err != nil {
			t.Fatalf("CreateOrg %s: %v", tag, err)
		}
		if _, err := st.CreateTeam(ctx, identity.Team{ID: teamID, OrgID: orgID, Name: "Team " + tag, Slug: "team-" + tag}); err != nil {
			t.Fatalf("CreateTeam %s: %v", tag, err)
		}
		return
	}
	orgA, teamA := seedOrgTeam("a")
	orgB, teamB := seedOrgTeam("b")

	ins := func(coll string, doc bson.M) {
		if _, err := db.Collection(coll).InsertOne(ctx, doc); err != nil {
			t.Fatalf("insert %s: %v", coll, err)
		}
	}
	// Team-scoped, both tenant_id and team_id key variants.
	ins("runs", bson.M{"_id": "r-a1", "tenant_id": teamA})
	ins("runs", bson.M{"_id": "r-a2", "team_id": teamA})
	ins("runs", bson.M{"_id": "r-b1", "tenant_id": teamB})
	ins("forge_connections", bson.M{"_id": "fc-a", "tenant_id": teamA})
	ins("forge_connections", bson.M{"_id": "fc-b", "tenant_id": teamB})
	ins("api_keys", bson.M{"_id": "k-a", "tenant_id": teamA})
	ins("api_keys", bson.M{"_id": "k-b", "tenant_id": teamB})
	// Org-scoped.
	ins("audit_events", bson.M{"_id": "au-a", "tenant_id": orgA})
	ins("audit_events", bson.M{"_id": "au-b", "tenant_id": orgB})
	ins("org_usage", bson.M{"_id": "org|" + orgA + "|2026-06", "org_id": orgA})
	ins("org_usage", bson.M{"_id": "org|" + orgB + "|2026-06", "org_id": orgB})

	cascade := func(ctx context.Context, orgID string) error {
		teams, err := st.ListTeamsByOrg(ctx, orgID)
		if err != nil {
			return err
		}
		for _, tm := range teams {
			if err := st.DeleteTeam(ctx, tm.ID); err != nil {
				return err
			}
		}
		return st.DeleteOrg(ctx, orgID)
	}

	p := &Purger{DB: db, Store: st, Cascade: cascade}
	if _, err := p.PurgeOrg(ctx, orgA); err != nil {
		t.Fatalf("PurgeOrg(A): %v", err)
	}

	count := func(coll string, filter bson.M) int64 {
		n, err := db.Collection(coll).CountDocuments(ctx, filter)
		if err != nil {
			t.Fatalf("count %s: %v", coll, err)
		}
		return n
	}
	teamFilter := func(id string) bson.M {
		return bson.M{"$or": bson.A{bson.M{"tenant_id": id}, bson.M{"team_id": id}}}
	}

	// A's data is gone…
	if n := count("runs", teamFilter(teamA)); n != 0 {
		t.Errorf("runs for team A: got %d, want 0", n)
	}
	if n := count("forge_connections", teamFilter(teamA)); n != 0 {
		t.Errorf("forge_connections for team A: got %d, want 0", n)
	}
	if n := count("api_keys", teamFilter(teamA)); n != 0 {
		t.Errorf("api_keys for team A: got %d, want 0", n)
	}
	if n := count("audit_events", bson.M{"tenant_id": orgA}); n != 0 {
		t.Errorf("audit for org A: got %d, want 0", n)
	}
	if n := count("org_usage", bson.M{"org_id": orgA}); n != 0 {
		t.Errorf("org_usage for org A: got %d, want 0", n)
	}
	if _, err := st.GetOrg(ctx, orgA); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("GetOrg(A) err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetTeam(ctx, teamA); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("GetTeam(A) err = %v, want ErrNotFound", err)
	}

	// …and B's data is fully intact (no over-deletion).
	if n := count("runs", teamFilter(teamB)); n != 1 {
		t.Errorf("runs for team B: got %d, want 1", n)
	}
	if n := count("forge_connections", teamFilter(teamB)); n != 1 {
		t.Errorf("forge_connections for team B: got %d, want 1", n)
	}
	if n := count("api_keys", teamFilter(teamB)); n != 1 {
		t.Errorf("api_keys for team B: got %d, want 1", n)
	}
	if n := count("audit_events", bson.M{"tenant_id": orgB}); n != 1 {
		t.Errorf("audit for org B: got %d, want 1", n)
	}
	if n := count("org_usage", bson.M{"org_id": orgB}); n != 1 {
		t.Errorf("org_usage for org B: got %d, want 1", n)
	}
	if _, err := st.GetOrg(ctx, orgB); err != nil {
		t.Errorf("GetOrg(B) err = %v, want nil", err)
	}

	// Idempotent: purging A again is a clean no-op.
	if n, err := p.PurgeOrg(ctx, orgA); err != nil || n != 0 {
		t.Errorf("PurgeOrg(A) second call = (%d, %v), want (0, nil)", n, err)
	}
}
