package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// The flows that end sessions — a password change or reset, the forced
// rotation, a sign-out — revoke before they commit and answer a revocation
// that failed as the failure it is; the flows that mint a session after their
// own revocation give it the generation that revocation raised; and a switch
// neither extends the access token it re-scopes nor answers for a session a
// revocation ended.

// seedReset stores a reset link for userID on svc and returns its token.
func seedReset(t *testing.T, svc *Service, userID string) string {
	t.Helper()
	if svc.resets == nil {
		svc.resets = NewMemoryPasswordResetStore()
	}
	token := ResetTokenPrefix + "a-reset-token-for-" + userID
	now := time.Now().UTC()
	if err := svc.resets.Create(context.Background(), PasswordReset{
		ID: "reset-" + userID, UserID: userID, TokenHash: HashRefreshToken(token),
		CreatedAt: now, ExpiresAt: now.Add(ResetTokenTTL),
	}); err != nil {
		t.Fatalf("seed a reset link: %v", err)
	}
	return token
}

// setStatus moves the fixture's account to status through the store.
func setStatus(t *testing.T, f *rotationFixture, status identity.UserStatus) {
	t.Helper()
	if _, err := f.ids.PatchUser(context.Background(), f.userID, identity.UserPatch{Status: &status}); err != nil {
		t.Fatalf("set the account's status to %q: %v", status, err)
	}
}

// mustRefresh fails the test unless raw refreshes.
func mustRefresh(t *testing.T, f *rotationFixture, raw, what string) LoginResult {
	t.Helper()
	res, err := f.svc.Refresh(context.Background(), raw, "ua", "ip")
	if err != nil {
		t.Fatalf("%s does not refresh: %v", what, err)
	}
	return res
}

func TestChangePassword_ARevocationThatFailsChangesNothing(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.failRevokeAllArmed.Store(true)
	if _, err := f.svc.ChangePassword(ctx, f.userID, "correct-horse-9", "another-horse-7", "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a password change whose revocation fails: got %v, want the store's failure", err)
	}
	if _, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip"); err != nil {
		t.Fatalf("the password changed under an error answer: the old one no longer signs in (%v)", err)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("a change answered as a failure revoked the other device")
	}
	res, err := f.svc.ChangePassword(ctx, f.userID, "correct-horse-9", "another-horse-7", "ua", "ip")
	if err != nil {
		t.Fatalf("the change retried once the store answers: %v", err)
	}
	if !f.otherDeviceRevoked(t) {
		t.Fatal("the change left the other device's session live")
	}
	mustRefresh(t, f, res.RefreshToken, "the session the change issued")
}

func TestConfirmPasswordReset_ARevocationThatFailsKeepsTheLink(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	token := seedReset(t, f.svc, f.userID)
	f.sessions.failRevokeAllArmed.Store(true)
	if _, err := f.svc.ConfirmPasswordReset(ctx, token, "brand-new-password-1", "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a reset confirm whose revocation fails: got %v, want the store's failure", err)
	}
	if _, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip"); err != nil {
		t.Fatalf("the password changed under an error answer: %v", err)
	}
	if f.otherDeviceRevoked(t) {
		t.Fatal("a reset answered as a failure revoked the other device")
	}
	res, err := f.svc.ConfirmPasswordReset(ctx, token, "brand-new-password-1", "ua", "ip")
	if err != nil {
		t.Fatalf("the link the failed confirm was given no longer resets: %v", err)
	}
	if !f.otherDeviceRevoked(t) {
		t.Fatal("the reset left the other device's session live")
	}
	mustRefresh(t, f, res.RefreshToken, "the session the reset issued")
}

// TestConfirmPasswordReset_ASpentLinkRevokesNothing: the revocation runs
// before the link is spent, so a spent link must be refused before it — a
// replay of an old link signs nobody out.
func TestConfirmPasswordReset_ASpentLinkRevokesNothing(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	token := seedReset(t, f.svc, f.userID)
	if _, err := f.svc.ConfirmPasswordReset(ctx, token, "brand-new-password-1", "ua", "ip"); err != nil {
		t.Fatalf("the reset: %v", err)
	}
	later, err := f.svc.Login(ctx, "rotation@example.org", "brand-new-password-1", "later-device", "ip3")
	if err != nil {
		t.Fatalf("a login with the new password: %v", err)
	}
	if _, err := f.svc.ConfirmPasswordReset(ctx, token, "replayed-password-2", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("a spent link replayed: got %v, want ErrInvalidCredentials", err)
	}
	mustRefresh(t, f, later.RefreshToken, "a session a spent link's replay must not touch")
}

