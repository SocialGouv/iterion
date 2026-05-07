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
