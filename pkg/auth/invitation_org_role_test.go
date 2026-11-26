package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// orgTeamFixture is a service whose team "team-x" belongs to org "org-x",
// with an inviter account.
type orgTeamFixture struct {
	svc     *Service
	inviter identity.User
}

func newOrgTeamFixture(t *testing.T) orgTeamFixture {
	t.Helper()
	ctx := context.Background()
	svc := newTestService(t, SignupInviteOnly)
	inviter, _, err := svc.CreateUserAndPersonalTeam(ctx, "inviter@example.org", "Inviter", "correcthorse", true, identity.UserStatusActive)
	if err != nil {
		t.Fatalf("bootstrap inviter: %v", err)
	}
	if _, err := svc.store.CreateOrg(ctx, identity.Org{ID: "org-x", Name: "Org X", Slug: "org-x"}); err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := svc.store.CreateTeam(ctx, identity.Team{ID: "team-x", Name: "Team X", Slug: "team-x", OrgID: "org-x"}); err != nil {
		t.Fatalf("create team: %v", err)
	}
	return orgTeamFixture{svc: svc, inviter: inviter}
}

func (f orgTeamFixture) orgRoleOf(t *testing.T, userID string) identity.OrgRole {
	t.Helper()
	om, err := f.svc.store.GetOrgMembership(context.Background(), userID, "org-x")
	if err != nil {
		t.Fatalf("read the org membership of %s: %v", userID, err)
	}
	return om.Role
}

func (f orgTeamFixture) teamRoleOf(t *testing.T, userID string) identity.Role {
	t.Helper()
	m, err := f.svc.store.GetMembership(context.Background(), userID, "team-x")
	if err != nil {
		t.Fatalf("read the team grant of %s: %v", userID, err)
	}
	return m.Role
}

// TestAcceptedInvitation_WritesTheOrgRoleItNames: accepting writes the team
// grant the invitation names and the org membership it names — an org member
// for a team invitation whatever its team rank, the named role for an org
// one — on both accept paths (registration, existing account).
func TestAcceptedInvitation_WritesTheOrgRoleItNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		role     identity.Role
		orgRole  identity.OrgRole
		existing bool
		wantOrg  identity.OrgRole
	}{
		{name: "team admin invitation, registration", role: identity.RoleAdmin, wantOrg: identity.OrgRoleMember},
		{name: "team owner invitation, existing account", role: identity.RoleOwner, existing: true, wantOrg: identity.OrgRoleMember},
		{name: "org admin named, registration", role: identity.RoleMember, orgRole: identity.OrgRoleAdmin, wantOrg: identity.OrgRoleAdmin},
		{name: "org owner named, existing account", role: identity.RoleViewer, orgRole: identity.OrgRoleOwner, existing: true, wantOrg: identity.OrgRoleOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newOrgTeamFixture(t)
			tok, inv, err := f.svc.CreateInvitation(mintVerdictCtx(ctx), InvitationRequest{TeamID: "team-x", Email: "invitee@example.org", Role: tc.role, OrgRole: tc.orgRole, InvitedBy: f.inviter.ID})
			if err != nil {
				t.Fatalf("CreateInvitation: %v", err)
			}
			if inv.OrgRole != tc.wantOrg {
				t.Fatalf("the invitation row names org role %q, want %q", inv.OrgRole, tc.wantOrg)
			}
			var userID string
			if tc.existing {
				u, _, err := f.svc.CreateUserAndPersonalTeam(ctx, "invitee@example.org", "Invitee", "correcthorse", false, identity.UserStatusActive)
				if err != nil {
					t.Fatalf("create invitee: %v", err)
				}
				if _, err := f.svc.AcceptInvitationForExistingUser(ctx, u.ID, tok); err != nil {
					t.Fatalf("AcceptInvitationForExistingUser: %v", err)
				}
				userID = u.ID
			} else {
				res, err := f.svc.Register(ctx, "invitee@example.org", "correcthorse", "Invitee", tok, "ua", "ip")
				if err != nil {
					t.Fatalf("Register: %v", err)
				}
				userID = res.User.ID
			}
			if got := f.teamRoleOf(t, userID); got != tc.role {
				t.Errorf("team grant = %q, want %q", got, tc.role)
			}
			if got := f.orgRoleOf(t, userID); got != tc.wantOrg {
				t.Errorf("org membership = %q, want %q", got, tc.wantOrg)
			}
		})
	}
}

