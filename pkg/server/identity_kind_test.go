package server

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestSyntheticIdentityDeniedByRBACGates locks in the isolation invariant the
// config-share editor (and the inbound-webhook actor) rely on: a synthetic
// principal — Kind webhook/share — never passes an operator RBAC gate, even
// when it carries a matching TeamID + admin Role AND a spuriously-set
// super-admin flag (defence against a future bug that over-privileges one).
// A real user (zero Kind) with a real membership still passes.
func TestSyntheticIdentityDeniedByRBACGates(t *testing.T) {
	s := newOrgTestServer(t)
	seedTeam(t, s, "t1", "acme")
	ctx := context.Background()
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "real", TeamID: "t1", Role: identity.RoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []auth.IdentityKind{auth.KindShare, auth.KindWebhook} {
		// Deliberately over-privileged: matching team, owner-adjacent role,
		// and the super-admin flag — all of which the gates would honour for
		// a real user. The synthetic check must win.
		syn := auth.Identity{
			UserID: string(kind) + ":x", TeamID: "t1",
			Role: identity.RoleAdmin, IsSuperAdmin: true, Kind: kind,
		}
		if !syn.IsSynthetic() {
			t.Fatalf("Kind %q must report synthetic", kind)
		}
		if s.canViewTeam(ctx, syn, "t1") {
			t.Fatalf("synthetic %q passed canViewTeam", kind)
		}
		if s.canManageTeam(ctx, syn, "t1") {
			t.Fatalf("synthetic %q passed canManageTeam", kind)
		}
		if s.canViewOrg(ctx, syn, "org1") {
			t.Fatalf("synthetic %q passed canViewOrg", kind)
		}
		if s.canManageOrg(ctx, syn, "org1") {
			t.Fatalf("synthetic %q passed canManageOrg", kind)
		}
	}

	// Control: a real user (zero Kind) with the admin membership passes.
	real := auth.Identity{UserID: "real", TeamID: "t1", Role: identity.RoleAdmin}
	if real.IsSynthetic() {
		t.Fatal("zero-Kind identity must not be synthetic")
	}
	if !s.canViewTeam(ctx, real, "t1") || !s.canManageTeam(ctx, real, "t1") {
		t.Fatal("real user with admin membership must pass the team gates")
	}
}
