package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// The typed refusal reasons the audit log records (ADR-157): every refusal
// site of a sign-in or credential-change path annotates the sentinel it
// answers with the reason it refused, and the sentinel stays what the wire
// reads (errors.Is).

// wantReason fails unless err is ErrInvalidCredentials annotated with want.
func wantReason(t *testing.T, what string, err error, want RefusalReason) {
	t.Helper()
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("%s: got %v, want ErrInvalidCredentials", what, err)
	}
	got, ok := ReasonOf(err)
	if !ok || got != want {
		t.Fatalf("%s: ReasonOf = %q, %v, want %q", what, got, ok, want)
	}
	if err.Error() != ErrInvalidCredentials.Error() {
		t.Fatalf("%s: the wire message %q is not the sentinel's", what, err.Error())
	}
}

func TestReasonOf_EverySignInRefusalNamesItsReason(t *testing.T) {
	ctx := context.Background()
	disabled := identity.UserStatusDisabled

	t.Run("login", func(t *testing.T) {
		f := newChangeFixture(t)
		_, err := f.svc.Login(ctx, "nobody@example.org", changeOldPassword, "ua", "ip")
		wantReason(t, "an unknown address", err, ReasonUnknownAccount)
		_, err = f.svc.Login(ctx, f.email, "not-the-password", "ua", "ip")
		wantReason(t, "a wrong password", err, ReasonBadPassword)
		until := time.Now().Add(time.Hour)
		if _, err := f.ids.Store.PatchUser(ctx, f.userID, identity.UserPatch{Lockout: &identity.UserLockout{FailedLogins: LockoutThreshold, LockedUntil: &until}}); err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.Login(ctx, f.email, changeOldPassword, "ua", "ip")
		wantReason(t, "a locked account", err, ReasonLocked)
	})

	t.Run("reset confirm without a reset store", func(t *testing.T) {
		svc, err := NewService(Config{Store: identity.NewMemoryStore(), Sessions: NewMemorySessionStore(), Signer: newTestSigner(t), SignupMode: SignupOpen, RefreshTTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.ConfirmPasswordReset(ctx, ResetTokenPrefix+"x", changeNewPassword, "ua", "ip")
		wantReason(t, "a deployment with no reset store", err, ReasonResetUnavailable)
	})

	t.Run("reset confirm", func(t *testing.T) {
		f := newChangeFixture(t)
		_, err := f.svc.ConfirmPasswordReset(ctx, ResetTokenPrefix+"unknown", changeNewPassword, "ua", "ip")
		wantReason(t, "an unknown link", err, ReasonResetTokenInvalid)

		link := seedReset(t, f.svc, f.userID)
		if _, err := f.ids.Store.PatchUser(ctx, f.userID, identity.UserPatch{Status: &disabled}); err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.ConfirmPasswordReset(ctx, link, changeNewPassword, "ua", "ip")
		wantReason(t, "a link of a disabled account", err, ReasonDisabled)
	})

	t.Run("reset confirm whose commit a disable beats", func(t *testing.T) {
		f := newChangeFixture(t)
		link := seedReset(t, f.svc, f.userID)
		f.ids.beforeCommit = func() {
			if _, err := f.ids.Store.PatchUser(ctx, f.userID, identity.UserPatch{Status: &disabled}); err != nil {
				t.Errorf("disable: %v", err)
			}
		}
		_, err := f.svc.ConfirmPasswordReset(ctx, link, changeNewPassword, "ua", "ip")
		wantReason(t, "a reset whose commit a disable beat", err, ReasonDisabled)
	})

	t.Run("reset confirm whose link another redemption claimed", func(t *testing.T) {
		f := newChangeFixture(t)
		f.svc.resets = &claimedResets{MemoryPasswordResetStore: f.resets}
		link := seedReset(t, f.svc, f.userID)
		_, err := f.svc.ConfirmPasswordReset(ctx, link, changeNewPassword, "ua", "ip")
		wantReason(t, "a link claimed by another redemption", err, ReasonResetTokenInvalid)
	})

	t.Run("password change", func(t *testing.T) {
		f := newChangeFixture(t)
		_, err := f.svc.ChangePassword(ctx, "no-such-user", changeOldPassword, changeNewPassword, "ua", "ip")
		wantReason(t, "an account that does not exist", err, ReasonUnknownAccount)
		_, err = f.svc.ChangePassword(ctx, f.userID, "not-the-password", changeNewPassword, "ua", "ip")
		wantReason(t, "a wrong current password", err, ReasonBadPassword)
		if _, err := f.ids.Store.PatchUser(ctx, f.userID, identity.UserPatch{Status: &disabled}); err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.ChangePassword(ctx, f.userID, changeOldPassword, changeNewPassword, "ua", "ip")
		wantReason(t, "a disabled account", err, ReasonWrongStatus)
	})

	t.Run("password change whose commit a replacement beats", func(t *testing.T) {
		f := newChangeFixture(t)
		f.ids.beforeCommit = replacePassword(t, f)
		_, err := f.svc.ChangePassword(ctx, f.userID, changeOldPassword, changeNewPassword, "ua", "ip")
		wantReason(t, "a change whose commit a replacement beat", err, ReasonCredentialReplaced)
	})

	t.Run("password change whose commit a disable beats", func(t *testing.T) {
		f := newChangeFixture(t)
		f.ids.beforeCommit = func() {
			if _, err := f.ids.Store.PatchUser(ctx, f.userID, identity.UserPatch{Status: &disabled}); err != nil {
				t.Errorf("disable: %v", err)
			}
		}
		_, err := f.svc.ChangePassword(ctx, f.userID, changeOldPassword, changeNewPassword, "ua", "ip")
		wantReason(t, "a change whose commit a disable beat", err, ReasonWrongStatus)
	})

	t.Run("forced rotation", func(t *testing.T) {
		f := newChangeFixture(t)
		_, err := f.svc.ChangePasswordPending(ctx, f.email, changeOldPassword, changeNewPassword, "ua", "ip")
		wantReason(t, "a rotation of an account not pending one", err, ReasonWrongStatus)
		pending := identity.UserStatusPendingPasswordChange
		if _, err := f.ids.Store.PatchUser(ctx, f.userID, identity.UserPatch{Status: &pending}); err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.ChangePasswordPending(ctx, "nobody@example.org", changeOldPassword, changeNewPassword, "ua", "ip")
		wantReason(t, "an unknown address", err, ReasonUnknownAccount)
		_, err = f.svc.ChangePasswordPending(ctx, f.email, "not-the-password", changeNewPassword, "ua", "ip")
		wantReason(t, "a wrong temporary password", err, ReasonBadPassword)
		f.ids.beforeCommit = replacePassword(t, f)
		_, err = f.svc.ChangePasswordPending(ctx, f.email, changeOldPassword, changeNewPassword, "ua", "ip")
		wantReason(t, "a rotation whose commit a replacement beat", err, ReasonCredentialReplaced)
	})
}

// replacePassword is a beforeCommit hook that replaces the account's
// password underneath the change — an admin reset landing mid-change.
func replacePassword(t *testing.T, f *changeFixture) func() {
	return func() {
		hash, err := HashPassword("an-admin-set-password-1")
		if err != nil {
			t.Errorf("hash: %v", err)
			return
		}
		if _, err := f.ids.Store.PatchUser(context.Background(), f.userID, identity.UserPatch{PasswordHash: &hash}); err != nil {
			t.Errorf("replace the password: %v", err)
		}
	}
}

// claimedResets is a reset store whose Consume finds the link already
// claimed: another redemption of the same link won it.
type claimedResets struct {
	*MemoryPasswordResetStore
}

func (claimedResets) Consume(context.Context, string, time.Time) (bool, error) {
	return false, nil
}