// TestChangePasswordPending_EndsEverySessionOfThePendingAccount: the forced
// rotation replaces a credential an administrator declared spent, so every
// session of the account ends there — revoked before the new password lands,
// and a revocation that fails leaves the temporary password usable.
func TestChangePasswordPending_EndsEverySessionOfThePendingAccount(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	temp, err := HashPassword("temporary-pw-1")
	if err != nil {
		t.Fatal(err)
	}
	pending := identity.UserStatusPendingPasswordChange
	if _, err := f.ids.PatchUser(ctx, f.userID, identity.UserPatch{PasswordHash: &temp, Status: &pending}); err != nil {
		t.Fatalf("set the account pending: %v", err)
	}
	f.sessions.failRevokeAllArmed.Store(true)
	if _, err := f.svc.ChangePasswordPending(ctx, "rotation@example.org", "temporary-pw-1", "chosen-password-2", "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a forced rotation whose revocation fails: got %v, want the store's failure", err)
	}
	if u, err := f.ids.Store.GetUser(ctx, f.userID); err != nil || u.Status != identity.UserStatusPendingPasswordChange {
		t.Fatalf("a forced rotation answered as a failure moved the account: status=%q err=%v", u.Status, err)
	}
	res, err := f.svc.ChangePasswordPending(ctx, "rotation@example.org", "temporary-pw-1", "chosen-password-2", "ua", "ip")
	if err != nil {
		t.Fatalf("the forced rotation retried once the store answers: %v", err)
	}
	if !f.otherDeviceRevoked(t) {
		t.Fatal("a session born before the forced rotation is still live after it")
	}
	if _, err := f.svc.Refresh(ctx, f.other, "other-device", "ip2"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a session born before the forced rotation, presented after it: got %v, want ErrSessionRevoked", err)
	}
	mustRefresh(t, f, res.RefreshToken, "the session the forced rotation issued")
}

// TestRefresh_APendingAccountIsRefusedAndItsSessionRetired: an account set
// pending a password change refreshes nothing, and the session it presented
// is retired so the change it waits for does not bring it back.
func TestRefresh_APendingAccountIsRefusedAndItsSessionRetired(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	setStatus(t, f, identity.UserStatusPendingPasswordChange)
	_, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
	if !errors.Is(err, ErrSessionRevoked) || !errors.Is(err, errPasswordChangePending) {
		t.Fatalf("a refresh of an account pending a password change: got %v, want errPasswordChangePending", err)
	}
	if f.session(t, f.token).RevokedAt == nil {
		t.Fatal("the session a pending account presented was not retired")
	}
	setStatus(t, f, identity.UserStatusActive)
	if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the retired session once the account is active again: got %v, want ErrSessionRevoked", err)
	}
}

// TestLogout_AFailureIsNotASignOut: a sign-out whose read or revocation fails
// returns the failure, and the token it was given still refreshes — nothing
// ended — until a sign-out the store answers ends its lineage.
func TestLogout_AFailureIsNotASignOut(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{"revocation", "read"} {
		t.Run(step, func(t *testing.T) {
			f := newRotationFixture(t, errStoreUnreachable)
			if step == "read" {
				f.sessions.failGetArmed.Store(true)
			} else {
				f.sessions.failRevokeLineageArmed.Store(true)
			}
			if err := f.svc.Logout(ctx, f.token); err == nil {
				t.Fatalf("a sign-out whose %s fails answered as done", step)
			}
			if f.session(t, f.token).RevokedAt != nil {
				t.Fatalf("the sign-out whose %s failed revoked the session", step)
			}
			if err := f.svc.Logout(ctx, f.token); err != nil {
				t.Fatalf("the sign-out retried once the store answers: %v", err)
			}
			if _, err := f.svc.Refresh(ctx, f.token, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
				t.Fatalf("the token of the retried sign-out: got %v, want ErrSessionRevoked", err)
			}
		})
	}
}

// TestRegister_ReadsNoSessionGeneration: a registration's session is born
// under generation zero, read from nothing — the account was created by the
// same call — so a session store that cannot answer the generation fails no
// registration after its commit.
func TestRegister_ReadsNoSessionGeneration(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	f.sessions.failGeneration.Store(true)
	f.sessions.resetDeadlines()
	res, err := f.svc.Register(ctx, "fresh@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("a registration with the generation read failing: %v", err)
	}
	if n := len(f.sessions.recordedDeadlines()["SessionGeneration"]); n != 0 {
		t.Fatalf("a registration read the session generation %d time(s), want 0", n)
	}
	f.sessions.failGeneration.Store(false)
	if got := f.session(t, res.RefreshToken); got.Generation != 0 {
		t.Fatalf("the registration's session was born under generation %d, want 0", got.Generation)
	}
}

