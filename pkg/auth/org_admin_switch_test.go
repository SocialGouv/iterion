package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// orgAdminFixture builds one org holding one team the user is NOT a member
// of, and grants the user the given ORG role. It is the exact shape the
// studio's team switcher offers an org admin: buildOrgTree lists every team
// of the org with a synthesized admin role, none of them backed by a
// membership row.
func orgAdminFixture(t *testing.T, svc *Service, email string, orgRole identity.OrgRole) (userID, orgID, teamID string) {
	t.Helper()
	ctx := context.Background()
	res, err := svc.Register(ctx, email, "correcthorse", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The memory store persists the ID it is given and generates none, so a
	// fixture that omits them yields org.ID == team.ID == "" and every
	// comparison below would pass by comparing "" to "".
	org, err := svc.store.CreateOrg(ctx, identity.Org{ID: "org-" + email, Name: "Acme " + email, Slug: "acme-" + email})
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	team, err := svc.store.CreateTeam(ctx, identity.Team{ID: "team-" + email, Name: "Product", Slug: "product-" + email, OrgID: org.ID})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if org.ID == "" || team.ID == "" || team.OrgID != org.ID {
		t.Fatalf("fixture is inert: org=%q team=%q team.org=%q", org.ID, team.ID, team.OrgID)
	}
	if err := svc.store.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: res.User.ID, OrgID: org.ID, Role: orgRole,
	}); err != nil {
		t.Fatalf("UpsertOrgMembership: %v", err)
	}
	if _, err := svc.store.GetMembership(ctx, res.User.ID, team.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("fixture must have no team membership row, got %v", err)
	}
	return res.User.ID, org.ID, team.ID
}

// TestSwitchTeamAdmitsOrgAdmin is the defect this closes: buildOrgTree
// offered an org admin every team of its org, and SwitchTeam refused them
// all with ErrNotAMember — the switcher click did nothing, silently.
func TestSwitchTeamAdmitsOrgAdmin(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, orgID, teamID := orgAdminFixture(t, svc, "orgadmin@example.com", identity.OrgRoleAdmin)

	id, _, _, err := svc.SwitchTeam(ctx, uid, teamID)
	if err != nil {
		t.Fatalf("SwitchTeam as org admin: %v", err)
	}
	if id.TeamID != teamID {
		t.Fatalf("active team = %q, want %q", id.TeamID, teamID)
	}
	if id.Role != identity.RoleAdmin {
		t.Fatalf("team role = %q, want admin (the role buildOrgTree advertises)", id.Role)
	}
	if id.OrgID != orgID || id.OrgRole != identity.OrgRoleAdmin {
		t.Fatalf("org context = %q/%s, want %q/admin", id.OrgID, id.OrgRole, orgID)
	}
}

// TestSwitchTeamAdmitsOrgOwner covers the other half of the ladder: owner
// is AtLeast(admin), so it must step in too.
func TestSwitchTeamAdmitsOrgOwner(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, _, teamID := orgAdminFixture(t, svc, "orgowner@example.com", identity.OrgRoleOwner)

	if _, _, _, err := svc.SwitchTeam(ctx, uid, teamID); err != nil {
		t.Fatalf("SwitchTeam as org owner: %v", err)
	}
}

// TestSwitchTeamRefusesPlainOrgMember is the BOUND. Without it this suite
// would only prove the widening, never its limit: a plain org member holds
// no claim on a team it was never granted, and buildOrgTree does not offer
// it one either.
func TestSwitchTeamRefusesPlainOrgMember(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, _, teamID := orgAdminFixture(t, svc, "orgmember@example.com", identity.OrgRoleMember)

	if _, _, _, err := svc.SwitchTeam(ctx, uid, teamID); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("expected ErrNotAMember for a plain org member, got %v", err)
	}
}

// TestSwitchTeamRefusesForeignOrgAdmin is the tenant boundary: administering
// one org must not open another org's teams.
func TestSwitchTeamRefusesForeignOrgAdmin(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, _, _ := orgAdminFixture(t, svc, "admin-a@example.com", identity.OrgRoleAdmin)
	_, _, foreignTeam := orgAdminFixture(t, svc, "admin-b@example.com", identity.OrgRoleAdmin)

	if _, _, _, err := svc.SwitchTeam(ctx, uid, foreignTeam); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("org admin of A must not reach a team of B, got %v", err)
	}
}

// TestSwitchTeamHidesTeamExistenceFromStrangers pins the disclosure rule:
// an unauthorized caller gets the same answer whether or not the team is
// real, so the endpoint is not an id oracle.
func TestSwitchTeamHidesTeamExistenceFromStrangers(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, _, _ := orgAdminFixture(t, svc, "stranger@example.com", identity.OrgRoleAdmin)
	_, _, realTeam := orgAdminFixture(t, svc, "owner-of-real@example.com", identity.OrgRoleAdmin)

	_, _, _, realErr := svc.SwitchTeam(ctx, uid, realTeam)
	_, _, _, fakeErr := svc.SwitchTeam(ctx, uid, "team-that-does-not-exist")
	if !errors.Is(realErr, ErrNotAMember) || !errors.Is(fakeErr, ErrNotAMember) {
		t.Fatalf("existing and missing team must answer alike: real=%v missing=%v", realErr, fakeErr)
	}
}

// TestSwitchOrgLandsOrgAdminOnATeam covers the SECOND site of the same
// class: naming no team at all. An org admin with no team grant used to
// land org-scoped with an empty active team — a workspace it could not
// leave, since the only way out was the switch that refused it.
func TestSwitchOrgLandsOrgAdminOnATeam(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, orgID, teamID := orgAdminFixture(t, svc, "landing@example.com", identity.OrgRoleAdmin)

	id, _, _, err := svc.SwitchOrg(ctx, uid, orgID)
	if err != nil {
		t.Fatalf("SwitchOrg: %v", err)
	}
	if id.TeamID != teamID {
		t.Fatalf("active team = %q, want the org's only team %q", id.TeamID, teamID)
	}
	if id.Role != identity.RoleAdmin {
		t.Fatalf("team role = %q, want admin", id.Role)
	}
}

// TestSwitchOrgLeavesPlainMemberTeamless is that site's bound.
func TestSwitchOrgLeavesPlainMemberTeamless(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	uid, orgID, _ := orgAdminFixture(t, svc, "landing-member@example.com", identity.OrgRoleMember)

	id, _, _, err := svc.SwitchOrg(ctx, uid, orgID)
	if err != nil {
		t.Fatalf("SwitchOrg: %v", err)
	}
	if id.TeamID != "" {
		t.Fatalf("plain org member must land teamless, got %q", id.TeamID)
	}
}
