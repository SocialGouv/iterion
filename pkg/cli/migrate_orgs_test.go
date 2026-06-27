package cli

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

func seedLegacyTeam(t *testing.T, st identity.Store, id, slug string, members map[string]identity.Role) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateTeam(ctx, identity.Team{ID: id, Name: id, Slug: slug, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed team %s: %v", id, err)
	}
	for uid, role := range members {
		if err := st.UpsertMembership(ctx, identity.Membership{UserID: uid, TeamID: id, Role: role, JoinedAt: time.Now()}); err != nil {
			t.Fatalf("seed membership %s/%s: %v", uid, id, err)
		}
	}
}

func TestMigrateTeamsToOrgs_Backfill(t *testing.T) {
	st := identity.NewMemoryStore()
	ctx := context.Background()
	seedLegacyTeam(t, st, "t1", "acme", map[string]identity.Role{"owner": identity.RoleOwner, "viewer": identity.RoleViewer})
	seedLegacyTeam(t, st, "t2", "beta", map[string]identity.Role{"admin": identity.RoleAdmin})

	res, err := MigrateTeamsToOrgs(ctx, st, nil, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.OrgsCreated != 2 || res.TeamsLinked != 2 || res.OrgMembersCreated != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Each team is now linked to a NEW org (distinct id).
	for _, teamID := range []string{"t1", "t2"} {
		team, _ := st.GetTeam(ctx, teamID)
		if team.OrgID == "" {
			t.Fatalf("team %s not linked", teamID)
		}
		if team.OrgID == teamID {
			t.Fatalf("org id must differ from team id, got %s", team.OrgID)
		}
		org, err := st.GetOrg(ctx, team.OrgID)
		if err != nil {
			t.Fatalf("org %s missing: %v", team.OrgID, err)
		}
		if org.MigratedFromTeamID != teamID {
			t.Fatalf("org %s migrated_from = %q, want %s", org.ID, org.MigratedFromTeamID, teamID)
		}
	}

	// Role mapping: owner/admin -> org admin, viewer -> org member.
	t1org, _ := st.GetTeam(ctx, "t1")
	ownerOM, _ := st.GetOrgMembership(ctx, "owner", t1org.OrgID)
	if ownerOM.Role != identity.OrgRoleAdmin {
		t.Fatalf("owner org role = %s, want admin", ownerOM.Role)
	}
	viewerOM, _ := st.GetOrgMembership(ctx, "viewer", t1org.OrgID)
	if viewerOM.Role != identity.OrgRoleMember {
		t.Fatalf("viewer org role = %s, want member", viewerOM.Role)
	}
}

func TestMigrateTeamsToOrgs_Idempotent(t *testing.T) {
	st := identity.NewMemoryStore()
	ctx := context.Background()
	seedLegacyTeam(t, st, "t1", "acme", map[string]identity.Role{"owner": identity.RoleOwner})

	first, err := MigrateTeamsToOrgs(ctx, st, nil, false)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	orgID := first.Mapping["t1"]

	second, err := MigrateTeamsToOrgs(ctx, st, nil, false)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.OrgsCreated != 0 || second.TeamsLinked != 0 {
		t.Fatalf("re-run must be a no-op, got %+v", second)
	}
	// Only one org exists for the team.
	orgs, _ := st.ListOrgs(ctx, identity.Page{Limit: 100})
	count := 0
	for _, o := range orgs {
		if o.MigratedFromTeamID == "t1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 migrated org for t1, got %d", count)
	}
	team, _ := st.GetTeam(ctx, "t1")
	if team.OrgID != orgID {
		t.Fatalf("team org id changed across runs: %s != %s", team.OrgID, orgID)
	}
}

func TestMigrateTeamsToOrgs_DryRunWritesNothing(t *testing.T) {
	st := identity.NewMemoryStore()
	ctx := context.Background()
	seedLegacyTeam(t, st, "t1", "acme", map[string]identity.Role{"owner": identity.RoleOwner})

	res, err := MigrateTeamsToOrgs(ctx, st, nil, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.OrgsCreated != 1 {
		t.Fatalf("dry-run should still report 1 planned org, got %d", res.OrgsCreated)
	}
	orgs, _ := st.ListOrgs(ctx, identity.Page{Limit: 100})
	if len(orgs) != 0 {
		t.Fatalf("dry-run wrote %d orgs, want 0", len(orgs))
	}
	team, _ := st.GetTeam(ctx, "t1")
	if team.OrgID != "" {
		t.Fatal("dry-run linked a team")
	}
}

func TestReverseTeamsToOrgs(t *testing.T) {
	st := identity.NewMemoryStore()
	ctx := context.Background()
	seedLegacyTeam(t, st, "t1", "acme", map[string]identity.Role{"owner": identity.RoleOwner, "viewer": identity.RoleViewer})

	if _, err := MigrateTeamsToOrgs(ctx, st, nil, false); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rev, err := ReverseTeamsToOrgs(ctx, st, nil, false)
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if rev.OrgsDeleted != 1 {
		t.Fatalf("reverse deleted %d orgs, want 1", rev.OrgsDeleted)
	}
	// Team unlinked, orgs gone, org memberships gone.
	team, _ := st.GetTeam(ctx, "t1")
	if team.OrgID != "" {
		t.Fatalf("team still linked after reverse: %s", team.OrgID)
	}
	orgs, _ := st.ListOrgs(ctx, identity.Page{Limit: 100})
	if len(orgs) != 0 {
		t.Fatalf("reverse left %d orgs", len(orgs))
	}
	oms, _ := st.ListOrgMembershipsByUser(ctx, "owner")
	if len(oms) != 0 {
		t.Fatalf("reverse left %d org memberships", len(oms))
	}

	// Reverse is idempotent.
	rev2, err := ReverseTeamsToOrgs(ctx, st, nil, false)
	if err != nil {
		t.Fatalf("reverse #2: %v", err)
	}
	if rev2.OrgsDeleted != 0 {
		t.Fatalf("second reverse must be a no-op, deleted %d", rev2.OrgsDeleted)
	}
}