// TestPasswordFlows_TheNewSessionIsBornUnderTheirOwnRevocation: a flow that
// revokes every session of its account and then signs it in again must give
// that session — and the access token naming it — the generation its own
// revocation raised; born under the one before, the revocation would cover
// it and the user would be signed out at its first refresh. None of them
// reads the generation.
func TestPasswordFlows_TheNewSessionIsBornUnderTheirOwnRevocation(t *testing.T) {
	ctx := context.Background()
	for _, flow := range []string{"password change", "reset confirm", "forced rotation"} {
		t.Run(flow, func(t *testing.T) {
			f := newRotationFixture(t, errStoreUnreachable)
			f.sessions.failGeneration.Store(true)
			var res LoginResult
			var err error
			switch flow {
			case "password change":
				res, err = f.svc.ChangePassword(ctx, f.userID, "correct-horse-9", "another-horse-7", "ua", "ip")
			case "reset confirm":
				res, err = f.svc.ConfirmPasswordReset(ctx, seedReset(t, f.svc, f.userID), "another-horse-7", "ua", "ip")
			case "forced rotation":
				temp, herr := HashPassword("temporary-pw-1")
				if herr != nil {
					t.Fatal(herr)
				}
				pending := identity.UserStatusPendingPasswordChange
				if _, perr := f.ids.PatchUser(ctx, f.userID, identity.UserPatch{PasswordHash: &temp, Status: &pending}); perr != nil {
					t.Fatalf("set the account pending: %v", perr)
				}
				res, err = f.svc.ChangePasswordPending(ctx, "rotation@example.org", "temporary-pw-1", "another-horse-7", "ua", "ip")
			}
			if err != nil {
				t.Fatalf("the %s with the generation read failing: %v", flow, err)
			}
			f.sessions.failGeneration.Store(false)
			gen, err := f.sessions.SessionGeneration(ctx, f.userID)
			if err != nil {
				t.Fatalf("SessionGeneration: %v", err)
			}
			if got := f.session(t, res.RefreshToken); got.Generation != gen || gen == 0 {
				t.Fatalf("the %s's session was born under generation %d, the account is at %d (want equal, and raised)", flow, got.Generation, gen)
			}
			presented, err := f.svc.signer.Verify(res.AccessToken)
			if err != nil {
				t.Fatalf("verify the access token: %v", err)
			}
			if presented.SessionGeneration != gen || presented.SessionLineage != f.session(t, res.RefreshToken).ID {
				t.Fatalf("the access token names session %q generation %d, want %q and %d", presented.SessionLineage, presented.SessionGeneration, f.session(t, res.RefreshToken).ID, gen)
			}
			mustRefresh(t, f, res.RefreshToken, "the session the "+flow+" issued")
			if _, err := f.svc.SwitchTeam(ctx, presented, res.ActiveTeamID); err != nil {
				t.Fatalf("a switch with the access token the %s answered: %v", flow, err)
			}
		})
	}
}

// presentedAccess reads the identity an access token carries.
func presentedAccess(t *testing.T, svc *Service, token string) Identity {
	t.Helper()
	id, err := svc.signer.Verify(token)
	if err != nil {
		t.Fatalf("verify an access token: %v", err)
	}
	return id
}

