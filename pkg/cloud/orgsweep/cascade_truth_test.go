package orgsweep

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// cascadeWitnesses count, per identity collection, the rows of the seeded
// org (or its user) that are still there after the cascade. A row of
// purgedByCascade with no witness here cannot be proven and fails; a witness
// for a collection in notPurged proves the other direction — the rows the
// purge says it leaves are left.
var cascadeWitnesses = map[string]func(ctx context.Context, st identity.Store, f cascadeFixture) (int, error){
	"orgs": func(ctx context.Context, st identity.Store, f cascadeFixture) (int, error) {
		return found(st.GetOrg(ctx, f.orgID))
	},
	"teams": func(ctx context.Context, st identity.Store, f cascadeFixture) (int, error) {
		teams, err := st.ListTeamsByOrg(ctx, f.orgID)
		return len(teams), err
	},
	"memberships": func(ctx context.Context, st identity.Store, f cascadeFixture) (int, error) {
		m, err := st.ListMembershipsByTeam(ctx, f.teamID)
		return len(m), err
	},
	"org_memberships": func(ctx context.Context, st identity.Store, f cascadeFixture) (int, error) {
		m, err := st.ListOrgMembershipsByOrg(ctx, f.orgID)
		return len(m), err
	},
	"invitations": func(ctx context.Context, st identity.Store, f cascadeFixture) (int, error) {
		inv, err := st.ListInvitationsByTeam(ctx, f.teamID)
		return len(inv), err
	},
	"users": func(ctx context.Context, st identity.Store, f cascadeFixture) (int, error) {
		return found(st.GetUser(ctx, f.userID))
	},
	"oidc_links": func(ctx context.Context, st identity.Store, f cascadeFixture) (int, error) {
		links, err := st.ListOIDCLinksByUser(ctx, f.userID)
		return len(links), err
	},
}

// found turns a Get into a row count: 1 when the row is there, 0 on
// ErrNotFound, and the error otherwise.
func found[T any](_ T, err error) (int, error) {
	switch {
	case err == nil:
		return 1, nil
	case errors.Is(err, identity.ErrNotFound):
		return 0, nil
	default:
		return 0, err
	}
}

type cascadeFixture struct {
	orgID, teamID, userID string
}

// seedCascadeFixture writes one row into every collection a witness reads:
// an org, its team, a user who is a member of both, a pending invitation to
// the team and the user's SSO link.
func seedCascadeFixture(t *testing.T, st identity.Store) cascadeFixture {
	t.Helper()
	ctx := context.Background()
	f := cascadeFixture{orgID: "org-cascade", teamID: "team-cascade", userID: "user-cascade"}
	if _, err := st.CreateUser(ctx, identity.User{ID: f.userID, Email: "member@example.org", Name: "Member"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := st.CreateOrg(ctx, identity.Org{ID: f.orgID, Name: "Cascade", Slug: "cascade"}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := st.CreateTeam(ctx, identity.Team{ID: f.teamID, OrgID: f.orgID, Name: "Cascade", Slug: "cascade"}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := st.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: f.userID, OrgID: f.orgID, Role: identity.OrgRoleOwner, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertOrgMembership: %v", err)
	}
	if err := st.UpsertMembership(ctx, identity.Membership{UserID: f.userID, TeamID: f.teamID, Role: identity.RoleOwner, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertMembership: %v", err)
	}
	if err := st.CreateInvitation(ctx, identity.Invitation{ID: "inv-cascade", TeamID: f.teamID, Email: "invited@example.org", Role: identity.RoleAdmin, TokenHash: "h", InvitedBy: f.userID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	if err := st.UpsertOIDCLink(ctx, identity.OIDCLink{Provider: "idp", ProviderUserID: "sub-cascade", UserID: f.userID, Email: "member@example.org"}); err != nil {
		t.Fatalf("UpsertOIDCLink: %v", err)
	}
	// Every witness must see its row BEFORE the cascade, or an empty
	// collection would read as "purged".
	for name, witness := range cascadeWitnesses {
		n, err := witness(ctx, st, f)
		if err != nil {
			t.Fatalf("witness %s before the cascade: %v", name, err)
		}
		if n == 0 {
			t.Fatalf("witness %s sees no row before the cascade — the fixture does not seed what it claims to", name)
		}
	}
	return f
}

// realCascade is auth.Service.DeleteOrgCascade, the Cascade production wires
// — the one the tables describe, not a test double.
func realCascade(t *testing.T, st identity.Store) Cascade {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      st,
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	return svc.DeleteOrgCascade
}

// TestPurgedByCascadeTellsTheTruth holds the two identity tables to the real
// cascade: every collection purgedByCascade names is empty of the org after
// DeleteOrgCascade, and every identity collection notPurged names still has
// its row — the user and their SSO link survive the org they belonged to.
// A collection declared purged by the cascade that the cascade leaves fails
// here by name.
func TestPurgedByCascadeTellsTheTruth(t *testing.T) {
	ctx := context.Background()
	st := identity.NewMemoryStore()
	f := seedCascadeFixture(t, st)
	if err := realCascade(t, st)(ctx, f.orgID); err != nil {
		t.Fatalf("DeleteOrgCascade: %v", err)
	}
	for name := range purgedByCascade {
		witness, ok := cascadeWitnesses[name]
		if !ok {
			t.Errorf("purgedByCascade names %q and no witness here proves the cascade empties it — add one, or the table is a claim nobody checks", name)
			continue
		}
		n, err := witness(ctx, st, f)
		if err != nil {
			t.Errorf("witness %s: %v", name, err)
			continue
		}
		if n != 0 {
			t.Errorf("%s: %d row(s) of the purged org survive the cascade, and purgedByCascade declares the collection purged by it — the table lies, move the row to notPurged with the true reason or make the cascade delete it", name, n)
		}
	}
	for name, witness := range cascadeWitnesses {
		if _, declaredPurged := purgedByCascade[name]; declaredPurged {
			continue
		}
		if _, left := notPurged[name]; !left {
			t.Errorf("%s: an identity collection this test witnesses is in neither purgedByCascade nor notPurged", name)
			continue
		}
		n, err := witness(ctx, st, f)
		if err != nil {
			t.Errorf("witness %s: %v", name, err)
			continue
		}
		if n == 0 {
			t.Errorf("%s: notPurged says the purge leaves it and the cascade emptied it — the table lies the other way", name)
		}
	}
}
