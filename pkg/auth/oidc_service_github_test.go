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

// githubFixture wires an invite-only service (to prove github gating onboards
// regardless of SignupMode) with one enabled github row granting `role` to
// members of acme/eng. Returns the service + the team id.
func githubFixture(t *testing.T, role identity.Role) (*Service, string) {
	t.Helper()
	svc := newTestService(t, SignupInviteOnly)
	store := orgsso.NewMemoryStore()
	svc.orgSSO = store
	ctx := context.Background()
	team, err := svc.store.CreateTeam(ctx, identity.Team{ID: "team-acme", Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatalf("team: %v", err)
	}
	if err := store.Create(ctx, orgsso.OrgSSOProvider{
		ID: "gh1", TenantID: team.ID, Kind: orgsso.KindGitHub, Enabled: true, AutoProvision: true,
		Grants: []orgsso.GitHubTeamGrant{{GitHubOrg: "acme", TeamSlug: "eng", Role: role, Verified: true}}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("github row: %v", err)
	}
	return svc, team.ID
}

func githubExt(subject string, groups ...string) oidc.ExternalUser {
	return oidc.ExternalUser{Provider: "github", Subject: subject, Email: subject + "@x.example", Name: subject, Groups: groups}
}

func TestLoginGitHub_NewUserMatchedOnboarded(t *testing.T) {
	svc, teamID := githubFixture(t, identity.RoleMember)
	res, err := svc.LoginWithExternal(context.Background(), githubExt("u1", "acme/*", "acme/eng"), "ua", "ip")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.ActiveTeamID != teamID || res.ActiveRole != identity.RoleMember {
		t.Errorf("team=%q role=%q, want %q/member", res.ActiveTeamID, res.ActiveRole, teamID)
	}
}

func TestLoginGitHub_NewUserNoMatchRestrictedNoOrphan(t *testing.T) {
	svc, _ := githubFixture(t, identity.RoleMember)
	ctx := context.Background()
	ext := githubExt("intruder", "other/*")
	_, err := svc.LoginWithExternal(ctx, ext, "ua", "ip")
	if !errors.Is(err, ErrSSORestricted) {
		t.Fatalf("expected ErrSSORestricted, got %v", err)
	}
	// No orphan account must have been created.
	if _, err := svc.store.GetUserByEmail(ctx, "intruder@x.example"); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("orphan account created for restricted login: %v", err)
	}
}

func TestLoginGitHub_NewUserNoMatchSubmitterTeamless(t *testing.T) {
	// With GitHubUngrantedPolicy=submitter, a gated GitHub login that matches
	// no allow-listed team is ADMITTED as a teamless account (no personal
	// team) instead of refused — the public "submit a bot" tier.
	svc, _ := githubFixture(t, identity.RoleMember)
	svc.githubUngrantedPolicy = GitHubUngrantedSubmitter
	ctx := context.Background()
	res, err := svc.LoginWithExternal(ctx, githubExt("submitter", "other/*"), "ua", "ip")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.ActiveTeamID != "" {
		t.Errorf("submitter should be teamless, got active team %q", res.ActiveTeamID)
	}
	u, err := svc.store.GetUserByEmail(ctx, "submitter@x.example")
	if err != nil {
		t.Fatalf("submitter account not created: %v", err)
	}
	if u.DefaultTeamID != "" {
		t.Errorf("submitter must have no personal team, got %q", u.DefaultTeamID)
	}
	// The OIDC link is persisted, so a returning login finds the same user.
	res2, err := svc.LoginWithExternal(ctx, githubExt("submitter", "other/*"), "ua", "ip")
	if err != nil {
		t.Fatalf("relogin: %v", err)
	}
	if res2.User.ID != u.ID {
		t.Errorf("relogin created a different user: %q vs %q", res2.User.ID, u.ID)
	}
}

func TestLoginGitHub_SubmitterUpgradesWhenAddedToTeam(t *testing.T) {
	// A teamless submitter who later joins an allow-listed GitHub team is
	// upgraded to full org membership on their next login.
	svc, teamID := githubFixture(t, identity.RoleMember)
	svc.githubUngrantedPolicy = GitHubUngrantedSubmitter
	ctx := context.Background()
	if _, err := svc.LoginWithExternal(ctx, githubExt("grow", "other/*"), "ua", "ip"); err != nil {
		t.Fatalf("first (teamless) login: %v", err)
	}
	// The user now belongs to acme/eng.
	res, err := svc.LoginWithExternal(ctx, githubExt("grow", "acme/*", "acme/eng"), "ua", "ip")
	if err != nil {
		t.Fatalf("upgrade login: %v", err)
	}
	if _, err := svc.store.GetMembership(ctx, res.User.ID, teamID); err != nil {
		t.Errorf("teamless submitter not upgraded into team on join: %v", err)
	}
}

func TestLoginGitHub_NoGatingFallsThrough(t *testing.T) {
	// Invite-only service, NO github rows → a github login behaves as before
	// (ErrSignupClosed for a brand-new user), not ErrSSORestricted.
	svc := newTestService(t, SignupInviteOnly)
	svc.orgSSO = orgsso.NewMemoryStore()
	_, err := svc.LoginWithExternal(context.Background(), githubExt("nobody", "acme/eng"), "ua", "ip")
	if !errors.Is(err, ErrSignupClosed) {
		t.Fatalf("expected ErrSignupClosed (no gating), got %v", err)
	}
}

func TestLoginGitHub_ReturningUserPicksUpNewOrg(t *testing.T) {
	svc, _ := githubFixture(t, identity.RoleMember)
	ctx := context.Background()
	ext := githubExt("u2", "acme/*", "acme/eng", "beta/*")
	if _, err := svc.LoginWithExternal(ctx, ext, "ua", "ip"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	// Admin adds a second org's allow-list matching this user's beta membership.
	beta, _ := svc.store.CreateTeam(ctx, identity.Team{ID: "team-beta", Name: "Beta", Slug: "beta"})
	if err := svc.orgSSO.Create(ctx, orgsso.OrgSSOProvider{
		ID: "gh2", TenantID: beta.ID, Kind: orgsso.KindGitHub, Enabled: true, AutoProvision: true,
		Grants: []orgsso.GitHubTeamGrant{{GitHubOrg: "beta", TeamSlug: "*", Role: identity.RoleMember, Verified: true}}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Re-login: the returning user is now also granted into beta.
	res, err := svc.LoginWithExternal(ctx, ext, "ua", "ip")
	if err != nil {
		t.Fatalf("relogin: %v", err)
	}
	if _, err := svc.store.GetMembership(ctx, res.User.ID, beta.ID); err != nil {
		t.Errorf("returning user not granted into newly-added org: %v", err)
	}
}

func TestLoginGitHub_RevokesStaleGrant(t *testing.T) {
	svc, teamID := githubFixture(t, identity.RoleMember) // gh1: acme/eng → member of team-acme
	ctx := context.Background()
	beta, _ := svc.store.CreateTeam(ctx, identity.Team{ID: "team-beta", Name: "Beta", Slug: "beta"})

	// First login: user matches acme/eng → github_sso membership in team-acme.
	res, err := svc.LoginWithExternal(ctx, githubExt("u3", "acme/*", "acme/eng"), "ua", "ip")
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	m, _ := svc.store.GetMembership(ctx, res.User.ID, teamID)
	if m.Source != identity.MembershipSourceGitHubSSO {
		t.Fatalf("acme membership source = %q, want github_sso", m.Source)
	}
	// A human (invitation) membership in another team must survive reconciliation.
	if err := svc.store.UpsertMembership(ctx, identity.Membership{
		UserID: res.User.ID, TeamID: beta.ID, Role: identity.RoleMember, InvitedBy: "admin", JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Re-login (returning user) with groups that no longer match acme/eng.
	if _, err := svc.LoginWithExternal(ctx, githubExt("u3", "other/*"), "ua", "ip"); err != nil {
		t.Fatalf("relogin: %v", err)
	}
	// The stale github_sso membership is revoked...
	if _, err := svc.store.GetMembership(ctx, res.User.ID, teamID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("stale github_sso membership not revoked: %v", err)
	}
	// ...but the human-created membership is untouched.
	if _, err := svc.store.GetMembership(ctx, res.User.ID, beta.ID); err != nil {
		t.Errorf("human membership wrongly revoked: %v", err)
	}
}

func TestLoginGitHub_WildcardGrant(t *testing.T) {
	// A grant with team_slug "*" matches any member of the org.
	svc := newTestService(t, SignupInviteOnly)
	store := orgsso.NewMemoryStore()
	svc.orgSSO = store
	ctx := context.Background()
	team, _ := svc.store.CreateTeam(ctx, identity.Team{ID: "t", Name: "T", Slug: "t"})
	_ = store.Create(ctx, orgsso.OrgSSOProvider{
		ID: "g", TenantID: team.ID, Kind: orgsso.KindGitHub, Enabled: true, AutoProvision: true,
		Grants: []orgsso.GitHubTeamGrant{{GitHubOrg: "acme", TeamSlug: "*", Role: identity.RoleMember, Verified: true}}, CreatedAt: time.Now(),
	})
	// User is an org member (acme/*) but in no specific allow-listed team.
	res, err := svc.LoginWithExternal(ctx, githubExt("w", "acme/*"), "ua", "ip")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.ActiveTeamID != team.ID {
		t.Errorf("wildcard grant did not admit org member: team=%q", res.ActiveTeamID)
	}
}
