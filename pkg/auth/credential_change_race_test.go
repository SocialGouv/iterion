package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// recordHook runs a hook right before the n-th account revocation record.
type recordHook struct {
	SessionStore
	mu     sync.Mutex
	calls  int
	onCall map[int]func()
}

func (h *recordHook) RecordAccountRevocation(ctx context.Context, userID string, at time.Time) (int64, error) {
	h.mu.Lock()
	h.calls++
	fn := h.onCall[h.calls]
	delete(h.onCall, h.calls)
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
	return h.SessionStore.RecordAccountRevocation(ctx, userID, at)
}

// credentialChangeFlows runs each credential flow that commits a new password
// for the account of email, which is registered with oldPassword: the
// in-app change, the emailed reset's confirm (the link it redeems is left in
// *resetToken), and the forced rotation of a pending account (setPending
// flips it first).
func credentialChangeFlows(svc *Service, resets PasswordResetStore, resetToken *string, setPending func()) map[string]func(ctx context.Context, userID, email, oldPassword, newPassword string) (LoginResult, error) {
	return map[string]func(ctx context.Context, userID, email, oldPassword, newPassword string) (LoginResult, error){
		"password change": func(ctx context.Context, userID, _, oldPassword, newPassword string) (LoginResult, error) {
			return svc.ChangePassword(ctx, userID, oldPassword, newPassword, "ua", "ip")
		},
		"reset confirm": func(ctx context.Context, userID, _, _, newPassword string) (LoginResult, error) {
			token := ResetTokenPrefix + uuid.NewString()
			*resetToken = token
			now := time.Now().UTC()
			if err := resets.Create(ctx, PasswordReset{ID: uuid.NewString(), UserID: userID, TokenHash: HashRefreshToken(token), CreatedAt: now, ExpiresAt: now.Add(ResetTokenTTL)}); err != nil {
				return LoginResult{}, err
			}
			return svc.ConfirmPasswordReset(ctx, token, newPassword, "ua", "ip")
		},
		"forced rotation": func(ctx context.Context, _, email, oldPassword, newPassword string) (LoginResult, error) {
			setPending()
			return svc.ChangePasswordPending(ctx, email, oldPassword, newPassword, "ua", "ip")
		},
	}
}