// TestSwitch_ExtendsNothing: a switch re-scopes the presented access token
// and keeps its expiry and its session; a token naming no session (minted
// before access tokens named theirs) switches too, bounded by its own
// expiry.
func TestSwitch_ExtendsNothing(t *testing.T) {
	ctx := context.Background()
	f := newRotationFixture(t, errStoreUnreachable)
	// The login's token was minted a minute ago: a switch minting a fresh
	// lifetime would expire a minute after it.
	f.svc.signer.now = func() time.Time { return time.Now().Add(-time.Minute) }
	login, err := f.svc.Login(ctx, "rotation@example.org", "correct-horse-9", "ua", "ip")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	f.svc.signer.now = time.Now
	presented := presentedAccess(t, f.svc, login.AccessToken)
	if presented.SessionLineage == "" || presented.ExpiresAt.IsZero() {
		t.Fatalf("a login's access token names session %q, expiry %v: want both", presented.SessionLineage, presented.ExpiresAt)
	}
	for _, sw := range []struct {
		name string
		run  func(Identity) (SwitchResult, error)
	}{
		{"team", func(p Identity) (SwitchResult, error) { return f.svc.SwitchTeam(ctx, p, login.ActiveTeamID) }},
		{"org", func(p Identity) (SwitchResult, error) { return f.svc.SwitchOrg(ctx, p, login.ActiveOrgID) }},
	} {
		t.Run(sw.name, func(t *testing.T) {
			res, err := sw.run(presented)
			if err != nil {
				t.Fatalf("the switch: %v", err)
			}
			minted := presentedAccess(t, f.svc, res.AccessToken)
			if !minted.ExpiresAt.Equal(presented.ExpiresAt) || !res.AccessExpires.Equal(presented.ExpiresAt) {
				t.Fatalf("the switch minted a token expiring %v (answer %v), want the presented %v", minted.ExpiresAt, res.AccessExpires, presented.ExpiresAt)
			}
			if minted.SessionLineage != presented.SessionLineage || minted.SessionGeneration != presented.SessionGeneration {
				t.Fatalf("the switch minted a token naming session %q/%d, want %q/%d", minted.SessionLineage, minted.SessionGeneration, presented.SessionLineage, presented.SessionGeneration)
			}
			legacy := presented
			legacy.SessionLineage, legacy.SessionGeneration = "", 0
			old, err := sw.run(legacy)
			if err != nil {
				t.Fatalf("a token naming no session: %v", err)
			}
			if got := presentedAccess(t, f.svc, old.AccessToken).ExpiresAt; !got.Equal(presented.ExpiresAt) {
				t.Fatalf("a token naming no session switched into one expiring %v, want the presented %v", got, presented.ExpiresAt)
			}
		})
	}
}

// TestSwitch_RefusesWhatARefreshRefuses: once the session an access token
// names is revoked — its lineage signed out, its account signed out
// everywhere — or its account disabled or pending a password change, both
// switches refuse; a revocation read that fails is a failure, not a refusal.
func TestSwitch_RefusesWhatARefreshRefuses(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		do   func(t *testing.T, f *rotationFixture)
		want error
	}{
		{"its lineage signed out", func(t *testing.T, f *rotationFixture) {
			if err := f.svc.Logout(ctx, f.token); err != nil {
				t.Fatalf("Logout: %v", err)
			}
		}, ErrSessionRevoked},
		{"its account signed out everywhere", func(t *testing.T, f *rotationFixture) {
			if err := f.svc.RevokeUserSessions(ctx, f.userID); err != nil {
				t.Fatalf("RevokeUserSessions: %v", err)
			}
		}, ErrSessionRevoked},
		{"its account disabled", func(t *testing.T, f *rotationFixture) {
			setStatus(t, f, identity.UserStatusDisabled)
		}, ErrAccountDisabled},
		{"its account pending a password change", func(t *testing.T, f *rotationFixture) {
			setStatus(t, f, identity.UserStatusPendingPasswordChange)
		}, errPasswordChangePending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t, errStoreUnreachable)
			reg := f.session(t, f.token)
			login, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			presented := presentedAccess(t, f.svc, login.AccessToken)
			if presented.SessionLineage != reg.lineage() {
				t.Fatalf("a refresh's access token names lineage %q, want the session's %q", presented.SessionLineage, reg.lineage())
			}
			f.token = login.RefreshToken
			tc.do(t, f)
			if _, err := f.svc.SwitchTeam(ctx, presented, login.ActiveTeamID); !errors.Is(err, tc.want) {
				t.Fatalf("a team switch: got %v, want %v", err, tc.want)
			}
			if _, err := f.svc.SwitchOrg(ctx, presented, login.ActiveOrgID); !errors.Is(err, tc.want) {
				t.Fatalf("an org switch: got %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("a revocation read that fails", func(t *testing.T) {
		f := newRotationFixture(t, errStoreUnreachable)
		login, err := f.svc.Refresh(ctx, f.token, "ua", "ip")
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		f.sessions.failCheckArmed.Store(true)
		_, err = f.svc.SwitchTeam(ctx, presentedAccess(t, f.svc, login.AccessToken), login.ActiveTeamID)
		if !errors.Is(err, errStoreUnreachable) || errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("a switch whose revocation read fails: got %v, want the store's failure", err)
		}
	})
}