// TestAcceptedInvitation_ALegacyAdminRowWritesAnOrgMember: a row minted
// before invitations named their org role (OrgRole empty) at team role admin
// writes an org member, not an org admin.
func TestAcceptedInvitation_ALegacyAdminRowWritesAnOrgMember(t *testing.T) {
	ctx := context.Background()
	f := newOrgTeamFixture(t)
	tok, inv, err := f.svc.CreateInvitation(mintVerdictCtx(ctx), InvitationRequest{TeamID: "team-x", Email: "legacy@example.org", Role: identity.RoleAdmin, InvitedBy: f.inviter.ID})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	inv.OrgRole = ""
	if err := f.svc.store.UpdateInvitation(ctx, inv); err != nil {
		t.Fatalf("strip the org role to the legacy shape: %v", err)
	}
	res, err := f.svc.Register(ctx, "legacy@example.org", "correcthorse", "Legacy", tok, "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := f.orgRoleOf(t, res.User.ID); got != identity.OrgRoleMember {
		t.Fatalf("a legacy team-admin invitation wrote org %q, want member", got)
	}
}

// TestAcceptedInvitation_KeepsAHigherOrgRole: an org admin accepting a team
// invitation stays an org admin — the accept writes at least the role it
// names, never lower.
func TestAcceptedInvitation_KeepsAHigherOrgRole(t *testing.T) {
	ctx := context.Background()
	f := newOrgTeamFixture(t)
	u, _, err := f.svc.CreateUserAndPersonalTeam(ctx, "orgadmin@example.org", "OrgAdmin", "correcthorse", false, identity.UserStatusActive)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := f.svc.store.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: u.ID, OrgID: "org-x", Role: identity.OrgRoleAdmin, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("seed org admin: %v", err)
	}
	tok, _, err := f.svc.CreateInvitation(mintVerdictCtx(ctx), InvitationRequest{TeamID: "team-x", Email: "orgadmin@example.org", Role: identity.RoleMember, InvitedBy: f.inviter.ID})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	if _, err := f.svc.AcceptInvitationForExistingUser(ctx, u.ID, tok); err != nil {
		t.Fatalf("AcceptInvitationForExistingUser: %v", err)
	}
	if got := f.orgRoleOf(t, u.ID); got != identity.OrgRoleAdmin {
		t.Fatalf("an org admin accepting a team invitation was left org %q, want admin", got)
	}
}

// TestSSOGrant_ATeamAdminGrantMirrorsAnOrgMember: an SSO provider granting
// team admin (an org's OIDC default_role) writes an org member.
func TestSSOGrant_ATeamAdminGrantMirrorsAnOrgMember(t *testing.T) {
	ctx := context.Background()
	f := newOrgTeamFixture(t)
	u, _, err := f.svc.CreateUserAndPersonalTeam(ctx, "sso@example.org", "SSO", "correcthorse", false, identity.UserStatusActive)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := f.svc.grantMembership(ctx, u.ID, "team-x", identity.RoleAdmin, identity.MembershipSourceOIDCSSO, time.Now()); err != nil {
		t.Fatalf("grantMembership: %v", err)
	}
	if got := f.teamRoleOf(t, u.ID); got != identity.RoleAdmin {
		t.Fatalf("team grant = %q, want admin", got)
	}
	if got := f.orgRoleOf(t, u.ID); got != identity.OrgRoleMember {
		t.Fatalf("an SSO team-admin grant wrote org %q, want member", got)
	}
}

// TestCreateInvitation_RefusesAnOrgRoleItCannotCarry: an org role that is
// none of the three, or one named on a capability off the ladder.
func TestCreateInvitation_RefusesAnOrgRoleItCannotCarry(t *testing.T) {
	ctx := mintVerdictCtx(context.Background())
	f := newOrgTeamFixture(t)
	for _, req := range []InvitationRequest{
		{TeamID: "team-x", Email: "x@example.org", Role: identity.RoleMember, OrgRole: "superadmin", InvitedBy: f.inviter.ID},
		{TeamID: "team-x", Email: "x@example.org", Role: identity.RoleConfigEditor, OrgRole: identity.OrgRoleMember, InvitedBy: f.inviter.ID},
	} {
		if _, _, err := f.svc.CreateInvitation(ctx, req); !errors.Is(err, identity.ErrInvalidRole) {
			t.Errorf("CreateInvitation(role %s, org role %q) = %v, want ErrInvalidRole", req.Role, req.OrgRole, err)
		}
	}
	// A config_editor invitation naming nothing mints, with no org role.
	_, inv, err := f.svc.CreateInvitation(ctx, InvitationRequest{TeamID: "team-x", Email: "ce@example.org", Role: identity.RoleConfigEditor, InvitedBy: f.inviter.ID})
	if err != nil {
		t.Fatalf("a config_editor invitation: %v", err)
	}
	if inv.OrgRole != "" {
		t.Fatalf("a config_editor invitation names org role %q, want none", inv.OrgRole)
	}
}