// TestCredentialChange_ADisableLandingAfterTheCommitLeavesNoSession: an admin
// disable — the status, then "revoke every session" — landing between a
// credential change's commit and the revocation after it records a
// generation below the one the change's session would be opened under; the
// change opens no session for the account it disabled, so a later re-enable
// resurrects nothing.
func TestCredentialChange_ADisableLandingAfterTheCommitLeavesNoSession(t *testing.T) {
	for _, flow := range []string{"password change", "reset confirm", "forced rotation"} {
		t.Run(flow, func(t *testing.T) {
			ctx := context.Background()
			ids := identity.NewMemoryStore()
			hook := &recordHook{SessionStore: NewMemorySessionStore(), onCall: map[int]func(){}}
			svc := newTestServiceOn(t, ids, hook)
			resets := NewMemoryPasswordResetStore()
			svc.resets = resets
			reg, err := svc.Register(ctx, "disable@example.org", "pw-zero-000", "", "", "ua", "ip")
			if err != nil {
				t.Fatal(err)
			}
			uid := reg.User.ID
			disabled, active, pending := identity.UserStatusDisabled, identity.UserStatusActive, identity.UserStatusPendingPasswordChange
			hook.mu.Lock()
			hook.calls = 0
			hook.onCall[2] = func() { // the change's revocation after its commit
				if _, err := ids.PatchUser(ctx, uid, identity.UserPatch{Status: &disabled}); err != nil {
					t.Errorf("admin disable: %v", err)
				}
				if err := svc.RevokeUserSessions(ctx, uid); err != nil {
					t.Errorf("admin disable's revocation: %v", err)
				}
			}
			hook.mu.Unlock()
			setPending := func() {
				if _, err := ids.PatchUser(ctx, uid, identity.UserPatch{Status: &pending}); err != nil {
					t.Fatal(err)
				}
			}
			var token string
			res, err := credentialChangeFlows(svc, resets, &token, setPending)[flow](ctx, uid, "disable@example.org", "pw-zero-000", "pw-one-111")
			if !errors.Is(err, ErrPasswordChangedSignIn) || res.RefreshToken != "" || res.AccessToken != "" {
				t.Fatalf("a %s a disable landed after: session delivered=%t err=%v, want ErrPasswordChangedSignIn and no session", flow, res.RefreshToken != "", err)
			}
			if _, err := ids.PatchUser(ctx, uid, identity.UserPatch{Status: &active}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Login(ctx, "disable@example.org", "pw-one-111", "ua", "ip"); err != nil {
				t.Fatalf("the committed password after the re-enable: %v", err)
			}
		})
	}
}

// lostCommitStore wraps the identity store: a credential change's commit (a
// patch carrying a new password hash) first lets beforeCommit run, then lands
// and answers lost — what a cancelled request or a dropped connection makes
// of a write the server applied.
type lostCommitStore struct {
	identity.Store
	mu           sync.Mutex
	beforeCommit func()
}

var errAnswerLost = errors.New("connection reset: the answer of the commit was lost")

func (s *lostCommitStore) PatchUser(ctx context.Context, id string, p identity.UserPatch) (identity.User, error) {
	if p.PasswordHash == nil {
		return s.Store.PatchUser(ctx, id, p)
	}
	s.mu.Lock()
	fn := s.beforeCommit
	s.beforeCommit = nil
	s.mu.Unlock()
	if fn == nil {
		return s.Store.PatchUser(ctx, id, p)
	}
	fn()
	if _, err := s.Store.PatchUser(ctx, id, p); err != nil {
		return identity.User{}, err
	}
	return identity.User{}, errAnswerLost
}

// TestCredentialChange_ACommitWhoseAnswerIsLostStillRevokes: a commit whose
// answer is lost may have landed; the revocation that follows a commit runs
// anyway — the account's generation is raised once before the commit and
// once after it, whatever the commit answered — so a sign-in with the old
// password that raced the change dies with it, and an emailed reset's link
// stays spent.
func TestCredentialChange_ACommitWhoseAnswerIsLostStillRevokes(t *testing.T) {
	for _, flow := range []string{"password change", "reset confirm", "forced rotation", "admin reset"} {
		t.Run(flow, func(t *testing.T) {
			ctx := context.Background()
			ids := &lostCommitStore{Store: identity.NewMemoryStore()}
			sessions := NewMemorySessionStore()
			svc := newTestServiceOn(t, ids, sessions)
			resets := NewMemoryPasswordResetStore()
			svc.resets = resets
			reg, err := svc.Register(ctx, "lost@example.org", "pw-zero-000", "", "", "ua", "ip")
			if err != nil {
				t.Fatal(err)
			}
			uid := reg.User.ID
			pending := identity.UserStatusPendingPasswordChange
			setPending := func() {
				if _, err := ids.Store.PatchUser(ctx, uid, identity.UserPatch{Status: &pending}); err != nil {
					t.Fatal(err)
				}
			}
			var raced LoginResult
			ids.mu.Lock()
			ids.beforeCommit = func() {
				// A sign-in with the old password, between the revocation
				// before the commit and the commit. A pending account's
				// sign-in is sent to its rotation, so the race is the
				// session a forced rotation would leave: opened before it.
				if flow == "forced rotation" || flow == "admin reset" {
					return
				}
				var lerr error
				if raced, lerr = svc.Login(ctx, "lost@example.org", "pw-zero-000", "old", "ip"); lerr != nil {
					t.Errorf("the old-password sign-in racing the change: %v", lerr)
				}
			}
			ids.mu.Unlock()
			before, err := sessions.SessionGeneration(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			var token string
			var res LoginResult
			if flow == "admin reset" {
				hash, herr := HashPassword("temp-password-1")
				if herr != nil {
					t.Fatal(herr)
				}
				_, err = svc.ResetPasswordByAdmin(ctx, uid, hash)
			} else {
				res, err = credentialChangeFlows(svc, resets, &token, setPending)[flow](ctx, uid, "lost@example.org", "pw-zero-000", "pw-one-111")
			}
			if !errors.Is(err, errAnswerLost) || res.RefreshToken != "" {
				t.Fatalf("a %s whose commit answer was lost: session delivered=%t err=%v, want the lost answer and no session", flow, res.RefreshToken != "", err)
			}
			svc.WaitRevocationPasses()
			if after, err := sessions.SessionGeneration(ctx, uid); err != nil || after != before+2 {
				t.Fatalf("a %s whose commit answer was lost raised the account's generation %d -> %d (%v), want once before the commit and once after it", flow, before, after, err)
			}
			if raced.RefreshToken != "" {
				if _, err := svc.Refresh(ctx, raced.RefreshToken, "old", "ip"); !errors.Is(err, ErrSessionRevoked) {
					t.Fatalf("the old-password session that raced a %s whose commit answer was lost: refresh %v, want ErrSessionRevoked", flow, err)
				}
			}
			if flow == "reset confirm" {
				rec, err := resets.GetByTokenHash(ctx, HashRefreshToken(token))
				if err != nil {
					t.Fatal(err)
				}
				if rec.ConsumedAt == nil {
					t.Fatalf("the reset link of a confirm whose commit answer was lost was given back: %+v", rec)
				}
			}
		})
	}
}
