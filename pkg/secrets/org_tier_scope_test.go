package secrets

import "testing"

// The org tier stores its keys and forfaits under a reserved scope, exactly
// as the platform tier does. Three properties make that safe, and each one
// is a silent failure if it breaks: the scope must not be mistakable for a
// real tenant, it must not collide with the TEAM-scoped OrgOwnerKey (whose
// name says "org" but whose argument is a team id), and it must not collide
// with the platform scope.
func TestOrgTierScope_cannotCollideWithTheOtherOwnerNamespaces(t *testing.T) {
	const orgID = "5f916212-a1bd-453a-b8cc-ebbe24abd8cc"
	const teamID = "3a29c5ee-0509-48da-88cf-5439707630c1"

	orgTenant := OrgTierTenantID(orgID)
	orgOwner := OrgTierOwnerKey(orgID)

	// The API-key and OAuth namespaces address the org by the same literal
	// — one concept, two indexes (the PlatformTenantID/PlatformOwnerKey
	// relationship).
	if orgTenant != orgOwner {
		t.Errorf("org-tier tenant id %q and owner key %q must share their literal", orgTenant, orgOwner)
	}

	// A team forfait lives under "org:<team-uuid>" (OrgOwnerPrefix). If the
	// org tier reused that prefix, an org credential and a team credential
	// would land in one owner namespace and the publisher would serve
	// whichever the store returned first.
	if teamOwner := OrgOwnerKey(teamID); orgOwner == teamOwner {
		t.Fatalf("org-tier owner key collides with the TEAM owner key: both %q", orgOwner)
	}
	if IsOrgTierScope(OrgOwnerKey(orgID)) {
		t.Errorf("OrgOwnerKey(%q) = %q reads as an org-tier scope — the prefixes overlap",
			orgID, OrgOwnerKey(orgID))
	}

	// The platform scope is a different tier entirely.
	if orgTenant == PlatformTenantID || orgOwner == PlatformOwnerKey {
		t.Errorf("org-tier scope %q collides with the platform scope %q", orgTenant, PlatformTenantID)
	}
	if IsOrgTierScope(PlatformTenantID) {
		t.Errorf("the platform scope %q reads as an org-tier scope", PlatformTenantID)
	}

	// A real tenant/user id is never mistaken for the org tier.
	for _, real := range []string{orgID, teamID, "devthejo@protonmail.com", ""} {
		if IsOrgTierScope(real) {
			t.Errorf("IsOrgTierScope(%q) = true — a real id must never read as a reserved scope", real)
		}
	}
}

func TestOrgIDFromTierScope_roundTripsAndRefusesForeignScopes(t *testing.T) {
	const orgID = "5f916212-a1bd-453a-b8cc-ebbe24abd8cc"
	got, ok := OrgIDFromTierScope(OrgTierTenantID(orgID))
	if !ok || got != orgID {
		t.Errorf("OrgIDFromTierScope(OrgTierTenantID(%q)) = (%q, %v), want (%q, true)", orgID, got, ok, orgID)
	}
	for _, foreign := range []string{PlatformTenantID, OrgOwnerKey(orgID), orgID, ""} {
		if _, ok := OrgIDFromTierScope(foreign); ok {
			t.Errorf("OrgIDFromTierScope(%q) claimed an org id from a scope it does not own", foreign)
		}
	}
}
