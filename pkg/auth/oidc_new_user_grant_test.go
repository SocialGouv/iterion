package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// ssoOrgFixture is an SSO service over org o-acme holding team team-acme,
// on a gated identity store: the shape both new-user SSO paths grant into.
type ssoOrgFixture struct {
	svc *Service
	ids *nthCallGate
}

// raw is the identity store BEHIND the gate: a test seeds and inspects rows
// through it, so those calls are neither counted nor failed.
func (f *ssoOrgFixture) raw() identity.Store { return f.ids.Store }

// newGitHubOrgFixture is githubFixture with a PARENT ORG on the granted
// team: without one no org membership is mirrored at all, so the reads this
// file is about never run.
func newGitHubOrgFixture(t *testing.T, role identity.Role) *ssoOrgFixture {
	t.Helper()
	ctx := context.Background()
	ids := newNthCallGate(identity.NewMemoryStore(), errStoreUnreachable)
	svc, err := NewService(Config{
		Store: ids, Sessions: NewMemorySessionStore(), Signer: newTestSigner(t),
		SignupMode: SignupInviteOnly, RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	providers := orgsso.NewMemoryStore()
	svc.orgSSO = providers
	seed := ids.Store
	if _, err := seed.CreateOrg(ctx, identity.Org{ID: "o-acme", Name: "Acme", Slug: "acme-org", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := seed.CreateTeam(ctx, identity.Team{ID: "team-acme", OrgID: "o-acme", Name: "Acme", Slug: "acme"}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := providers.Create(ctx, orgsso.OrgSSOProvider{
		ID: "gh1", TenantID: "team-acme", Kind: orgsso.KindGitHub, Enabled: true, AutoProvision: true,
		Grants:    []orgsso.GitHubTeamGrant{{GitHubOrg: "acme", TeamSlug: "eng", Role: role, Verified: true}},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("github row: %v", err)
	}
	return &ssoOrgFixture{svc: svc, ids: ids}
}

// newPerOrgSSOFixture is the same shape for a tenant's own OIDC provider.
func newPerOrgSSOFixture(t *testing.T) *ssoOrgFixture {
	t.Helper()
	ctx := context.Background()
	ids := newNthCallGate(identity.NewMemoryStore(), errStoreUnreachable)
	svc := newTestServiceOn(t, ids, NewMemorySessionStore())
	providers := orgsso.NewMemoryStore()
	svc.orgSSO = providers
	seed := ids.Store
	if _, err := seed.CreateOrg(ctx, identity.Org{ID: "o1", Name: "Org One", Slug: "org-one", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := seed.CreateTeam(ctx, identity.Team{ID: "tb", OrgID: "o1", Name: "Team B", Slug: "team-b"}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if err := providers.Create(ctx, orgsso.OrgSSOProvider{
		ID: "prov1", TenantID: "tb", Kind: orgsso.KindOIDC, Enabled: true,
		IssuerURL: "https://sso.example.org/realms/main", ClientID: "iterion",
		DefaultRole: identity.RoleMember, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the provider: %v", err)
	}
	return &ssoOrgFixture{svc: svc, ids: ids}
}

// nothingCommitted fails the test unless the store holds neither an account
// for email nor an SSO link for the identity: a first login whose read failed
// before any write must leave the store as it was.
func (f *ssoOrgFixture) nothingCommitted(t *testing.T, provider, subject, email string) {
	t.Helper()
	ctx := context.Background()
	if u, err := f.raw().GetUserByEmail(ctx, email); err == nil {
		t.Fatalf("the failed first login committed the account %s (%s)", u.ID, u.Email)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("read the account back: %v", err)
	}
	if l, err := f.raw().GetOIDCLink(ctx, provider, subject); err == nil {
		t.Fatalf("the failed first login committed the SSO link of %s", l.UserID)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("read the SSO link back: %v", err)
	}
}

// TestLoginGitHub_AFirstLoginsFailedGrantReadCommitsNothing: a gated GitHub
// first login resolves the granted team — its parent org is both the
// membership to mirror the grant up to and the account's default org — and
// that read runs BEFORE the account exists. A failure there leaves no
// account and no SSO link, so the user retries into the same first login
// rather than into a half-provisioned one.
func TestLoginGitHub_AFirstLoginsFailedGrantReadCommitsNothing(t *testing.T) {
	ctx := context.Background()
	f := newGitHubOrgFixture(t, identity.RoleMember)
	ext := githubExt("u1", "acme/*", "acme/eng")
	f.ids.fail("GetTeam", 1)
	if _, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a first GitHub login whose team read failed: got %v, want the read's failure", err)
	}
	f.nothingCommitted(t, ext.Provider, ext.Subject, ext.Email)
	f.ids.disarm()
	res, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip")
	if err != nil {
		t.Fatalf("the first login retried once the store answers: %v", err)
	}
	if res.ActiveTeamID != "team-acme" || res.ActiveOrgID != "o-acme" {
		t.Fatalf("the retried first login landed in team=%q org=%q, want team-acme/o-acme", res.ActiveTeamID, res.ActiveOrgID)
	}
}

// TestLoginGitHub_AFirstLoginReadsNoRowOfTheAccountItCreates: the grant of an
// account THIS call creates reads nothing about it — a uuid minted a moment
// ago holds no membership row, so the org-membership read and the team-grant
// read can only fail or answer "not found", and a read that can only fail is
// a read that must not run after a commit. The only membership reads of the
// login are the login context's, and the landing defaults come from the
// grant's own team read.
func TestLoginGitHub_AFirstLoginReadsNoRowOfTheAccountItCreates(t *testing.T) {
	ctx := context.Background()
	f := newGitHubOrgFixture(t, identity.RoleMember)
	ext := githubExt("u2", "acme/*", "acme/eng")
	res, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip")
	if err != nil {
		t.Fatalf("the first GitHub login: %v", err)
	}
	if res.ActiveTeamID != "team-acme" || res.ActiveOrgID != "o-acme" || res.ActiveRole != identity.RoleMember {
		t.Fatalf("the first login landed in team=%q org=%q role=%q, want team-acme/o-acme/member", res.ActiveTeamID, res.ActiveOrgID, res.ActiveRole)
	}
	u, err := f.raw().GetUser(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("read the new account back: %v", err)
	}
	if u.DefaultTeamID != "team-acme" || u.DefaultOrgID != "o-acme" {
		t.Fatalf("the first login left default_team=%q default_org=%q, want team-acme/o-acme", u.DefaultTeamID, u.DefaultOrgID)
	}
	// One org-membership read (the login context's resolveOrgForTeam) and no
	// team-grant read at all: the grant wrote the rows it planned.
	if n := f.ids.count("GetOrgMembership"); n != 1 {
		t.Fatalf("the first login read the org membership %d time(s), want 1 (the login context's) — the grant of a fresh account reads no row of it", n)
	}
	if n := f.ids.count("GetMembership"); n != 0 {
		t.Fatalf("the first login read the team grant %d time(s), want 0 — a fresh account holds none", n)
	}
	if om, err := f.raw().GetOrgMembership(ctx, u.ID, "o-acme"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the grant's org mirror in o-acme: role=%q err=%v, want member", om.Role, err)
	}
	if mb, err := f.raw().GetMembership(ctx, u.ID, "team-acme"); err != nil || mb.Role != identity.RoleMember || mb.Source != identity.MembershipSourceGitHubSSO {
		t.Fatalf("the team grant: role=%q source=%q err=%v, want member/github_sso", mb.Role, mb.Source, err)
	}
}

// TestLoginWithExternalForOrg_AFirstLoginsFailedGrantReadCommitsNothing is
// the per-org SSO twin: the team read the grant needs runs before the
// account, so its failure commits neither the account nor the link.
func TestLoginWithExternalForOrg_AFirstLoginsFailedGrantReadCommitsNothing(t *testing.T) {
	ctx := context.Background()
	f := newPerOrgSSOFixture(t)
	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-new", Email: "new@example.org"}
	f.ids.fail("GetTeam", 1)
	if _, err := f.svc.LoginWithExternalForOrg(ctx, ext, "tb", "prov1", "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a first per-org SSO login whose team read failed: got %v, want the read's failure", err)
	}
	f.nothingCommitted(t, ext.Provider, ext.Subject, ext.Email)
	f.ids.disarm()
	res, err := f.svc.LoginWithExternalForOrg(ctx, ext, "tb", "prov1", "ua", "ip")
	if err != nil {
		t.Fatalf("the first login retried once the store answers: %v", err)
	}
	if res.ActiveTeamID != "tb" || res.ActiveOrgID != "o1" || res.ActiveRole != identity.RoleMember {
		t.Fatalf("the retried first login landed in team=%q org=%q role=%q, want tb/o1/member", res.ActiveTeamID, res.ActiveOrgID, res.ActiveRole)
	}
}

// TestLoginWithExternalForOrg_AFirstLoginReadsNoRowOfTheAccountItCreates is
// the per-org SSO twin of the read-nothing-you-know rule.
func TestLoginWithExternalForOrg_AFirstLoginReadsNoRowOfTheAccountItCreates(t *testing.T) {
	ctx := context.Background()
	f := newPerOrgSSOFixture(t)
	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-new2", Email: "new2@example.org"}
	res, err := f.svc.LoginWithExternalForOrg(ctx, ext, "tb", "prov1", "ua", "ip")
	if err != nil {
		t.Fatalf("the first per-org SSO login: %v", err)
	}
	if res.ActiveTeamID != "tb" || res.ActiveOrgID != "o1" {
		t.Fatalf("the first login landed in team=%q org=%q, want tb/o1", res.ActiveTeamID, res.ActiveOrgID)
	}
	if n := f.ids.count("GetOrgMembership"); n != 1 {
		t.Fatalf("the first login read the org membership %d time(s), want 1 (the login context's) — the grant of a fresh account reads no row of it", n)
	}
	if n := f.ids.count("GetMembership"); n != 0 {
		t.Fatalf("the first login read the team grant %d time(s), want 0 — a fresh account holds none", n)
	}
	if om, err := f.raw().GetOrgMembership(ctx, res.User.ID, "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the grant's org mirror in o1: role=%q err=%v, want member", om.Role, err)
	}
	if mb, err := f.raw().GetMembership(ctx, res.User.ID, "tb"); err != nil || mb.Role != identity.RoleMember || mb.Source != identity.MembershipSourceOIDCSSO {
		t.Fatalf("the team grant: role=%q source=%q err=%v, want member/oidc_sso", mb.Role, mb.Source, err)
	}
}

// TestLoginWithExternal_AReturningUsersGrantStillReadsTheirRows guards the
// other side of the same change: for an account the call did NOT create, the
// grant keeps its reads — a membership at or above the granted role is kept,
// never overwritten, and a role a read could not see is not one to write
// over (the round-2 owner-demotion rule).
func TestLoginWithExternal_AReturningUsersGrantStillReadsTheirRows(t *testing.T) {
	ctx := context.Background()
	f := newGitHubOrgFixture(t, identity.RoleMember)
	ext := githubExt("u3", "acme/*", "acme/eng")
	res, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip")
	if err != nil {
		t.Fatalf("the first GitHub login: %v", err)
	}
	// Promote the account above what the row grants, by hand, as an admin
	// would: the next login must not write the grant back down.
	if err := f.raw().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: res.User.ID, OrgID: "o-acme", Role: identity.OrgRoleOwner, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("promote in the org: %v", err)
	}
	if err := f.raw().UpsertMembership(ctx, identity.Membership{
		UserID: res.User.ID, TeamID: "team-acme", Role: identity.RoleOwner, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("promote in the team: %v", err)
	}
	if _, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip"); err != nil {
		t.Fatalf("the returning GitHub login: %v", err)
	}
	if om, err := f.raw().GetOrgMembership(ctx, res.User.ID, "o-acme"); err != nil || om.Role != identity.OrgRoleOwner {
		t.Fatalf("a returning login wrote the org role down to %q (err %v)", om.Role, err)
	}
	if mb, err := f.raw().GetMembership(ctx, res.User.ID, "team-acme"); err != nil || mb.Role != identity.RoleOwner {
		t.Fatalf("a returning login wrote the team role down to %q (err %v)", mb.Role, err)
	}
	// And a read of those rows that FAILS fails the login, rather than
	// passing for "holds nothing".
	f.ids.fail("GetOrgMembership", 0)
	if _, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a returning login whose org-membership read failed: got %v, want the read's failure", err)
	}
}
