package orgsso

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func testSealer(t *testing.T) secrets.Sealer {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	s, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

func oidcRow(id, tenant string) OrgSSOProvider {
	return OrgSSOProvider{
		ID:          id,
		TenantID:    tenant,
		Kind:        KindOIDC,
		Enabled:     true,
		DisplayName: "Acme Keycloak",
		IssuerURL:   "https://sso.acme.example/realms/main",
		ClientID:    "iterion",
		CreatedAt:   time.Now(),
	}
}

func githubRow(id, tenant string, grants ...GitHubTeamGrant) OrgSSOProvider {
	// Test rows model verified (active) grants — proof-of-control gating
	// (FlattenGitHubTeamKeys / RoleForGroups) skips unverified ones.
	for i := range grants {
		grants[i].Verified = true
	}
	return OrgSSOProvider{
		ID:            id,
		TenantID:      tenant,
		Kind:          KindGitHub,
		Enabled:       true,
		AutoProvision: true,
		Grants:        grants,
		CreatedAt:     time.Now(),
	}
}

func TestMemoryStore_OIDC_CRUD(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	row := oidcRow("p1", "t1")
	// Validate runs after Normalize; the store normalizes on Create, so
	// row.Validate() isn't asserted here.
	if err := st.Create(ctx, row); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.Get(ctx, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.DefaultRole != identity.RoleMember {
		t.Errorf("default role not normalized: %q", got.DefaultRole)
	}
	if len(got.Scopes) == 0 {
		t.Errorf("scopes not defaulted")
	}
	got.DisplayName = "Renamed"
	if err := st.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, _ := st.Get(ctx, "p1")
	if again.DisplayName != "Renamed" {
		t.Errorf("update not persisted")
	}
	if err := st.Delete(ctx, "p1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, "p1"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStore_GitHub_OnePerTenant(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	g := GitHubTeamGrant{GitHubOrg: "acme", TeamSlug: "eng", Role: identity.RoleMember}
	if err := st.Create(ctx, githubRow("g1", "t1", g)); err != nil {
		t.Fatalf("create g1: %v", err)
	}
	if err := st.Create(ctx, githubRow("g2", "t1", g)); err != ErrExists {
		t.Errorf("expected ErrExists for second github row in tenant, got %v", err)
	}
	// A second github row in a *different* tenant is fine.
	if err := st.Create(ctx, githubRow("g3", "t2", g)); err != nil {
		t.Errorf("github row in other tenant should succeed: %v", err)
	}
}

// TestGitHubGrant_ConfigEditorNotCapped proves the config_editor capability is
// assignable via SSO (ADR-078): unlike an admin grant (capped to member),
// config_editor is Valid and below-admin, so RoleForGroups passes it through
// unchanged — a "veille editors" GitHub team can grant it directly.
func TestGitHubGrant_ConfigEditorNotCapped(t *testing.T) {
	row := githubRow("g", "t", GitHubTeamGrant{GitHubOrg: "acme", TeamSlug: "veille-editors", Role: identity.RoleConfigEditor})
	if r, ok := row.RoleForGroups([]string{"acme/veille-editors"}); !ok || r != identity.RoleConfigEditor {
		t.Errorf("config_editor grant → %q,%v want config_editor (not capped like admin)", r, ok)
	}
}

func TestMemoryStore_ListByTenantKind(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	_ = st.Create(ctx, oidcRow("p1", "t1"))
	_ = st.Create(ctx, githubRow("g1", "t1", GitHubTeamGrant{GitHubOrg: "acme", Role: identity.RoleMember}))
	_ = st.Create(ctx, oidcRow("p2", "t2"))

	oidc, err := st.ListByTenantKind(ctx, "t1", KindOIDC)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(oidc) != 1 || oidc[0].ID != "p1" {
		t.Errorf("expected only p1 for t1/oidc, got %+v", oidc)
	}
	all, _ := st.ListByTenant(ctx, "t1")
	if len(all) != 2 {
		t.Errorf("expected 2 rows for t1, got %d", len(all))
	}
}

func TestMemoryStore_FindGitHubGrantingOrgs(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	// t1: exact team eng. t2: wildcard org. t3: disabled. t4: different org.
	_ = st.Create(ctx, githubRow("g1", "t1", GitHubTeamGrant{GitHubOrg: "Acme", TeamSlug: "Eng", Role: identity.RoleMember}))
	_ = st.Create(ctx, githubRow("g2", "t2", GitHubTeamGrant{GitHubOrg: "acme", TeamSlug: "*", Role: identity.RoleAdmin}))
	disabled := githubRow("g3", "t3", GitHubTeamGrant{GitHubOrg: "acme", TeamSlug: "eng", Role: identity.RoleMember})
	disabled.Enabled = false
	_ = st.Create(ctx, disabled)
	_ = st.Create(ctx, githubRow("g4", "t4", GitHubTeamGrant{GitHubOrg: "other", TeamSlug: "eng", Role: identity.RoleMember}))

	// A user in acme/eng (lowercased keys) should match g1 (exact) and g2 (wildcard).
	keys := []string{"acme/eng", "acme/*"}
	got, err := st.FindGitHubGrantingOrgs(ctx, keys)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	ids := map[string]bool{}
	for _, p := range got {
		ids[p.ID] = true
	}
	if !ids["g1"] || !ids["g2"] {
		t.Errorf("expected g1+g2, got %v", ids)
	}
	if ids["g3"] {
		t.Errorf("disabled row g3 must be excluded")
	}
	if ids["g4"] {
		t.Errorf("non-matching org g4 must be excluded")
	}
}

func TestFlattenGitHubTeamKeys(t *testing.T) {
	keys := FlattenGitHubTeamKeys([]GitHubTeamGrant{
		{GitHubOrg: "Acme", TeamSlug: "Eng", Verified: true},
		{GitHubOrg: "acme", TeamSlug: "", Verified: true},  // → acme/*
		{GitHubOrg: "acme", TeamSlug: "*", Verified: true}, // dup of acme/*
	})
	want := map[string]bool{"acme/eng": true, "acme/*": true}
	if len(keys) != len(want) {
		t.Fatalf("expected %d keys, got %v", len(want), keys)
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q", k)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		row     OrgSSOProvider
		wantErr bool
	}{
		{"oidc ok", oidcRow("p", "t"), false},
		{"oidc http rejected", func() OrgSSOProvider {
			r := oidcRow("p", "t")
			r.IssuerURL = "http://sso.acme.example"
			return r
		}(), true},
		{"oidc missing client", func() OrgSSOProvider {
			r := oidcRow("p", "t")
			r.ClientID = ""
			return r
		}(), true},
		{"oidc owner default role rejected", func() OrgSSOProvider {
			r := oidcRow("p", "t")
			r.DefaultRole = identity.RoleOwner
			return r
		}(), true},
		{"github member ok", githubRow("g", "t", GitHubTeamGrant{GitHubOrg: "acme", Role: identity.RoleMember}), false},
		{"github viewer ok", githubRow("g", "t", GitHubTeamGrant{GitHubOrg: "acme", Role: identity.RoleViewer}), false},
		{"github admin grant rejected (capped at member)", githubRow("g", "t", GitHubTeamGrant{GitHubOrg: "acme", Role: identity.RoleAdmin}), true},
		{"github owner grant rejected", githubRow("g", "t", GitHubTeamGrant{GitHubOrg: "acme", Role: identity.RoleOwner}), true},
		{"github no grants rejected", githubRow("g", "t"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row
			row.Normalize()
			err := row.Validate()
			if tc.wantErr != (err != nil) {
				t.Errorf("wantErr=%v got %v", tc.wantErr, err)
			}
		})
	}
}

func TestSealRoundTrip(t *testing.T) {
	sealer := testSealer(t)
	sealed, err := SealClientSecret(sealer, "p1", "s3cr3t")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := OpenClientSecret(sealer, "p1", sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != "s3cr3t" {
		t.Errorf("got %q", got)
	}
	// AAD binding: opening under a different provider id must fail.
	if _, err := OpenClientSecret(sealer, "p2", sealed); err == nil {
		t.Errorf("expected AAD mismatch to fail open")
	}
}

func TestRoleForGroups(t *testing.T) {
	row := OrgSSOProvider{Kind: KindGitHub, Grants: []GitHubTeamGrant{
		{GitHubOrg: "acme", TeamSlug: "eng", Role: identity.RoleMember, Verified: true},
		{GitHubOrg: "acme", TeamSlug: "ops", Role: identity.RoleViewer, Verified: true},
		{GitHubOrg: "acme", TeamSlug: "*", Role: identity.RoleMember, Verified: true},
	}}
	row.Normalize()
	// First matching grant in order wins.
	if r, ok := row.RoleForGroups([]string{"acme/eng"}); !ok || r != identity.RoleMember {
		t.Errorf("eng → %q,%v want member", r, ok)
	}
	if r, ok := row.RoleForGroups([]string{"acme/ops"}); !ok || r != identity.RoleViewer {
		t.Errorf("ops → %q,%v want viewer", r, ok)
	}
	// Wildcard matches a plain org member.
	if r, ok := row.RoleForGroups([]string{"acme/*"}); !ok || r != identity.RoleMember {
		t.Errorf("acme/* → %q,%v want member", r, ok)
	}
	if _, ok := row.RoleForGroups([]string{"other/*"}); ok {
		t.Errorf("non-matching group should not match")
	}
}

func TestRoleForGroups_CapsAdminToMember(t *testing.T) {
	// Even if a row somehow carries an admin grant (Validate rejects it at
	// write time), RoleForGroups clamps to member at login — defence in depth.
	row := OrgSSOProvider{Kind: KindGitHub, Grants: []GitHubTeamGrant{
		{GitHubOrg: "acme", TeamSlug: "eng", Role: identity.RoleAdmin, Verified: true},
	}}
	row.Normalize()
	if r, ok := row.RoleForGroups([]string{"acme/eng"}); !ok || r != identity.RoleMember {
		t.Errorf("admin grant should clamp to member, got %q", r)
	}
}

func TestGitHubGatingActive(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	if active, _ := st.GitHubGatingActive(ctx); active {
		t.Errorf("no rows → not active")
	}
	_ = st.Create(ctx, oidcRow("p1", "t1")) // an OIDC row doesn't count
	if active, _ := st.GitHubGatingActive(ctx); active {
		t.Errorf("only oidc row → not active")
	}
	disabled := githubRow("g1", "t1", GitHubTeamGrant{GitHubOrg: "acme", Role: identity.RoleMember})
	disabled.Enabled = false
	_ = st.Create(ctx, disabled)
	if active, _ := st.GitHubGatingActive(ctx); active {
		t.Errorf("disabled github row → not active")
	}
	_ = st.Create(ctx, githubRow("g2", "t2", GitHubTeamGrant{GitHubOrg: "acme", Role: identity.RoleMember}))
	if active, _ := st.GitHubGatingActive(ctx); !active {
		t.Errorf("an enabled github row → active")
	}
}

func TestOIDCSlugRoundTrip(t *testing.T) {
	p := oidcRow("abc-123", "t1")
	slug := p.OIDCSlug()
	id, ok := ParseOIDCSlug(slug)
	if !ok || id != "abc-123" {
		t.Errorf("slug roundtrip failed: slug=%q id=%q ok=%v", slug, id, ok)
	}
	if _, ok := ParseOIDCSlug("github"); ok {
		t.Errorf("global slug must not parse as per-org")
	}
}
