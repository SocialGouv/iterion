package identity

import "testing"

// TestRoleConfigEditor_OrthogonalToLadder locks ADR-078's core invariant: the
// config_editor capability is a real, assignable role (Valid → round-trips
// through the JWT claim + member API) yet sits OUTSIDE the viewer<member<admin<
// owner ladder — it is neither ≥ any ladder role nor is any ladder role ≥ it —
// so a gate that admits AtLeast(RoleViewer) can never mistake it for viewer+.
func TestRoleConfigEditor_OrthogonalToLadder(t *testing.T) {
	if !RoleConfigEditor.Valid() {
		t.Fatal("config_editor must be Valid() so it round-trips through JWT + member API")
	}
	ladder := []Role{RoleViewer, RoleMember, RoleAdmin, RoleOwner}
	for _, r := range ladder {
		if RoleConfigEditor.AtLeast(r) {
			t.Errorf("config_editor must NOT be AtLeast(%s)", r)
		}
		if r.AtLeast(RoleConfigEditor) {
			t.Errorf("%s must NOT be AtLeast(config_editor)", r)
		}
		// The gate's new check (AtLeast(RoleViewer)) still admits every ladder
		// role — equivalent to the old Valid() for these four.
		if !r.AtLeast(RoleViewer) {
			t.Errorf("%s must be AtLeast(viewer) — the standard team-view gate", r)
		}
	}
	if RoleConfigEditor.AtLeast(RoleViewer) {
		t.Error("config_editor must NOT be AtLeast(viewer) — that is the over-grant hazard ADR-078 closes")
	}
}

// TestOrgRoleForTeamRole_ConfigEditorConfersNoOrgRole locks ADR-078 decision 4:
// a config_editor team membership must NOT mirror to an org role (an org-member
// row would pass canViewOrg → org roster/usage read). The ladder roles are
// unchanged.
func TestOrgRoleForTeamRole_ConfigEditorConfersNoOrgRole(t *testing.T) {
	if got := OrgRoleForTeamRole(RoleConfigEditor); got != "" {
		t.Errorf("config_editor org role = %q, want \"\" (no org mirror)", got)
	}
	// A team rank is a rank in that team: every ladder role, owner included,
	// mirrors up to an org MEMBER and never to an org admin.
	for role, want := range map[Role]OrgRole{
		RoleViewer: OrgRoleMember,
		RoleMember: OrgRoleMember,
		RoleAdmin:  OrgRoleMember,
		RoleOwner:  OrgRoleMember,
	} {
		if got := OrgRoleForTeamRole(role); got != want {
			t.Errorf("OrgRoleForTeamRole(%s) = %q, want %q", role, got, want)
		}
	}
}

func TestInvitationOrgRole_NamesTheOrgRoleApartFromTheTeamRole(t *testing.T) {
	for _, tc := range []struct {
		role  Role
		named OrgRole
		want  OrgRole
	}{
		// A team invitation names no org role: member, whatever the team rank.
		{RoleAdmin, "", OrgRoleMember},
		{RoleOwner, "", OrgRoleMember},
		{RoleViewer, "", OrgRoleMember},
		// An org invitation names the org role apart from the team role.
		{RoleMember, OrgRoleAdmin, OrgRoleAdmin},
		{RoleViewer, OrgRoleOwner, OrgRoleOwner},
		{RoleAdmin, OrgRoleMember, OrgRoleMember},
		// Off the ladder, no org membership at all, whatever is named.
		{RoleConfigEditor, "", ""},
		{RoleConfigEditor, OrgRoleAdmin, ""},
		// A name that is no org role reads as none.
		{RoleMember, OrgRole("superadmin"), OrgRoleMember},
	} {
		if got := InvitationOrgRole(tc.role, tc.named); got != tc.want {
			t.Errorf("InvitationOrgRole(%s, %q) = %q, want %q", tc.role, tc.named, got, tc.want)
		}
		inv := Invitation{Role: tc.role, OrgRole: tc.named}
		if got := inv.OrgRoleOnAccept(); got != tc.want {
			t.Errorf("Invitation{Role: %s, OrgRole: %q}.OrgRoleOnAccept() = %q, want %q", tc.role, tc.named, got, tc.want)
		}
	}
}
