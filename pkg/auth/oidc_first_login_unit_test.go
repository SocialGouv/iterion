package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The three SSO FIRST logins each end on the same cosmetic write — the
// landing defaults and the LastLoginAt stamp of an account that call itself
// created — and the tree held three different answers to it failing: two
// fatal (the global new user, the submitter) and one tolerant but SILENT
// (the per-org new user). They now share one tolerant writer that logs
// (patchBestEffort): the account, its SSO link and its grants are already
// written, and an SSO first login that answers an error over them cannot be
// retried — the second attempt finds the email taken and, with no
// auto-link provider trusted (the default), answers 409 link_required for
// good.

// seedGitHubGatingOn arms GitHub team-gating for the service's store: one
// enabled github row granting `role` on acme/eng to the team it creates.
func seedGitHubGatingOn(t *testing.T, svc *Service, seed identity.Store, role identity.Role) string {
	t.Helper()
	ctx := context.Background()
	store := orgsso.NewMemoryStore()
	svc.orgSSO = store
	team, err := seed.CreateTeam(ctx, identity.Team{ID: "team-acme", Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatalf("team: %v", err)
	}
	if err := store.Create(ctx, orgsso.OrgSSOProvider{
		ID: "gh1", TenantID: team.ID, Kind: orgsso.KindGitHub, Enabled: true, AutoProvision: true,
		Grants:    []orgsso.GitHubTeamGrant{{GitHubOrg: "acme", TeamSlug: "eng", Role: role, Verified: true}},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("github row: %v", err)
	}
	return team.ID
}

// seedOrgOIDCProviderOn arms a per-org OIDC trust root (a tenant's own
// Keycloak) for the service's store.
func seedOrgOIDCProviderOn(t *testing.T, svc *Service, seed identity.Store, role identity.Role) string {
	t.Helper()
	ctx := context.Background()
	store := orgsso.NewMemoryStore()
	svc.orgSSO = store
	team, err := seed.CreateTeam(ctx, identity.Team{ID: "team-acme", Name: "Acme", Slug: "acme"})
	if err != nil {
		t.Fatalf("team: %v", err)
	}
	if err := store.Create(ctx, orgsso.OrgSSOProvider{
		ID: "prov1", TenantID: team.ID, Kind: orgsso.KindOIDC, Enabled: true,
		IssuerURL: "https://sso.acme.example/realms/main", ClientID: "iterion",
		DefaultRole: role, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("provider: %v", err)
	}
	return team.ID
}

// TestSSOFirstLogin_aFailedStampStillAnswersTheLogin: one isolate per first
// -login path, each failing the ONE write that comes after everything that
// matters is committed.
func TestSSOFirstLogin_aFailedStampStillAnswersTheLogin(t *testing.T) {
	ctx := context.Background()

	t.Run("the global new user, with its personal team", func(t *testing.T) {
		svc, gate, logs := newLoggingGatedService(t, SignupOpen)
		gate.fail("PatchUser", 0)
		ext := oidc.ExternalUser{Provider: "sso", Subject: "kc-alice", Email: "alice@example.com", Name: "Alice"}

		res, err := svc.LoginWithExternal(ctx, ext, "ua", "ip")
		if err != nil {
			t.Fatalf("LoginWithExternal: %v — the account, its link and its personal space are written: the stamp failing is no reason to answer an error over them", err)
		}
		if res.ActiveTeamID == "" {
			t.Fatalf("the first login answered no active team: %+v", res)
		}
		u, err := gate.raw().GetUserByEmail(ctx, "alice@example.com")
		if err != nil {
			t.Fatalf("the account is not in the store: %v", err)
		}
		if _, err := gate.raw().GetOIDCLink(ctx, "sso", "kc-alice"); err != nil {
			t.Fatalf("the SSO link is not in the store: %v", err)
		}
		assertStampFailureLogged(t, logs.String(), u.ID, "landing defaults")
	})

	t.Run("the submitter, whose stamp is the only write left", func(t *testing.T) {
		svc, gate, logs := newLoggingGatedService(t, SignupInviteOnly)
		seedGitHubGatingOn(t, svc, gate.raw(), identity.RoleMember)
		svc.githubUngrantedPolicy = GitHubUngrantedSubmitter
		gate.fail("PatchUser", 0)

		res, err := svc.LoginWithExternal(ctx, githubExt("submitter", "other/*"), "ua", "ip")
		if err != nil {
			t.Fatalf("LoginWithExternal: %v — the submitter account and its link are written", err)
		}
		if res.ActiveTeamID != "" {
			t.Errorf("a submitter must be teamless, got active team %q", res.ActiveTeamID)
		}
		u, err := gate.raw().GetUserByEmail(ctx, "submitter@x.example")
		if err != nil {
			t.Fatalf("the submitter account is not in the store: %v", err)
		}
		if _, err := gate.raw().GetOIDCLink(ctx, "github", "submitter"); err != nil {
			t.Fatalf("the SSO link is not in the store: %v", err)
		}
		assertStampFailureLogged(t, logs.String(), u.ID, "stamp")
	})

	t.Run("the per-org new user, whose tolerance was silent", func(t *testing.T) {
		svc, gate, logs := newLoggingGatedService(t, SignupInviteOnly)
		teamID := seedOrgOIDCProviderOn(t, svc, gate.raw(), identity.RoleMember)
		gate.fail("PatchUser", 0)
		ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-bob", Email: "bob@acme.example", Name: "Bob"}

		res, err := svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip")
		if err != nil {
			t.Fatalf("LoginWithExternalForOrg: %v", err)
		}
		if res.ActiveTeamID != teamID {
			t.Errorf("active team = %q, want the org's team %q", res.ActiveTeamID, teamID)
		}
		u, err := gate.raw().GetUserByEmail(ctx, "bob@acme.example")
		if err != nil {
			t.Fatalf("the onboarded account is not in the store: %v", err)
		}
		// The site was already tolerant: what it owed, and never answered,
		// is the cause — logged nowhere at all.
		assertStampFailureLogged(t, logs.String(), u.ID, "stamp")
	})
}

// TestSSOFirstLogin_theStampLandsWhenTheStoreAnswers is the mirror of all
// three: the tolerance must not cover a writer that stopped writing.
func TestSSOFirstLogin_theStampLandsWhenTheStoreAnswers(t *testing.T) {
	ctx := context.Background()

	t.Run("the global new user", func(t *testing.T) {
		svc, gate, logs := newLoggingGatedService(t, SignupOpen)
		ext := oidc.ExternalUser{Provider: "sso", Subject: "kc-alice", Email: "alice@example.com", Name: "Alice"}
		res, err := svc.LoginWithExternal(ctx, ext, "ua", "ip")
		if err != nil {
			t.Fatalf("LoginWithExternal: %v", err)
		}
		u, err := gate.raw().GetUserByEmail(ctx, "alice@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if u.LastLoginAt == nil || u.DefaultTeamID != res.ActiveTeamID || u.DefaultOrgID == "" {
			t.Fatalf("stored stamp = %v / team %q / org %q, want the stamp and the answered team %q", u.LastLoginAt, u.DefaultTeamID, u.DefaultOrgID, res.ActiveTeamID)
		}
		if logged := logs.String(); strings.Contains(logged, "could not be written") {
			t.Errorf("a first login that wrote its stamp logged a failure: %q", logged)
		}
	})

	t.Run("the submitter", func(t *testing.T) {
		svc, gate, _ := newLoggingGatedService(t, SignupInviteOnly)
		seedGitHubGatingOn(t, svc, gate.raw(), identity.RoleMember)
		svc.githubUngrantedPolicy = GitHubUngrantedSubmitter
		if _, err := svc.LoginWithExternal(ctx, githubExt("submitter", "other/*"), "ua", "ip"); err != nil {
			t.Fatalf("LoginWithExternal: %v", err)
		}
		u, err := gate.raw().GetUserByEmail(ctx, "submitter@x.example")
		if err != nil {
			t.Fatal(err)
		}
		if u.LastLoginAt == nil {
			t.Error("last_login_at is unset on a submitter signup whose store answered: the account reads as never signed in")
		}
	})

	t.Run("the per-org new user", func(t *testing.T) {
		svc, gate, _ := newLoggingGatedService(t, SignupInviteOnly)
		teamID := seedOrgOIDCProviderOn(t, svc, gate.raw(), identity.RoleMember)
		ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-bob", Email: "bob@acme.example", Name: "Bob"}
		if _, err := svc.LoginWithExternalForOrg(ctx, ext, teamID, "prov1", "ua", "ip"); err != nil {
			t.Fatalf("LoginWithExternalForOrg: %v", err)
		}
		u, err := gate.raw().GetUserByEmail(ctx, "bob@acme.example")
		if err != nil {
			t.Fatal(err)
		}
		if u.LastLoginAt == nil {
			t.Error("last_login_at is unset on a per-org SSO signup whose store answered")
		}
	})
}

// assertStampFailureLogged holds the one thing a tolerated write may never
// be: silent. The account id and the store's own cause have to be there,
// since the row an operator reads afterwards no longer says any of it.
func assertStampFailureLogged(t *testing.T, logged, userID, what string) {
	t.Helper()
	if !strings.Contains(logged, userID) {
		t.Errorf("the unwritten patch was not logged with its account: %q, want %s", logged, userID)
	}
	if !strings.Contains(logged, "PatchUser") {
		t.Errorf("the log does not carry the store's own cause: %q", logged)
	}
	if !strings.Contains(logged, what) {
		t.Errorf("the log does not name what could not be written: %q, want %q", logged, what)
	}
}
