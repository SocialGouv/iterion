package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// ownerFixture is an account that OWNS org o1, holding no grant in its team
// tb, on an identity store whose reads fail on chosen calls.
type ownerFixture struct {
	svc   *Service
	ids   *nthCallGate
	owner identity.User
}

// raw is the identity store BEHIND the gate: a test seeds and inspects rows
// through it, so those calls are neither counted nor failed.
func (f *ownerFixture) raw() identity.Store { return f.ids.Store }

func newOwnerFixture(t *testing.T) *ownerFixture {
	t.Helper()
	ctx := context.Background()
	ids := newNthCallGate(identity.NewMemoryStore(), errStoreUnreachable)
	seed := ids.Store
	svc := newTestServiceOn(t, ids, NewMemorySessionStore())
	svc.orgSSO = orgsso.NewMemoryStore()
	if _, err := seed.CreateOrg(ctx, identity.Org{ID: "o1", Name: "Org One", Slug: "org-one", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := seed.CreateTeam(ctx, identity.Team{ID: "tb", OrgID: "o1", Name: "Team B", Slug: "team-b"}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	reg, err := svc.Register(ctx, "owner@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := seed.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: reg.User.ID, OrgID: "o1", Role: identity.OrgRoleOwner, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("UpsertOrgMembership: %v", err)
	}
	return &ownerFixture{svc: svc, ids: ids, owner: reg.User}
}

// orgRole reads the owner's stored role in o1, past the gate.
func (f *ownerFixture) orgRole(t *testing.T) identity.OrgRole {
	t.Helper()
	om, err := f.raw().GetOrgMembership(context.Background(), f.owner.ID, "o1")
	if err != nil {
		t.Fatalf("read the owner's org membership: %v", err)
	}
	return om.Role
}

// TestAcceptInvitation_AFailedOrgReadNeverDemotesTheOwner drives an org owner
// accepting a member invitation to a team of their own org while the read of
// their org membership fails. The failed read is not "no membership": the
// accept must fail with nothing written — the owner still owner, the
// invitation unspent — and succeed once the store answers.
func TestAcceptInvitation_AFailedOrgReadNeverDemotesTheOwner(t *testing.T) {
	ctx := context.Background()
	f := newOwnerFixture(t)
	f.svc.SetTeamRankReconcile(func(context.Context, string, string, identity.Role) error { return nil })
	token, _, err := f.svc.CreateInvitation(mintVerdictCtx(ctx), "tb", f.owner.Email, identity.RoleMember, "someone-else")
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	f.ids.fail("GetOrgMembership", 0)
	if _, err := f.svc.AcceptInvitationForExistingUser(ctx, f.owner.ID, token); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("the accept whose org-membership read failed: got %v, want the read's failure", err)
	}
	f.ids.disarm()
	if role := f.orgRole(t); role != identity.OrgRoleOwner {
		t.Fatalf("a failed read of the owner's org membership left them %q in o1", role)
	}
	inv, err := f.raw().GetInvitationByTokenHash(ctx, hashInvitationToken(token))
	if err != nil || inv.AcceptedAt != nil {
		t.Fatalf("the failed accept spent the invitation: accepted_at=%v (err %v)", inv.AcceptedAt, err)
	}
	if _, err := f.raw().GetMembership(ctx, f.owner.ID, "tb"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the failed accept wrote the team grant: %v", err)
	}
	if _, err := f.svc.AcceptInvitationForExistingUser(ctx, f.owner.ID, token); err != nil {
		t.Fatalf("the accept retried once the store answers: %v", err)
	}
	if role := f.orgRole(t); role != identity.OrgRoleOwner {
		t.Fatalf("accepting a member invitation left the owner %q in o1", role)
	}
}

// TestLoginWithExternalForOrg_AFailedOrgReadNeverDemotesTheOwner drives the
// same read failing on a returning per-org SSO login of an org owner: every
// such login re-grants the provider's role, never below what the user
// holds — a role it failed to read is not one it may write over.
func TestLoginWithExternalForOrg_AFailedOrgReadNeverDemotesTheOwner(t *testing.T) {
	ctx := context.Background()
	f := newOwnerFixture(t)
	if err := f.svc.orgSSO.Create(ctx, orgsso.OrgSSOProvider{
		ID: "prov1", TenantID: "tb", Kind: orgsso.KindOIDC, Enabled: true,
		IssuerURL: "https://sso.example.org/realms/main", ClientID: "iterion",
		DefaultRole: identity.RoleMember, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the provider: %v", err)
	}
	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-owner", Email: f.owner.Email}
	if err := f.raw().UpsertOIDCLink(ctx, identity.OIDCLink{Provider: ext.Provider, ProviderUserID: ext.Subject, UserID: f.owner.ID, Email: f.owner.Email, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("link the SSO identity: %v", err)
	}
	f.ids.fail("GetOrgMembership", 0)
	if _, err := f.svc.LoginWithExternalForOrg(ctx, ext, "tb", "prov1", "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("the SSO login whose org-membership read failed: got %v, want the read's failure", err)
	}
	f.ids.disarm()
	if role := f.orgRole(t); role != identity.OrgRoleOwner {
		t.Fatalf("a failed read of the owner's org membership left them %q in o1", role)
	}
	if _, err := f.raw().GetMembership(ctx, f.owner.ID, "tb"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the failed SSO login wrote the team grant: %v", err)
	}
	res, err := f.svc.LoginWithExternalForOrg(ctx, ext, "tb", "prov1", "ua", "ip")
	if err != nil {
		t.Fatalf("the SSO login retried once the store answers: %v", err)
	}
	if role := f.orgRole(t); role != identity.OrgRoleOwner || res.ActiveOrgRole != identity.OrgRoleOwner {
		t.Fatalf("the SSO login left the owner %q (session says %q)", role, res.ActiveOrgRole)
	}
}

// TestRegister_ReadsNothingAfterTheAccountIsWritten drives registration with
// every read a login makes failing: the account the registration writes is
// the one it answers for, from the rows it wrote, so no read after the
// commit can fail it — and the session lands in the org and team the rows
// say, with the roles they say.
func TestRegister_ReadsNothingAfterTheAccountIsWritten(t *testing.T) {
	ctx := context.Background()
	t.Run("open signup", func(t *testing.T) {
		ids := newNthCallGate(identity.NewMemoryStore(), errStoreUnreachable)
		seed := ids.Store
		svc := newTestServiceOn(t, ids, NewMemorySessionStore())
		ids.fail("ListMembershipsByUser", 0).fail("GetTeam", 0).fail("GetOrgMembership", 0).fail("ListOrgMembershipsByUser", 0)
		res, err := svc.Register(ctx, "fresh@example.org", "correct-horse-9", "", "", "ua", "ip")
		if err != nil {
			t.Fatalf("a registration whose login reads would fail: %v", err)
		}
		ids.disarm()
		u, err := seed.GetUser(ctx, res.User.ID)
		if err != nil {
			t.Fatalf("read the registered account: %v", err)
		}
		if res.ActiveTeamID == "" || res.ActiveTeamID != u.DefaultTeamID || res.ActiveOrgID != u.DefaultOrgID ||
			res.ActiveRole != identity.RoleOwner || res.ActiveOrgRole != identity.OrgRoleOwner {
			t.Fatalf("the registration landed in team %q (%s) of org %q (%s), want its personal team %q and org %q as owner",
				res.ActiveTeamID, res.ActiveRole, res.ActiveOrgID, res.ActiveOrgRole, u.DefaultTeamID, u.DefaultOrgID)
		}
	})
	t.Run("invitation", func(t *testing.T) {
		f := newInviteFixture(t, identity.RoleMember)
		f.ids.fail("ListMembershipsByUser", 0).fail("ListOrgMembershipsByUser", 0)
		res, err := f.svc.Register(ctx, "invitee@example.org", "correct-horse-9", "", f.token, "ua", "ip")
		if err != nil {
			t.Fatalf("a registration whose login reads would fail: %v", err)
		}
		if res.ActiveTeamID != "tb" || res.ActiveRole != identity.RoleMember || res.ActiveOrgID != "o1" || res.ActiveOrgRole != identity.OrgRoleMember {
			t.Fatalf("the registration landed in team %q (%s) of org %q (%s), want tb (member) of o1 (member)",
				res.ActiveTeamID, res.ActiveRole, res.ActiveOrgID, res.ActiveOrgRole)
		}
	})
}

// TestRegister_WithAnInvitationReadsBeforeItWrites drives the reads an
// invitation registration makes — the team the invitation grants, the org
// membership it mirrors up to — failing: no account is created and the
// invitation is not spent, so the same registration succeeds once the store
// answers, instead of answering "email already taken" to its retry.
func TestRegister_WithAnInvitationReadsBeforeItWrites(t *testing.T) {
	for _, read := range []string{"GetTeam", "GetOrgMembership"} {
		t.Run(read, func(t *testing.T) {
			ctx := context.Background()
			f := newInviteFixture(t, identity.RoleMember)
			f.ids.fail(read, 0)
			if _, err := f.svc.Register(ctx, "invitee@example.org", "correct-horse-9", "", f.token, "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("the registration whose %s failed: got %v, want the read's failure", read, err)
			}
			f.ids.disarm()
			if _, err := f.raw().GetUserByEmail(ctx, "invitee@example.org"); !errors.Is(err, identity.ErrNotFound) {
				t.Fatalf("the failed registration left an account behind: %v", err)
			}
			if inv, err := f.raw().GetInvitationByTokenHash(ctx, hashInvitationToken(f.token)); err != nil || inv.AcceptedAt != nil {
				t.Fatalf("the failed registration spent the invitation: accepted_at=%v (err %v)", inv.AcceptedAt, err)
			}
			if _, err := f.svc.Register(ctx, "invitee@example.org", "correct-horse-9", "", f.token, "ua", "ip"); err != nil {
				t.Fatalf("the registration retried once the store answers: %v", err)
			}
		})
	}
}

// inviteFixture is an invite-only service with team tb of org o1 and an
// invitation to it for invitee@example.org.
type inviteFixture struct {
	svc   *Service
	ids   *nthCallGate
	token string
}

// raw is the identity store BEHIND the gate — see ownerFixture.raw.
func (f *inviteFixture) raw() identity.Store { return f.ids.Store }

func newInviteFixture(t *testing.T, role identity.Role) *inviteFixture {
	t.Helper()
	ctx := context.Background()
	ids := newNthCallGate(identity.NewMemoryStore(), errStoreUnreachable)
	seed := ids.Store
	svc, err := NewService(Config{Store: ids, Sessions: NewMemorySessionStore(), Signer: newTestSigner(t), SignupMode: SignupInviteOnly, RefreshTTL: 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := seed.CreateOrg(ctx, identity.Org{ID: "o1", Name: "Org One", Slug: "org-one", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	if _, err := seed.CreateTeam(ctx, identity.Team{ID: "tb", OrgID: "o1", Name: "Team B", Slug: "team-b"}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	token, _, err := svc.CreateInvitation(mintVerdictCtx(ctx), "tb", "invitee@example.org", role, "inviter")
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	return &inviteFixture{svc: svc, ids: ids, token: token}
}

// failingProviders is an org SSO provider store whose Get fails.
type failingProviders struct {
	orgsso.Store
}

func (failingProviders) Get(context.Context, string) (orgsso.OrgSSOProvider, error) {
	return orgsso.OrgSSOProvider{}, fmt.Errorf("orgsso: get provider: %w", errStoreUnreachable)
}

// TestLoginWithExternalForOrg_AProviderReadFailureIsAFailure: a per-org SSO
// login whose provider row cannot be read is answered as the failure it is,
// not as a provider that does not exist.
func TestLoginWithExternalForOrg_AProviderReadFailureIsAFailure(t *testing.T) {
	f := newOwnerFixture(t)
	f.svc.orgSSO = failingProviders{Store: orgsso.NewMemoryStore()}
	ext := oidc.ExternalUser{Provider: "oidc-org-prov1", Subject: "kc-owner", Email: f.owner.Email}
	_, err := f.svc.LoginWithExternalForOrg(context.Background(), ext, "tb", "prov1", "ua", "ip")
	if !errors.Is(err, errStoreUnreachable) || errors.Is(err, oidc.ErrUnknownProvider) {
		t.Fatalf("a login whose provider read failed: got %v, want the read's failure, not ErrUnknownProvider", err)
	}
}

// TestCreateInvitation_ATeamReadFailureIsAFailure: minting an invitation for
// a team that cannot be read is answered as the failure it is, not as a team
// that does not exist.
func TestCreateInvitation_ATeamReadFailureIsAFailure(t *testing.T) {
	f := newOwnerFixture(t)
	f.ids.fail("GetTeam", 0)
	_, _, err := f.svc.CreateInvitation(mintVerdictCtx(context.Background()), "tb", "x@example.org", identity.RoleMember, f.owner.ID)
	if !errors.Is(err, errStoreUnreachable) || errors.Is(err, ErrTeamNotFound) {
		t.Fatalf("an invitation whose team read failed: got %v, want the read's failure, not ErrTeamNotFound", err)
	}
	f.ids.disarm()
	if _, _, err := f.svc.CreateInvitation(mintVerdictCtx(context.Background()), "t-missing", "x@example.org", identity.RoleMember, f.owner.ID); !errors.Is(err, ErrTeamNotFound) {
		t.Fatalf("an invitation for a team that does not exist: got %v, want ErrTeamNotFound", err)
	}
}

// TestLoginGitHub_ANewUsersTeamReadFailureIsAFailure: the first login of a
// gated GitHub identity reads the granted team twice — once for the grant,
// which also resolves the account's default org (planOrgGrantForNewAccount,
// before any write), and once for the login context it answers with
// (resolveOrgForTeam). A read that fails at EITHER is returned, never taken
// for "the team has no org". The first read's own guarantee — a failure
// there commits nothing — is
// TestLoginGitHub_AFirstLoginsFailedGrantReadCommitsNothing.
func TestLoginGitHub_ANewUsersTeamReadFailureIsAFailure(t *testing.T) {
	svc, _ := githubFixture(t, identity.RoleMember)
	ids := newNthCallGate(svc.store, errStoreUnreachable)
	svc.store = ids
	ids.fail("GetTeam", 2)
	if _, err := svc.LoginWithExternal(context.Background(), githubExt("u1", "acme/*", "acme/eng"), "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a first GitHub login whose team read failed: got %v, want the read's failure", err)
	}
	if n := ids.count("GetTeam"); n != 2 {
		t.Fatalf("the login read the team %d time(s) before failing, want 2 (the grant's, then the login context's)", n)
	}
}
