package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestBuildOrgTree_BulkLookup covers the batched org resolution: org
// rows keep the org-membership order (JoinedAt asc), a membership whose
// org is gone is skipped (the old per-row GetOrg ErrNotFound path), org
// admins see ungranted teams with an implied admin role, and plain org
// members see only their granted teams.
func TestBuildOrgTree_BulkLookup(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedOrg(t, s, "o1", "org-one")
	seedOrg(t, s, "o2", "org-two")
	for i, tm := range []identity.Team{
		{ID: "ta", OrgID: "o1", Name: "TA", Slug: "ta"},
		{ID: "tb", OrgID: "o1", Name: "TB", Slug: "tb"},
		{ID: "tc", OrgID: "o2", Name: "TC", Slug: "tc"},
	} {
		tm.CreatedAt = now.Add(time.Duration(i) * time.Second)
		if _, err := s.authStore().CreateTeam(ctx, tm); err != nil {
			t.Fatalf("seed team %s: %v", tm.ID, err)
		}
	}
	// Org-membership order is JoinedAt asc: o1 (admin), a dangling org,
	// o2 (plain member).
	for i, om := range []identity.OrgMembership{
		{UserID: "u1", OrgID: "o1", Role: identity.OrgRoleAdmin},
		{UserID: "u1", OrgID: "o-gone", Role: identity.OrgRoleMember},
		{UserID: "u1", OrgID: "o2", Role: identity.OrgRoleMember},
	} {
		om.JoinedAt = now.Add(time.Duration(i) * time.Second)
		if err := s.authStore().UpsertOrgMembership(ctx, om); err != nil {
			t.Fatalf("seed org membership %s: %v", om.OrgID, err)
		}
	}
	// Team grant: u1 is a member of ta only. tb is visible to them via
	// the org-admin fallback; tc stays hidden (plain member of o2).
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "u1", TeamID: "ta", Role: identity.RoleMember, JoinedAt: now,
	}); err != nil {
		t.Fatalf("seed team membership: %v", err)
	}

	orgs, err := s.buildOrgTree(ctx, "u1")
	if err != nil {
		t.Fatalf("buildOrgTree: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("got %d orgs, want 2 (dangling org membership skipped): %+v", len(orgs), orgs)
	}
	if orgs[0].OrgID != "o1" || orgs[1].OrgID != "o2" {
		t.Fatalf("org-membership order lost: %+v", orgs)
	}
	if orgs[0].OrgRole != "admin" || orgs[0].OrgSlug != "org-one" {
		t.Fatalf("o1 view mismatch: %+v", orgs[0])
	}
	if len(orgs[0].Teams) != 2 {
		t.Fatalf("org admin must see both o1 teams, got %+v", orgs[0].Teams)
	}
	if orgs[0].Teams[0].TeamID != "ta" || orgs[0].Teams[0].Role != "member" {
		t.Fatalf("granted team keeps its granted role: %+v", orgs[0].Teams[0])
	}
	if orgs[0].Teams[1].TeamID != "tb" || orgs[0].Teams[1].Role != "admin" {
		t.Fatalf("ungranted team gets the org-admin implied role: %+v", orgs[0].Teams[1])
	}
	if len(orgs[1].Teams) != 0 {
		t.Fatalf("plain org member must not see ungranted teams: %+v", orgs[1].Teams)
	}
}