// emailedFixture is newOrgTeamFixture with a mailer wired, and the context a
// personal access token's request carries once the gate granted the
// emailed-invitation mint.
func newEmailedFixture(t *testing.T, patExpiry time.Time) (orgTeamFixture, *captureMailer, context.Context) {
	t.Helper()
	f := newOrgTeamFixture(t)
	m := &captureMailer{}
	f.svc.mailer = m
	f.svc.publicURL = "https://iterion.example"
	// The minting PAT stands, unless a test says otherwise: accepting an
	// emailed invitation reads its standing (mintingCredentialStands).
	f.svc.SetPATState(func(_ context.Context, id string, _ time.Time) (bool, error) { return id == "pat-1", nil })
	ctx := WithIdentity(context.Background(), Identity{UserID: f.inviter.ID, Email: f.inviter.Email, Credential: CredentialPAT, CredentialID: "pat-1", ExpiresAt: patExpiry})
	return f, m, ContextWithMintVerdict(ctx, NewMintVerdict(MintEmailedInvitation, f.inviter.ID))
}

// TestCreateEmailedInvitation_TheTokenReachesTheMailboxAlone: the minter gets
// the row, the token leaves in the email only, and that token is the one the
// row's hash names — the invitee joins with it.
func TestCreateEmailedInvitation_TheTokenReachesTheMailboxAlone(t *testing.T) {
	f, m, ctx := newEmailedFixture(t, time.Time{})
	sealed, err := f.svc.CreateEmailedInvitation(ctx, InvitationRequest{TeamID: "team-x", Email: "Lucas@Example.org", Role: identity.RoleAdmin, InvitedBy: f.inviter.ID})
	if err != nil {
		t.Fatalf("CreateEmailedInvitation: %v", err)
	}
	if _, sent := m.last(); sent {
		t.Fatal("the email left before DeliverInvitation: the caller's caps were not confirmed yet")
	}
	if err := f.svc.DeliverInvitation(context.Background(), sealed, InvitationMail{TeamName: "Team X", OrgName: "Org X", InvitedBy: "inviter@example.org"}); err != nil {
		t.Fatalf("DeliverInvitation: %v", err)
	}
	msg, ok := m.last()
	if !ok {
		t.Fatal("DeliverInvitation sent nothing")
	}
	if msg.To != "lucas@example.org" {
		t.Fatalf("the email went to %q, want the invited address", msg.To)
	}
	i := strings.Index(msg.TextBody, "https://iterion.example/invitations/accept?token=")
	if i < 0 {
		t.Fatalf("the email carries no accept link:\n%s", msg.TextBody)
	}
	link := strings.Fields(msg.TextBody[i:])[0]
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse the accept link %q: %v", link, err)
	}
	tok := u.Query().Get("token")
	if hashInvitationToken(tok) != sealed.Invitation.TokenHash {
		t.Fatal("the emailed token is not the one the invitation row hashes")
	}
	res, err := f.svc.Register(context.Background(), "lucas@example.org", "correcthorse", "Lucas", tok, "ua", "ip")
	if err != nil {
		t.Fatalf("Register with the emailed token: %v", err)
	}
	if got := f.teamRoleOf(t, res.User.ID); got != identity.RoleAdmin {
		t.Fatalf("team grant = %q, want admin", got)
	}
	if got := f.orgRoleOf(t, res.User.ID); got != identity.OrgRoleMember {
		t.Fatalf("org membership = %q, want member", got)
	}
}

