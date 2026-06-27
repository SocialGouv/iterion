package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestRegisterCreatesPersonalOrg asserts a password signup provisions a
// personal Org wrapping the personal Team, with owner memberships at
// both levels and both defaults recorded on the user.
func TestRegisterCreatesPersonalOrg(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)

	res, err := svc.Register(ctx, "alice@example.com", "correcthorse", "Alice", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.ActiveOrgID == "" {
		t.Fatal("expected a personal org id")
	}
	if res.ActiveOrgRole != identity.OrgRoleOwner {
		t.Fatalf("expected owner of personal org, got %q", res.ActiveOrgRole)
	}
	// The active team belongs to the active org.
	team, _ := svc.store.GetTeam(ctx, res.ActiveTeamID)
	if team.OrgID != res.ActiveOrgID {
		t.Fatalf("active team org=%q != active org=%q", team.OrgID, res.ActiveOrgID)
	}
	// User defaults persisted.
	u, _ := svc.store.GetUser(ctx, res.User.ID)
	if u.DefaultOrgID != res.ActiveOrgID || u.DefaultTeamID != res.ActiveTeamID {
		t.Fatalf("defaults not persisted: org=%q team=%q", u.DefaultOrgID, u.DefaultTeamID)
	}
	// Org membership exists.
	om, err := svc.store.GetOrgMembership(ctx, res.User.ID, res.ActiveOrgID)
	if err != nil || om.Role != identity.OrgRoleOwner {
		t.Fatalf("personal org membership missing: %v role=%s", err, om.Role)
	}
}

// TestSwitchOrgAndTeam covers the two-level switch: a user who owns two
// orgs (their personal one + a second created org) can switch between
// them, landing on a team within each, and SwitchTeam validates the
// org→team chain.
func TestSwitchOrgAndTeam(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	res, err := svc.Register(ctx, "alice@example.com", "correcthorse", "Alice", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	uid := res.User.ID
	personalOrg := res.ActiveOrgID

	// Create a second org (with a default team) owned by Alice.
	org2, err := svc.CreateOrgFor(ctx, uid, "Acme Corp", "")
	if err != nil {
		t.Fatalf("CreateOrgFor: %v", err)
	}
	if org2.ID == personalOrg {
		t.Fatal("second org must be distinct")
	}

	// Switch to org2 → lands on a team within org2.
	id, _, _, err := svc.SwitchOrg(ctx, uid, org2.ID)
	if err != nil {
		t.Fatalf("SwitchOrg: %v", err)
	}
	if id.OrgID != org2.ID {
		t.Fatalf("active org = %q, want %q", id.OrgID, org2.ID)
	}
	if id.TeamID == "" {
		t.Fatal("switching org should land on a team in it")
	}
	landedTeam, _ := svc.store.GetTeam(ctx, id.TeamID)
	if landedTeam.OrgID != org2.ID {
		t.Fatalf("landed team org=%q != %q", landedTeam.OrgID, org2.ID)
	}

	// SwitchTeam to the landed team populates the org context too.
	id2, _, _, err := svc.SwitchTeam(ctx, uid, id.TeamID)
	if err != nil {
		t.Fatalf("SwitchTeam: %v", err)
	}
	if id2.OrgID != org2.ID || id2.OrgRole != identity.OrgRoleOwner {
		t.Fatalf("switch-team org context = %q/%s, want %q/owner", id2.OrgID, id2.OrgRole, org2.ID)
	}
}

// TestSwitchOrgNotMember asserts a non-member (non-super-admin) cannot
// switch into an org they don't belong to.
func TestSwitchOrgNotMember(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	a, _ := svc.Register(ctx, "a@example.com", "correcthorse", "A", "", "ua", "ip")
	b, _ := svc.Register(ctx, "b@example.com", "correcthorse", "B", "", "ua", "ip")

	// B tries to switch into A's personal org.
	if _, _, _, err := svc.SwitchOrg(ctx, b.User.ID, a.ActiveOrgID); !errors.Is(err, ErrNotAnOrgMember) {
		t.Fatalf("expected ErrNotAnOrgMember, got %v", err)
	}
}
