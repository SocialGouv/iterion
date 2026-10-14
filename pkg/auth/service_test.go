package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
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