// TestCreateEmailedInvitation_ExpiresNoLaterThanTheCredential: an invitation
// a personal access token minted does not outlive it; one with no expiry
// leaves the invitation's own TTL.
func TestCreateEmailedInvitation_ExpiresNoLaterThanTheCredential(t *testing.T) {
	patExpiry := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	f, _, ctx := newEmailedFixture(t, patExpiry)
	sealed, err := f.svc.CreateEmailedInvitation(ctx, InvitationRequest{TeamID: "team-x", Email: "a@example.org", Role: identity.RoleMember, InvitedBy: f.inviter.ID})
	if err != nil {
		t.Fatalf("CreateEmailedInvitation: %v", err)
	}
	if !sealed.Invitation.ExpiresAt.Equal(patExpiry) {
		t.Fatalf("expires at %v, want the PAT's expiry %v", sealed.Invitation.ExpiresAt, patExpiry)
	}
	f2, _, ctx2 := newEmailedFixture(t, time.Time{})
	sealed, err = f2.svc.CreateEmailedInvitation(ctx2, InvitationRequest{TeamID: "team-x", Email: "b@example.org", Role: identity.RoleMember, InvitedBy: f2.inviter.ID})
	if err != nil {
		t.Fatalf("CreateEmailedInvitation: %v", err)
	}
	if left := time.Until(sealed.Invitation.ExpiresAt); left < InvitationTTL-time.Minute {
		t.Fatalf("a PAT with no expiry left the invitation %v, want its TTL %v", left, InvitationTTL)
	}
}

// TestEmailedAndLinkedMints_EachDemandsItsOwnVerdict: the emailed primitive
// refuses the team-token verdict, and the linked one refuses the emailed
// verdict — a personal access token's request never reaches the primitive
// that hands the token back.
func TestEmailedAndLinkedMints_EachDemandsItsOwnVerdict(t *testing.T) {
	f, _, emailedCtx := newEmailedFixture(t, time.Time{})
	req := InvitationRequest{TeamID: "team-x", Email: "a@example.org", Role: identity.RoleMember, InvitedBy: f.inviter.ID}
	var missing *MintVerdictMissingError
	if _, _, err := f.svc.CreateInvitation(emailedCtx, req); !errors.As(err, &missing) {
		t.Fatalf("CreateInvitation under the emailed verdict: %v, want a *MintVerdictMissingError", err)
	}
	if _, err := f.svc.CreateEmailedInvitation(mintVerdictCtx(context.Background()), req); !errors.As(err, &missing) {
		t.Fatalf("CreateEmailedInvitation under the team-token verdict: %v, want a *MintVerdictMissingError", err)
	}
	if _, err := f.svc.CreateEmailedInvitation(context.Background(), req); !errors.As(err, &missing) {
		t.Fatalf("CreateEmailedInvitation under a bare context: %v, want a *MintVerdictMissingError", err)
	}
}

// TestCreateEmailedInvitation_RefusesWithNoMailer: an instance that sends no
// email mints nothing a personal access token asked for, and writes no row.
func TestCreateEmailedInvitation_RefusesWithNoMailer(t *testing.T) {
	f, _, ctx := newEmailedFixture(t, time.Time{})
	f.svc.mailer = nil
	if _, err := f.svc.CreateEmailedInvitation(ctx, InvitationRequest{TeamID: "team-x", Email: "a@example.org", Role: identity.RoleMember, InvitedBy: f.inviter.ID}); !errors.Is(err, ErrInvitationNeedsEmail) {
		t.Fatalf("CreateEmailedInvitation with no mailer: %v, want ErrInvitationNeedsEmail", err)
	}
	rows, err := f.svc.store.ListInvitationsByTeam(context.Background(), "team-x")
	if err != nil {
		t.Fatalf("ListInvitationsByTeam: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused emailed mint left %d invitation rows", len(rows))
	}
}

// TestDeliverInvitation_RefusesAnUnsealedInvitation: a SealedInvitation built
// outside this package carries no token, and delivering it is an error rather
// than an email with an empty link.
func TestDeliverInvitation_RefusesAnUnsealedInvitation(t *testing.T) {
	f, m, _ := newEmailedFixture(t, time.Time{})
	if err := f.svc.DeliverInvitation(context.Background(), SealedInvitation{Invitation: identity.Invitation{ID: "forged", Email: "a@example.org"}}, InvitationMail{}); err == nil {
		t.Fatal("DeliverInvitation of an unsealed invitation succeeded")
	}
	if _, sent := m.last(); sent {
		t.Fatal("an unsealed invitation was emailed")
	}
}

