package auth

import (
	"context"
	"errors"
	"strings"
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
	// The signup is a sign-in: provisionSubmitter's own patch carries the
	// stamp, with no login path behind it to make up for a loss.
	if u.LastLoginAt == nil {
		t.Error("last_login_at is unset on a submitter signup: the account reads as never signed in")
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

	// The sweep this reconcile owes, recorded: it takes the rows kept on
	// each grant it revokes (SetTeamGrantSweep).
	type swept struct{ userID, teamID, reason string }
	var sweeps []swept
	svc.SetTeamGrantSweep(func(_ context.Context, userID, teamID, reason string) error {
		sweeps = append(sweeps, swept{userID, teamID, reason})
		return nil
	})

	// Re-login (returning user) with groups that no longer match acme/eng.
	if _, err := svc.LoginWithExternal(ctx, githubExt("u3", "other/*"), "ua", "ip"); err != nil {
		t.Fatalf("relogin: %v", err)
	}
	// The revoked grant's rows were swept, under the cause naming the write,
	// and the human-created membership — never revoked — asked for nothing.
	if len(sweeps) != 1 || sweeps[0] != (swept{res.User.ID, teamID, MembershipRevokedBySSOReason}) {
		t.Errorf("the reconcile swept %+v, want exactly the %s grant of %s under %q", sweeps, teamID, res.User.ID, MembershipRevokedBySSOReason)
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

// A revoke with no sweep registered is NOT a revoke: taking the grant and
// leaving the pending invitations kept on it standing is the state the sweep
// exists to prevent, so the reconcile refuses before it deletes anything and
// the login fails with it — a half-revoke nobody is told about would be a
// re-entry ticket into the team the grant was taken from.
func TestLoginGitHub_aRevokeWithNoSweepRegisteredIsRefused(t *testing.T) {
	svc, teamID := githubFixture(t, identity.RoleMember)
	ctx := context.Background()

	res, err := svc.LoginWithExternal(ctx, githubExt("u4", "acme/*", "acme/eng"), "ua", "ip")
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	if _, err := svc.store.GetMembership(ctx, res.User.ID, teamID); err != nil {
		t.Fatalf("the grant the allow-list wrote: %v", err)
	}

	_, err = svc.LoginWithExternal(ctx, githubExt("u4", "other/*"), "ua", "ip")
	if err == nil {
		t.Fatalf("the login that no longer matches answered no error — then the grant was revoked with the rows kept on it unswept")
	}
	for _, want := range []string{"no team-grant invitation sweep is registered", teamID, res.User.ID, "SetTeamGrantSweep"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
	// And nothing was taken: the refusal comes before the delete.
	if _, err := svc.store.GetMembership(ctx, res.User.ID, teamID); err != nil {
		t.Errorf("the grant was revoked anyway (%v) — a refusal after the delete leaves exactly the state it refuses", err)
	}
}

// A sweep that fails there fails the login. The reconcile revokes the stale
// grant BEFORE it sweeps, so a sweep that cannot take the rows kept on that
// grant leaves them standing on a grant that is gone — the re-entry ticket
// the seam exists to prevent. The login answers that failure, naming the
// team, the holder and the sweep's own cause, so the residue is reported
// rather than left behind a login that looks clean.
func TestLoginGitHub_aSweepThatFailsTakesTheLoginWithIt(t *testing.T) {
	svc, teamID := githubFixture(t, identity.RoleMember)
	ctx := context.Background()

	res, err := svc.LoginWithExternal(ctx, githubExt("u5", "acme/*", "acme/eng"), "ua", "ip")
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	if _, err := svc.store.GetMembership(ctx, res.User.ID, teamID); err != nil {
		t.Fatalf("the grant the allow-list wrote: %v", err)
	}

	sweepErr := errors.New("listing this team's invitations failed")
	calls := 0
	svc.SetTeamGrantSweep(func(_ context.Context, _, _, _ string) error {
		calls++
		return sweepErr
	})

	_, err = svc.LoginWithExternal(ctx, githubExt("u5", "other/*"), "ua", "ip")
	if err == nil {
		t.Fatalf("the login answered no error with the sweep failing — then the rows kept on the revoked grant stand and nobody is told")
	}
	if calls != 1 {
		t.Errorf("the reconcile called the sweep %d times, want 1 — then this is not the failure the test describes", calls)
	}
	if !errors.Is(err, sweepErr) {
		t.Errorf("the login's error does not carry the sweep's own cause: %v", err)
	}
	for _, want := range []string{teamID, res.User.ID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the login's error does not name %q: %v", want, err)
		}
	}
	// The delete precedes the sweep, so the grant is gone: what the failed
	// login reports is a row kept on a grant that no longer exists.
	if _, err := svc.store.GetMembership(ctx, res.User.ID, teamID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("the grant reads %v, want it gone — the reconcile deletes before it sweeps, which is what makes the unswept rows a residue", err)
	}
}
