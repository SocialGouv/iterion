package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// runPatchSuite exercises the PatchTeam/PatchOrg contract both Store
// implementations must satisfy. It matters more than it looks: the two
// twins reach the same result by opposite means — the memory store
// mutates a copied struct, Mongo builds a `$set` of hand-written keys
// — so a field name that does not match its bson tag would write a NEW
// document key and update nothing, silently and without an error. Only
// a suite that runs against BOTH can see that.
func runPatchSuite(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	seedOrg := func(id, slug string) {
		t.Helper()
		if _, err := s.CreateOrg(ctx, Org{ID: id, Name: id, Slug: slug, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed org %s: %v", id, err)
		}
	}
	seedTeam := func(id, orgID, slug string) {
		t.Helper()
		if _, err := s.CreateTeam(ctx, Team{ID: id, OrgID: orgID, Name: id, Slug: slug, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed team %s: %v", id, err)
		}
	}

	seedOrg("po1", "patch-org-one")
	seedOrg("po2", "patch-org-two")
	seedTeam("pt1", "po1", "patch-team-one")
	seedTeam("pt2", "po1", "patch-team-two")

	t.Run("team: a named field moves and the others do not", func(t *testing.T) {
		name := "Renamed"
		got, err := s.PatchTeam(ctx, "pt1", TeamPatch{Name: &name})
		if err != nil {
			t.Fatalf("PatchTeam: %v", err)
		}
		if got.Name != "Renamed" {
			t.Fatalf("name not applied: %+v", got)
		}
		// The whole point of a patch: an untouched field survives.
		if got.Slug != "patch-team-one" || got.OrgID != "po1" {
			t.Fatalf("patch clobbered an unnamed field: %+v", got)
		}
	})

	t.Run("team: suspending stamps the trio, restoring clears it", func(t *testing.T) {
		susp := TeamStatusSuspended
		got, err := s.PatchTeam(ctx, "pt1", TeamPatch{
			Status: &susp, SuspendedBy: "admin-1", SuspendReason: "unpaid",
		})
		if err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if got.Status != TeamStatusSuspended {
			t.Fatalf("status not applied: %+v", got)
		}
		if got.SuspendedAt == nil || got.SuspendedBy != "admin-1" || got.SuspendReason != "unpaid" {
			t.Fatalf("suspension trio not stamped: at=%v by=%q reason=%q",
				got.SuspendedAt, got.SuspendedBy, got.SuspendReason)
		}
		// The subtle half, and the one a memory-only test cannot see:
		// Mongo clears these by writing an explicit null / "" through
		// $set, which must decode back to the zero value.
		active := TeamStatusActive
		got, err = s.PatchTeam(ctx, "pt1", TeamPatch{Status: &active})
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		if got.Status != TeamStatusActive {
			t.Fatalf("status not restored: %+v", got)
		}
		if got.SuspendedAt != nil || got.SuspendedBy != "" || got.SuspendReason != "" {
			t.Fatalf("stale suspension survived the restore: at=%v by=%q reason=%q",
				got.SuspendedAt, got.SuspendedBy, got.SuspendReason)
		}
	})

	t.Run("team: a taken slug is refused, not silently applied", func(t *testing.T) {
		taken := "patch-team-two"
		if _, err := s.PatchTeam(ctx, "pt1", TeamPatch{Slug: &taken}); !errors.Is(err, ErrSlugAlreadyTaken) {
			t.Fatalf("want ErrSlugAlreadyTaken, got %v", err)
		}
		got, err := s.GetTeam(ctx, "pt1")
		if err != nil {
			t.Fatalf("GetTeam: %v", err)
		}
		if got.Slug != "patch-team-one" {
			t.Fatalf("refused patch still moved the slug: %q", got.Slug)
		}
	})

	t.Run("team: unknown id is ErrNotFound", func(t *testing.T) {
		name := "ghost"
		if _, err := s.PatchTeam(ctx, "nope", TeamPatch{Name: &name}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("team: an empty patch reads back rather than erroring", func(t *testing.T) {
		got, err := s.PatchTeam(ctx, "pt2", TeamPatch{})
		if err != nil {
			t.Fatalf("empty PatchTeam: %v", err)
		}
		if got.ID != "pt2" || got.Slug != "patch-team-two" {
			t.Fatalf("empty patch did not read back the team: %+v", got)
		}
	})

	t.Run("org: the governance fields round-trip", func(t *testing.T) {
		req := true
		scope := ProvisionApprovalSharedCredentials
		// A nested struct through $set: the driver marshals it, and
		// nothing else in the suite would notice if it came back empty.
		aud := CredentialAudience{Teams: []string{"pt1", "pt2"}}
		got, err := s.PatchOrg(ctx, "po1", OrgPatch{
			RequireProvisionApproval: &req,
			ProvisionApprovalScope:   &scope,
			CredentialAudience:       &aud,
		})
		if err != nil {
			t.Fatalf("PatchOrg: %v", err)
		}
		if !got.RequireProvisionApproval {
			t.Fatalf("require_provision_approval not applied: %+v", got)
		}
		if got.ProvisionApprovalScope != ProvisionApprovalSharedCredentials {
			t.Fatalf("scope not applied: %q", got.ProvisionApprovalScope)
		}
		if len(got.CredentialAudience.Teams) != 2 ||
			!got.CredentialAudience.Allows("pt1") || !got.CredentialAudience.Allows("pt2") {
			t.Fatalf("audience did not round-trip: %+v", got.CredentialAudience)
		}
		// An audience that admits nobody must persist as such — the zero
		// value is load-bearing (lending is an explicit act), so it must
		// not be indistinguishable from "never written".
		empty := CredentialAudience{}
		got, err = s.PatchOrg(ctx, "po1", OrgPatch{CredentialAudience: &empty})
		if err != nil {
			t.Fatalf("PatchOrg(empty audience): %v", err)
		}
		if got.CredentialAudience.Allows("pt1") {
			t.Fatalf("cleared audience still admits a team: %+v", got.CredentialAudience)
		}
		// And the neighbouring field it did not name stayed put.
		if !got.RequireProvisionApproval {
			t.Fatalf("audience patch clobbered require_provision_approval: %+v", got)
		}
	})

	t.Run("org: a taken slug is refused", func(t *testing.T) {
		taken := "patch-org-two"
		if _, err := s.PatchOrg(ctx, "po1", OrgPatch{Slug: &taken}); !errors.Is(err, ErrOrgSlugAlreadyTaken) {
			t.Fatalf("want ErrOrgSlugAlreadyTaken, got %v", err)
		}
	})

	t.Run("org: unknown id is ErrNotFound", func(t *testing.T) {
		name := "ghost"
		if _, err := s.PatchOrg(ctx, "nope", OrgPatch{Name: &name}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

func TestMemoryStore_Patch(t *testing.T) {
	runPatchSuite(t, NewMemoryStore())
}

// TestMongoStore_Patch runs the shared patch suite against a real Mongo,
// gated on ITERION_TEST_MONGO_URI like the sibling bulk-get suite. The
// mongo-conformance CI job already runs ./pkg/identity/..., so this is
// the half that makes the Mongo $set paths executable at all.
func TestMongoStore_Patch(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo identity patch suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_identity_patch_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	st := NewMongoStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	runPatchSuite(t, st)
}