// patMintedInvitation mints an emailed invitation from personal access token
// "pat-1" of the fixture's inviter, with the PAT reader answering usable, and
// answers the token the email carried.
func patMintedInvitation(t *testing.T, f orgTeamFixture, m *captureMailer, ctx context.Context, email string) string {
	t.Helper()
	sealed, err := f.svc.CreateEmailedInvitation(ctx, InvitationRequest{TeamID: "team-x", Email: email, Role: identity.RoleAdmin, OrgRole: identity.OrgRoleAdmin, InvitedBy: f.inviter.ID})
	if err != nil {
		t.Fatalf("CreateEmailedInvitation: %v", err)
	}
	if sealed.Invitation.MintedWith != "pat-1" {
		t.Fatalf("the row names minting credential %q, want pat-1", sealed.Invitation.MintedWith)
	}
	if err := f.svc.DeliverInvitation(context.Background(), sealed, InvitationMail{TeamName: "Team X"}); err != nil {
		t.Fatalf("DeliverInvitation: %v", err)
	}
	msg, _ := m.last()
	i := strings.Index(msg.TextBody, "token=")
	return strings.Fields(msg.TextBody[i+len("token="):])[0]
}

// TestEmailedInvitation_EndsWithTheTokenThatSentIt: an invitation a personal
// access token minted is accepted while that token stands, and refused once
// it does not — revoked, or its account disabled — on both accept paths.
func TestEmailedInvitation_EndsWithTheTokenThatSentIt(t *testing.T) {
	usable := true
	setup := func(t *testing.T) (orgTeamFixture, *captureMailer, context.Context) {
		f, m, ctx := newEmailedFixture(t, time.Time{})
		usable = true
		f.svc.SetPATState(func(_ context.Context, id string, _ time.Time) (bool, error) { return id == "pat-1" && usable, nil })
		return f, m, ctx
	}

	t.Run("while the token stands, the invitation is accepted", func(t *testing.T) {
		f, m, ctx := setup(t)
		tok := patMintedInvitation(t, f, m, ctx, "a@example.org")
		if _, err := f.svc.Register(context.Background(), "a@example.org", "correcthorse", "A", tok, "ua", "ip"); err != nil {
			t.Fatalf("Register under a standing token: %v", err)
		}
	})
	t.Run("a revoked token ends its invitation, registration path", func(t *testing.T) {
		f, m, ctx := setup(t)
		tok := patMintedInvitation(t, f, m, ctx, "b@example.org")
		usable = false
		if _, err := f.svc.Register(context.Background(), "b@example.org", "correcthorse", "B", tok, "ua", "ip"); !errors.Is(err, ErrInvitationCredentialGone) {
			t.Fatalf("Register after the token was revoked: %v, want ErrInvitationCredentialGone", err)
		}
		if _, err := f.svc.store.GetUserByEmail(context.Background(), "b@example.org"); err == nil {
			t.Fatal("a refused registration left an account behind")
		}
	})
	t.Run("a revoked token ends its invitation, existing-account path", func(t *testing.T) {
		f, m, ctx := setup(t)
		tok := patMintedInvitation(t, f, m, ctx, "c@example.org")
		u, _, err := f.svc.CreateUserAndPersonalTeam(context.Background(), "c@example.org", "C", "correcthorse", false, identity.UserStatusActive)
		if err != nil {
			t.Fatal(err)
		}
		usable = false
		if _, err := f.svc.AcceptInvitationForExistingUser(context.Background(), u.ID, tok); !errors.Is(err, ErrInvitationCredentialGone) {
			t.Fatalf("accept after the token was revoked: %v, want ErrInvitationCredentialGone", err)
		}
		if _, err := f.svc.store.GetOrgMembership(context.Background(), u.ID, "org-x"); err == nil {
			t.Fatal("a refused accept wrote an org membership")
		}
	})
	t.Run("a disabled minter ends their invitation", func(t *testing.T) {
		f, m, ctx := setup(t)
		tok := patMintedInvitation(t, f, m, ctx, "d@example.org")
		disabled := identity.UserStatusDisabled
		if _, err := f.svc.store.PatchUser(context.Background(), f.inviter.ID, identity.UserPatch{Status: &disabled}); err != nil {
			t.Fatalf("disable the minter: %v", err)
		}
		_, err := f.svc.Register(context.Background(), "d@example.org", "correcthorse", "D", tok, "ua", "ip")
		if !errors.Is(err, ErrInvitationCredentialGone) {
			t.Fatalf("Register after the minter was disabled: %v, want ErrInvitationCredentialGone", err)
		}
		if strings.Contains(err.Error(), "disabled") {
			t.Fatalf("the refusal tells the invitee the state of its sender's account: %v", err)
		}
	})
	t.Run("a minter whose account is gone ends their invitation", func(t *testing.T) {
		f, m, ctx := setup(t)
		sealed, err := f.svc.CreateEmailedInvitation(ctx, InvitationRequest{TeamID: "team-x", Email: "g@example.org", Role: identity.RoleMember, InvitedBy: "ghost-account"})
		if err != nil {
			t.Fatalf("CreateEmailedInvitation: %v", err)
		}
		if err := f.svc.DeliverInvitation(context.Background(), sealed, InvitationMail{TeamName: "Team X"}); err != nil {
			t.Fatalf("DeliverInvitation: %v", err)
		}
		msg, _ := m.last()
		tok := strings.Fields(msg.TextBody[strings.Index(msg.TextBody, "token=")+len("token="):])[0]
		if _, err := f.svc.Register(context.Background(), "g@example.org", "correcthorse", "G", tok, "ua", "ip"); !errors.Is(err, ErrInvitationCredentialGone) {
			t.Fatalf("Register when the minter's account is gone: %v, want ErrInvitationCredentialGone", err)
		}
	})
	t.Run("a minter pending a password change does not stand", func(t *testing.T) {
		f, m, ctx := setup(t)
		tok := patMintedInvitation(t, f, m, ctx, "p@example.org")
		pending := identity.UserStatusPendingPasswordChange
		if _, err := f.svc.store.PatchUser(context.Background(), f.inviter.ID, identity.UserPatch{Status: &pending}); err != nil {
			t.Fatalf("set the minter pending a password change: %v", err)
		}
		_, err := f.svc.Register(context.Background(), "p@example.org", "correcthorse", "P", tok, "ua", "ip")
		if !errors.Is(err, ErrInvitationCredentialGone) {
			t.Fatalf("Register while the minter is pending a password change: %v, want ErrInvitationCredentialGone", err)
		}
		if strings.Contains(err.Error(), "password") {
			t.Fatalf("the refusal tells the invitee the state of its sender's account: %v", err)
		}
	})
	t.Run("a session's invitation names no token and needs no reader", func(t *testing.T) {
		f, _, _ := setup(t)
		f.svc.SetPATState(nil)
		tok, inv, err := f.svc.CreateInvitation(mintVerdictCtx(context.Background()), InvitationRequest{TeamID: "team-x", Email: "e@example.org", Role: identity.RoleMember, InvitedBy: f.inviter.ID})
		if err != nil {
			t.Fatal(err)
		}
		if inv.MintedWith != "" {
			t.Fatalf("a session's invitation names minting credential %q", inv.MintedWith)
		}
		if _, err := f.svc.Register(context.Background(), "e@example.org", "correcthorse", "E", tok, "ua", "ip"); err != nil {
			t.Fatalf("Register on a session's invitation: %v", err)
		}
	})
}

// TestSealedInvitation_KeepsItsTokenAndAddress: the sealed value prints no
// token, and an edit of its exported row's address redirects nothing — the
// email goes to the address sealed at the mint.
func TestSealedInvitation_KeepsItsTokenAndAddress(t *testing.T) {
	f, m, ctx := newEmailedFixture(t, time.Time{})
	sealed, err := f.svc.CreateEmailedInvitation(ctx, InvitationRequest{TeamID: "team-x", Email: "victim@example.org", Role: identity.RoleMember, InvitedBy: f.inviter.ID})
	if err != nil {
		t.Fatalf("CreateEmailedInvitation: %v", err)
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(verb, sealed), sealed.seal.token) {
			t.Fatalf("%s of a sealed invitation prints its token", verb)
		}
	}
	sealed.Invitation.Email = "attacker@evil.example"
	if err := f.svc.DeliverInvitation(context.Background(), sealed, InvitationMail{TeamName: "Team X"}); err != nil {
		t.Fatalf("DeliverInvitation: %v", err)
	}
	msg, _ := m.last()
	if msg.To != "victim@example.org" {
		t.Fatalf("the sealed invitation was delivered to %q, want the address sealed at the mint", msg.To)
	}
}
