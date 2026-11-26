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

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// runInvitationOrgRoleSuite holds both stores to the same reading of an
// invitation's org role: the role a row was written with comes back on every
// read path, and a row written without one (minted before invitations named
// their org role) reads as naming none, which OrgRoleOnAccept answers as an
// org member whatever the team rank.
func runInvitationOrgRoleSuite(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	const team = "team-org-role"
	rows := []struct {
		inv  Invitation
		want OrgRole
	}{
		{Invitation{ID: "inv-named-admin", TeamID: team, Email: "a@example.org", Role: RoleMember, OrgRole: OrgRoleAdmin, TokenHash: "h-named-admin"}, OrgRoleAdmin},
		{Invitation{ID: "inv-legacy-admin", TeamID: team, Email: "b@example.org", Role: RoleAdmin, TokenHash: "h-legacy-admin"}, OrgRoleMember},
		{Invitation{ID: "inv-config-editor", TeamID: team, Email: "c@example.org", Role: RoleConfigEditor, TokenHash: "h-config-editor"}, ""},
	}
	for _, r := range rows {
		inv := r.inv
		inv.InvitedBy, inv.CreatedAt, inv.ExpiresAt = "inviter", now, now.Add(time.Hour)
		if err := s.CreateInvitation(ctx, inv); err != nil {
			t.Fatalf("CreateInvitation(%s): %v", inv.ID, err)
		}
	}
	listed, err := s.ListInvitationsByTeam(ctx, team)
	if err != nil {
		t.Fatalf("ListInvitationsByTeam: %v", err)
	}
	byID := make(map[string]Invitation, len(listed))
	for _, inv := range listed {
		byID[inv.ID] = inv
	}
	for _, r := range rows {
		got, err := s.GetInvitation(ctx, r.inv.ID)
		if err != nil {
			t.Fatalf("GetInvitation(%s): %v", r.inv.ID, err)
		}
		byHash, err := s.GetInvitationByTokenHash(ctx, r.inv.TokenHash)
		if err != nil {
			t.Fatalf("GetInvitationByTokenHash(%s): %v", r.inv.TokenHash, err)
		}
		for path, inv := range map[string]Invitation{"GetInvitation": got, "GetInvitationByTokenHash": byHash, "ListInvitationsByTeam": byID[r.inv.ID]} {
			if inv.OrgRole != r.inv.OrgRole {
				t.Errorf("%s(%s).OrgRole = %q, want %q as written", path, r.inv.ID, inv.OrgRole, r.inv.OrgRole)
			}
			if role := inv.OrgRoleOnAccept(); role != r.want {
				t.Errorf("%s(%s).OrgRoleOnAccept() = %q, want %q", path, r.inv.ID, role, r.want)
			}
		}
	}
}

// runInvitationsMintedWithSuite holds both stores to the same selection of
// what revoking a personal access token ends: the pending invitations it
// minted, in every team — not an accepted one, not another token's, not a
// session's.
func runInvitationsMintedWithSuite(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	accepted := now
	for _, inv := range []Invitation{
		{ID: "pat-a-team1", TeamID: "t1", Email: "a@example.org", MintedWith: "pat-a", TokenHash: "h1"},
		{ID: "pat-a-team2", TeamID: "t2", Email: "b@example.org", MintedWith: "pat-a", TokenHash: "h2"},
		{ID: "pat-a-accepted", TeamID: "t1", Email: "c@example.org", MintedWith: "pat-a", TokenHash: "h3", AcceptedAt: &accepted, AcceptedBy: "u"},
		{ID: "pat-b", TeamID: "t1", Email: "d@example.org", MintedWith: "pat-b", TokenHash: "h4"},
		{ID: "session", TeamID: "t1", Email: "e@example.org", TokenHash: "h5"},
	} {
		inv.Role, inv.InvitedBy, inv.CreatedAt, inv.ExpiresAt = RoleMember, "inviter", now, now.Add(time.Hour)
		if err := s.CreateInvitation(ctx, inv); err != nil {
			t.Fatalf("CreateInvitation(%s): %v", inv.ID, err)
		}
	}
	got, err := s.ListPendingInvitationsMintedWith(ctx, "pat-a")
	if err != nil {
		t.Fatalf("ListPendingInvitationsMintedWith: %v", err)
	}
	ids := map[string]bool{}
	for _, inv := range got {
		ids[inv.ID] = true
	}
	if len(got) != 2 || !ids["pat-a-team1"] || !ids["pat-a-team2"] {
		t.Fatalf("pending invitations minted with pat-a = %v, want pat-a-team1 and pat-a-team2", ids)
	}
	if _, err := s.ListPendingInvitationsMintedWith(ctx, ""); err == nil {
		t.Fatal("an empty credential id selected invitations instead of refusing")
	}
}

func TestMemoryStore_InvitationsMintedWith(t *testing.T) {
	runInvitationsMintedWithSuite(t, NewMemoryStore())
}

func TestMemoryStore_InvitationOrgRole(t *testing.T) {
	runInvitationOrgRoleSuite(t, NewMemoryStore())
}

// TestMongoStore_InvitationOrgRole runs the suite against a real Mongo, gated
// on ITERION_TEST_MONGO_URI like the sibling suites.
func TestMongoStore_InvitationOrgRole(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo invitation org-role suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_identity_invorgrole_" + hex.EncodeToString(nonce))
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
	runInvitationOrgRoleSuite(t, st)
	runInvitationsMintedWithSuite(t, st)
}
