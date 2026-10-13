package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

func newTestService(t *testing.T, mode SignupMode) *Service {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := NewService(Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: mode,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestRegisterOpenAndLogin(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)

	res, err := svc.Register(ctx, "Alice@Example.com", "correcthorse", "Alice", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.User.Email != "alice@example.com" {
		t.Fatalf("normalized email expected, got %q", res.User.Email)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Fatal("expected tokens to be set")
	}
	if res.ActiveTeamID == "" {
		t.Fatal("expected personal team ID to be set")
	}
	if res.ActiveRole != identity.RoleOwner {
		t.Fatalf("expected Owner of personal team, got %q", res.ActiveRole)
	}

	// Login with same credentials yields a new access + refresh.
	res2, err := svc.Login(ctx, "alice@example.com", "correcthorse", "ua", "ip")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res2.AccessToken == res.AccessToken {
		t.Fatal("access token should differ across logins")
	}
}

func TestLoginInvalidCredentialsAndDisabled(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	_, err := svc.Register(ctx, "bob@example.com", "correcthorse", "Bob", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := svc.Login(ctx, "bob@example.com", "wrong", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	if _, err := svc.Login(ctx, "nobody@example.com", "anything", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials for unknown user, got %v", err)
	}
	// Disable Bob and try again.
	u, _ := svc.store.GetUserByEmail(ctx, "bob@example.com")
	u.Status = identity.UserStatusDisabled
	if err := svc.store.UpdateUser(ctx, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if _, err := svc.Login(ctx, "bob@example.com", "correcthorse", "ua", "ip"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("expected ErrAccountDisabled, got %v", err)
	}
}

func TestChangePasswordPending(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)

	// Bootstrap-style account: a temp password + pending_password_change.
	if _, _, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "temp-pass-123", true, identity.UserStatusPendingPasswordChange); err != nil {
		t.Fatalf("CreateUserAndPersonalTeam: %v", err)
	}

	// Login is gated until the password is rotated.
	if _, err := svc.Login(ctx, "ops@example.com", "temp-pass-123", "ua", "ip"); !errors.Is(err, ErrPasswordChangeRequired) {
		t.Fatalf("expected ErrPasswordChangeRequired, got %v", err)
	}
	// Wrong temp password → opaque invalid-credentials.
	if _, err := svc.ChangePasswordPending(ctx, "ops@example.com", "wrong", "brand-new-pass", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong temp: expected ErrInvalidCredentials, got %v", err)
	}
	// Too-weak new password → ErrPasswordWeak.
	if _, err := svc.ChangePasswordPending(ctx, "ops@example.com", "temp-pass-123", "short", "ua", "ip"); !errors.Is(err, ErrPasswordWeak) {
		t.Fatalf("weak new: expected ErrPasswordWeak, got %v", err)
	}
	// Unknown account stays opaque (no enumeration).
	if _, err := svc.ChangePasswordPending(ctx, "nobody@example.com", "temp-pass-123", "brand-new-pass", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown: expected ErrInvalidCredentials, got %v", err)
	}

	// Nothing has stamped a sign-in yet — the account was created pending and
	// every attempt above was refused — so the stamp asserted after the
	// rotation can only be the rotation's own.
	if before, _ := svc.store.GetUserByEmail(ctx, "ops@example.com"); before.LastLoginAt != nil {
		t.Fatalf("last_login_at = %v before the rotation: the stamp asserted below would prove nothing", before.LastLoginAt)
	}

	// Correct rotation activates the account and issues a session.
	res, err := svc.ChangePasswordPending(ctx, "ops@example.com", "temp-pass-123", "brand-new-pass", "ua", "ip")
	if err != nil {
		t.Fatalf("ChangePasswordPending: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Fatal("expected a session after rotation")
	}
	u, _ := svc.store.GetUserByEmail(ctx, "ops@example.com")
	if u.Status != identity.UserStatusActive {
		t.Fatalf("expected active status after rotation, got %q", u.Status)
	}
	// The rotation grants a session, so it owns the stamp its own patch
	// carries: the account must not read as never-signed-in afterwards.
	if u.LastLoginAt == nil {
		t.Error("last_login_at is unset after the rotation that signed the account in")
	}

	// The new password logs in; the temp no longer works.
	if _, err := svc.Login(ctx, "ops@example.com", "brand-new-pass", "ua", "ip"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	if _, err := svc.Login(ctx, "ops@example.com", "temp-pass-123", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old temp should fail, got %v", err)
	}
	// Single-use: an already-active account cannot re-run the rotation flow.
	if _, err := svc.ChangePasswordPending(ctx, "ops@example.com", "brand-new-pass", "another-pass", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("active account: expected ErrInvalidCredentials, got %v", err)
	}
}

func TestRegisterInviteOnlyRequiresToken(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupInviteOnly)
	if _, err := svc.Register(ctx, "carol@example.com", "correcthorse", "", "", "ua", "ip"); !errors.Is(err, ErrSignupClosed) {
		t.Fatalf("expected ErrSignupClosed, got %v", err)
	}
}

func TestInvitationFlow(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupInviteOnly)

	// Bootstrap an admin who creates a team.
	adminUser, _, err := svc.CreateUserAndPersonalTeam(ctx, "admin@example.com", "Admin", "correcthorse", true, identity.UserStatusActive)
	if err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	team, err := svc.store.CreateTeam(ctx, identity.Team{
		ID:   "team-acme",
		Name: "Acme",
		Slug: "acme",
	})
	if err != nil {
		t.Fatalf("create team: %v", err)
	}

	// Admin invites Carol as member.
	tok, inv, err := svc.CreateInvitation(ctx, team.ID, "Carol@example.com", identity.RoleMember, adminUser.ID)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	if inv.TokenHash == "" || tok == "" {
		t.Fatal("expected token + hash to be set")
	}

	// Carol registers using the token.
	res, err := svc.Register(ctx, "carol@example.com", "correcthorse", "Carol", tok, "ua", "ip")
	if err != nil {
		t.Fatalf("Register with invitation: %v", err)
	}
	if res.ActiveTeamID != team.ID || res.ActiveRole != identity.RoleMember {
		t.Fatalf("active team/role wrong: got %s/%s", res.ActiveTeamID, res.ActiveRole)
	}

	// Reusing the same token must fail.
	if _, err := svc.Register(ctx, "carol2@example.com", "correcthorse", "", tok, "ua", "ip"); !errors.Is(err, identity.ErrInvitationUsed) {
		t.Fatalf("expected ErrInvitationUsed on reuse, got %v", err)
	}
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	res, err := svc.Register(ctx, "dan@example.com", "correcthorse", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	r2, err := svc.Refresh(ctx, res.RefreshToken, "ua", "ip")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if r2.RefreshToken == res.RefreshToken {
		t.Fatal("refresh token should rotate")
	}
	// Reusing the now-revoked first token must wipe all sessions.
	_, err = svc.Refresh(ctx, res.RefreshToken, "ua", "ip")
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("expected ErrSessionRevoked, got %v", err)
	}
	// And r2's token must now also be revoked.
	if _, err := svc.Refresh(ctx, r2.RefreshToken, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("expected reuse to revoke all sessions; r2 still valid: %v", err)
	}
}

func TestSwitchTeamRequiresMembership(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	res, err := svc.Register(ctx, "eve@example.com", "correcthorse", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	other, _ := svc.store.CreateTeam(ctx, identity.Team{ID: "other", Name: "Other", Slug: "other"})
	if _, _, _, err := svc.SwitchTeam(ctx, res.User.ID, other.ID); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("expected ErrNotAMember, got %v", err)
	}
}

func TestJWTRoundTripCarriesIdentity(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, SignupOpen)
	res, err := svc.Register(ctx, "fay@example.com", "correcthorse", "Fay", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	parsed, err := svc.signer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if parsed.UserID != res.User.ID {
		t.Fatalf("UserID mismatch: %s vs %s", parsed.UserID, res.User.ID)
	}
	if parsed.TeamID != res.ActiveTeamID || parsed.Role != res.ActiveRole {
		t.Fatalf("team/role mismatch: %s/%s vs %s/%s", parsed.TeamID, parsed.Role, res.ActiveTeamID, res.ActiveRole)
	}
	_ = ctx
}

func TestVerifyExpiredToken(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	signer, _ := NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 1*time.Millisecond)
	tok, _, err := signer.IssueAccess(Identity{UserID: "x"})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := signer.Verify(tok); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correcthorse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("correcthorse", hash)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword: %v, ok=%v", err, ok)
	}
	ok, _ = VerifyPassword("wrong", hash)
	if ok {
		t.Fatal("VerifyPassword accepted wrong password")
	}
	if _, err := VerifyPassword("anything", "$invalid$hash$format$"); !errors.Is(err, ErrInvalidPasswordHash) {
		t.Fatalf("expected ErrInvalidPasswordHash, got %v", err)
	}
}

// TestEveryUserWriteMovesUpdatedAt pins what users.updated_at means now that
// the STORE stamps it, because the admin console ships the field: it is the
// ROW's last change, not the last administrative edit. A successful sign-in
// moves it (the login stamp) and so does a failed one (the brute-force
// counter), which is why dormancy is LastLoginAt's question — pinned here
// too, on the field the sign-in itself owns: it lands on a success and stays
// put on a failure.
func TestEveryUserWriteMovesUpdatedAt(t *testing.T) {
	svc := newTestService(t, SignupOpen)
	ctx := context.Background()
	res, err := svc.Register(ctx, "stamped@example.com", "correcthorse", "Stamped", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	read := func() identity.User {
		t.Helper()
		u, err := svc.Store().GetUser(ctx, res.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	before := read()
	if before.LastLoginAt != nil {
		t.Fatalf("last_login_at = %v on a registration: the stamp assertions below would prove nothing", before.LastLoginAt)
	}
	if _, err := svc.Login(ctx, "stamped@example.com", "correcthorse", "ua", "ip"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	afterLogin := read()
	if !afterLogin.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at after a sign-in = %v, want later than %v", afterLogin.UpdatedAt, before.UpdatedAt)
	}
	// The stamp itself: every login path delivers it through the single
	// stampLogin patch, so this is where the console's dormancy fact is won
	// or silently lost for every account on the platform. It must carry the
	// sign-in's own time, not a zero value that would read as "1 Jan 0001".
	if afterLogin.LastLoginAt == nil || !afterLogin.LastLoginAt.After(before.UpdatedAt) {
		t.Fatalf("last_login_at after a sign-in = %v, want a stamp later than the row's previous change %v",
			afterLogin.LastLoginAt, before.UpdatedAt)
	}

	if _, err := svc.Login(ctx, "stamped@example.com", "WRONG-password", "ua", "ip"); err == nil {
		t.Fatal("a wrong password must not sign in")
	}
	afterFailed := read()
	if !afterFailed.UpdatedAt.After(afterLogin.UpdatedAt) {
		t.Errorf("updated_at after a FAILED sign-in = %v, want later than %v", afterFailed.UpdatedAt, afterLogin.UpdatedAt)
	}
	// And the contrast that gives the two fields their separate meanings: a
	// failed sign-in moves updated_at and leaves last_login_at alone.
	if afterFailed.LastLoginAt == nil || !afterFailed.LastLoginAt.Equal(*afterLogin.LastLoginAt) {
		t.Errorf("last_login_at after a FAILED sign-in = %v, want %v unchanged", afterFailed.LastLoginAt, afterLogin.LastLoginAt)
	}
}

// orgMirrorFailStore fails the org-membership half of an accept, which is
// the half the accept writes SECOND.
type orgMirrorFailStore struct {
	identity.Store
}

func (s *orgMirrorFailStore) UpsertOrgMembership(ctx context.Context, om identity.OrgMembership) error {
	return errors.New("identity: upsert org membership: connection reset by peer")
}

// teamGrantFailStore fails the team-grant half instead.
type teamGrantFailStore struct {
	identity.Store
	userID, teamID string
}

func (s *teamGrantFailStore) UpsertMembership(ctx context.Context, mb identity.Membership) error {
	if mb.UserID == s.userID && mb.TeamID == s.teamID {
		return errors.New("identity: upsert membership: connection reset by peer")
	}
	return s.Store.UpsertMembership(ctx, mb)
}

// An accept writes two rows with no transaction behind them, so their ORDER
// decides what a failure between them leaves standing. A team grant written
// first and left behind is live access to a team of an org the holder is not
// a member of — the state the server compensates for in
// handlePutTeamMember/handleRemoveOrgMember, and the one
// GET /api/admin/users/{id} reports as orphan_grant. An org membership left
// behind instead is a member of an org with no team: org-scoped READ (the
// roster, the settings, the usage and the team and SSO-provider lists) and
// no team access — the lesser of the two states, witnessed in pkg/server by
// TestOrgMembership_withNoTeamGrantConfersOrgScopedReadOnly. The SSO path
// (grantMembership) already writes the mirror first.
func TestAcceptInvitation_anOrgMirrorThatFailsLeavesNoTeamGrant(t *testing.T) {
	ctx := context.Background()

	seed := func(t *testing.T, inviteeEmail string) (*Service, identity.Store, string) {
		t.Helper()
		svc := newTestService(t, SignupInviteOnly)
		real := svc.store
		admin, _, err := svc.CreateUserAndPersonalTeam(ctx, "admin@example.com", "Admin", "correcthorse", true, identity.UserStatusActive)
		if err != nil {
			t.Fatalf("bootstrap admin: %v", err)
		}
		org, err := real.CreateOrg(ctx, identity.Org{ID: "o1", Name: "o1", Slug: "o1", CreatedAt: time.Now()})
		if err != nil {
			t.Fatalf("create org: %v", err)
		}
		team, err := real.CreateTeam(ctx, identity.Team{ID: "t1", Name: "t1", Slug: "t1", OrgID: org.ID, CreatedAt: time.Now()})
		if err != nil {
			t.Fatalf("create team: %v", err)
		}
		tok, _, err := svc.CreateInvitation(ctx, team.ID, inviteeEmail, identity.RoleMember, admin.ID)
		if err != nil {
			t.Fatalf("CreateInvitation: %v", err)
		}
		return svc, real, tok
	}

	t.Run("an account that already exists", func(t *testing.T) {
		svc, real, tok := seed(t, "invitee@example.com")
		invitee, _, err := svc.CreateUserAndPersonalTeam(ctx, "invitee@example.com", "Invitee", "correcthorse", false, identity.UserStatusActive)
		if err != nil {
			t.Fatalf("create invitee: %v", err)
		}
		svc.SetStoreForTest(&orgMirrorFailStore{Store: real})

		if _, err := svc.AcceptInvitationForExistingUser(ctx, invitee.ID, tok); err == nil {
			t.Fatal("the accept reported success although the org mirror failed")
		}
		if om, err := real.GetOrgMembership(ctx, invitee.ID, "o1"); err == nil {
			t.Fatalf("org membership %+v landed — then this is not the scenario the test describes", om)
		}
		if mb, err := real.GetMembership(ctx, invitee.ID, "t1"); err == nil {
			t.Errorf("team grant %+v stands with no org membership behind it — the accept wrote the grant before its boundary: %+v", mb, mb.Role)
		}
		// And what the claim-first order costs, on the path that pays it:
		// the token is spent, so this accept cannot be retried. That is the
		// fail-closed half of making the invitation row the one write an
		// off-boarding can refuse — the invitee asks for another invitation.
		if _, err := svc.AcceptInvitationForExistingUser(ctx, invitee.ID, tok); !errors.Is(err, identity.ErrInvitationUsed) {
			t.Errorf("a retry of the accept whose mirror failed: %v, want ErrInvitationUsed — the claim is its first write", err)
		}
	})

	// The other order's own failure, which is what makes this one the right
	// way round: the grant write fails and the boundary it sits inside is
	// already there, so the state left behind is an org member with no team —
	// nothing the org roster does not already show.
	t.Run("the grant write is the half that fails", func(t *testing.T) {
		svc, real, tok := seed(t, "invitee@example.com")
		invitee, _, err := svc.CreateUserAndPersonalTeam(ctx, "invitee@example.com", "Invitee", "correcthorse", false, identity.UserStatusActive)
		if err != nil {
			t.Fatalf("create invitee: %v", err)
		}
		svc.SetStoreForTest(&teamGrantFailStore{Store: real, userID: invitee.ID, teamID: "t1"})

		if _, err := svc.AcceptInvitationForExistingUser(ctx, invitee.ID, tok); err == nil {
			t.Fatal("the accept reported success although the team grant failed")
		}
		if mb, err := real.GetMembership(ctx, invitee.ID, "t1"); err == nil {
			t.Fatalf("team grant %+v landed — then this is not the scenario the test describes", mb)
		}
		if _, err := real.GetOrgMembership(ctx, invitee.ID, "o1"); err != nil {
			t.Errorf("the boundary was not written before the grant, so the failure left neither: %v", err)
		}
	})

	t.Run("an account the invitation creates", func(t *testing.T) {
		svc, real, tok := seed(t, "newbie@example.com")
		svc.SetStoreForTest(&orgMirrorFailStore{Store: real})

		if _, err := svc.Register(ctx, "newbie@example.com", "correcthorse", "Newbie", tok, "ua", "ip"); err == nil {
			t.Fatal("the registration reported success although the org mirror failed")
		}
		u, err := real.GetUserByEmail(ctx, "newbie@example.com")
		if err != nil {
			// The account itself is created before either row; a store that
			// did not create it leaves no grant to assert on.
			t.Skipf("the account was not created, so this path writes no grant: %v", err)
		}
		if om, err := real.GetOrgMembership(ctx, u.ID, "o1"); err == nil {
			t.Fatalf("org membership %+v landed — then this is not the scenario the test describes", om)
		}
		if mb, err := real.GetMembership(ctx, u.ID, "t1"); err == nil {
			t.Errorf("team grant %+v stands with no org membership behind it — the accept wrote the grant before its boundary", mb)
		}
	})
}

// callOrderStore records, in order, the writes an accept makes that decide
// what a failure or a concurrent off-boarding between them leaves standing:
// the ACCOUNT row a registration creates (not access, and nothing an
// off-boarding can refuse), the CLAIM of the invitation, the org membership
// the grant is mirrored up to, and the grant itself.
type callOrderStore struct {
	identity.Store
	calls []string
}

func (s *callOrderStore) CreateUser(ctx context.Context, u identity.User) (identity.User, error) {
	s.calls = append(s.calls, "account")
	return s.Store.CreateUser(ctx, u)
}

func (s *callOrderStore) UpdateInvitation(ctx context.Context, inv identity.Invitation) error {
	s.calls = append(s.calls, "claim")
	return s.Store.UpdateInvitation(ctx, inv)
}

func (s *callOrderStore) UpsertOrgMembership(ctx context.Context, m identity.OrgMembership) error {
	s.calls = append(s.calls, "boundary")
	return s.Store.UpsertOrgMembership(ctx, m)
}

func (s *callOrderStore) UpsertMembership(ctx context.Context, m identity.Membership) error {
	s.calls = append(s.calls, "grant")
	return s.Store.UpsertMembership(ctx, m)
}

// Both accept paths CLAIM the invitation before they write any access, and
// the claim is a conditional write in both stores: a token whose invitation
// an org off-boarding already revoked writes no access at all, where
// claiming last let an accept that had merely READ the row walk the removed
// account back into the org and the team (pkg/server
// TestOrgOffboarding_anAcceptHoldingTheRevokedRowWritesNothing). The order
// of the two access rows stays boundary-before-grant.
//
// On the REGISTRATION path the account row comes before the claim — it is
// not access, and moving the claim ahead of it would burn the token on a
// CreateUser failure with no account to show for it — so the order there is
// account, claim, boundary, grant, and a registration that loses the claim
// race leaves that account row behind (the subtest below).
func TestAcceptInvitation_claimsTheInvitationBeforeAnyAccess(t *testing.T) {
	ctx := context.Background()

	seed := func(t *testing.T, inviteeEmail string) (*Service, *callOrderStore, string) {
		t.Helper()
		svc := newTestService(t, SignupInviteOnly)
		real := svc.store
		admin, _, err := svc.CreateUserAndPersonalTeam(ctx, "admin@example.com", "Admin", "correcthorse", true, identity.UserStatusActive)
		if err != nil {
			t.Fatalf("bootstrap admin: %v", err)
		}
		org, err := real.CreateOrg(ctx, identity.Org{ID: "o1", Name: "o1", Slug: "o1", CreatedAt: time.Now()})
		if err != nil {
			t.Fatalf("create org: %v", err)
		}
		team, err := real.CreateTeam(ctx, identity.Team{ID: "t1", Name: "t1", Slug: "t1", OrgID: org.ID, CreatedAt: time.Now()})
		if err != nil {
			t.Fatalf("create team: %v", err)
		}
		tok, _, err := svc.CreateInvitation(ctx, team.ID, inviteeEmail, identity.RoleMember, admin.ID)
		if err != nil {
			t.Fatalf("CreateInvitation: %v", err)
		}
		return svc, &callOrderStore{Store: real}, tok
	}

	t.Run("an account that already exists", func(t *testing.T) {
		svc, rec, tok := seed(t, "invitee@example.com")
		invitee, _, err := svc.CreateUserAndPersonalTeam(ctx, "invitee@example.com", "Invitee", "correcthorse", false, identity.UserStatusActive)
		if err != nil {
			t.Fatalf("create invitee: %v", err)
		}
		svc.SetStoreForTest(rec)
		if _, err := svc.AcceptInvitationForExistingUser(ctx, invitee.ID, tok); err != nil {
			t.Fatalf("accept: %v", err)
		}
		const want = "claim,boundary,grant"
		if got := strings.Join(rec.calls, ","); got != want {
			t.Errorf("the accept wrote %s, want %s — the claim is the write a revoked invitation refuses", got, want)
		}
	})

	t.Run("an account the invitation creates", func(t *testing.T) {
		svc, rec, tok := seed(t, "newbie@example.com")
		svc.SetStoreForTest(rec)
		if _, err := svc.Register(ctx, "newbie@example.com", "correcthorse", "Newbie", tok, "ua", "ip"); err != nil {
			t.Fatalf("register: %v", err)
		}
		// The account row is written FIRST here, and it is the one write of
		// this path the claim does not guard.
		const want = "account,claim,boundary,grant"
		if got := strings.Join(rec.calls, ","); got != want {
			t.Errorf("the registration wrote %s, want %s — the claim is the first ACCESS write, not the first write", got, want)
		}
	})

	t.Run("a registration that loses the claim race keeps its account row", func(t *testing.T) {
		svc, _, tok := seed(t, "newbie@example.com")
		real := svc.store
		svc.SetStoreForTest(&revokedClaimStore{Store: real})
		if _, err := svc.Register(ctx, "newbie@example.com", "correcthorse", "Newbie", tok, "ua", "ip"); !errors.Is(err, ErrInvitationNotFound) {
			t.Fatalf("a registration whose invitation was revoked under it: %v, want ErrInvitationNotFound", err)
		}
		u, err := real.GetUserByEmail(ctx, "newbie@example.com")
		if err != nil {
			t.Fatalf("the account row the claim could not guard is gone: %v", err)
		}
		if _, err := real.GetMembership(ctx, u.ID, "t1"); err == nil {
			t.Error("the refused claim still let a team grant land")
		}
		if _, err := real.GetOrgMembership(ctx, u.ID, "o1"); err == nil {
			t.Error("the refused claim still let an org membership land")
		}
		// What that row costs the invitee, and why the recovery is a login
		// rather than a second registration: the address is taken.
		if _, err := svc.Register(ctx, "newbie@example.com", "correcthorse", "Newbie", tok, "ua", "ip"); !errors.Is(err, identity.ErrEmailAlreadyTaken) {
			t.Errorf("a second registration on the same address: %v, want ErrEmailAlreadyTaken", err)
		}
		// And the recovery itself, executed rather than asserted: the
		// password the invitee set is on that row, so a login works, and a
		// fresh invitation to the same address lands both access rows
		// through the existing-user path.
		svc.SetStoreForTest(real)
		if _, err := svc.Login(ctx, "newbie@example.com", "correcthorse", "ua", "ip"); err != nil {
			t.Fatalf("logging in with the password the lost registration set: %v", err)
		}
		next, _, err := svc.CreateInvitation(ctx, "t1", "newbie@example.com", identity.RoleMember, "admin")
		if err != nil {
			t.Fatalf("a fresh invitation to the same address: %v", err)
		}
		if _, err := svc.AcceptInvitationForExistingUser(ctx, u.ID, next); err != nil {
			t.Fatalf("accepting it as the existing user: %v", err)
		}
		if _, err := real.GetMembership(ctx, u.ID, "t1"); err != nil {
			t.Errorf("the recovery left no team grant: %v", err)
		}
		if _, err := real.GetOrgMembership(ctx, u.ID, "o1"); err != nil {
			t.Errorf("the recovery left no org membership: %v", err)
		}
	})
}

// revokedClaimStore is the concurrent off-boarding seen from the accept's
// side: the invitation row is gone by the time the claim reaches it, which
// both stores answer with ErrNotFound.
type revokedClaimStore struct {
	identity.Store
}

func (s *revokedClaimStore) UpdateInvitation(ctx context.Context, inv identity.Invitation) error {
	return identity.ErrNotFound
}
