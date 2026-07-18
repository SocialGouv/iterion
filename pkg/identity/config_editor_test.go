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
