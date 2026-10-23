package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// A credential change — a password change, a reset confirm, the forced
// rotation, an admin reset — revokes before it commits and again after, holds
// the account's credential-change claim throughout, and answers a failure
// once its new password is committed as ErrPasswordChangedSignIn; a password
// sign-in reads its session generation before a stamp that compares-and-sets
// on the hash it verified.

// changeHooks is an identity store that runs, once each, a hook right before
// a credential commit (a patch writing a password hash), right before a
// password sign-in's stamp (a patch naming the hash it verified and writing
// no hash), and right after that stamp landed.
type changeHooks struct {
	identity.Store
	mu                                    sync.Mutex
	beforeCommit, beforeStamp, afterStamp func()
}

func (h *changeHooks) take(slot *func()) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn := *slot
	*slot = nil
	return fn
}

func (h *changeHooks) PatchUser(ctx context.Context, id string, p identity.UserPatch) (identity.User, error) {
	stamp := p.PasswordHash == nil && p.IfPasswordHash != nil && p.LastLoginAt != nil
	if p.PasswordHash != nil {
		if fn := h.take(&h.beforeCommit); fn != nil {
			fn()
		}
	}
	if stamp {
		if fn := h.take(&h.beforeStamp); fn != nil {
			fn()
		}
	}
	u, err := h.Store.PatchUser(ctx, id, p)
	if stamp && err == nil {
		if fn := h.take(&h.afterStamp); fn != nil {
			fn()
		}
	}
	return u, err
}

// changeFixture is a service on hooked stores holding one account, its
// victim's session, and a reset link for it.
type changeFixture struct {
	svc      *Service
	ids      *changeHooks
	sessions *MemorySessionStore
	resets   *MemoryPasswordResetStore
	userID   string
	victim   string
	email    string
}

const (
	changeOldPassword = "old-password-123"
	changeNewPassword = "brand-new-password-456"
)

func newChangeFixture(t *testing.T) *changeFixture {
	t.Helper()
	ctx := context.Background()
	f := &changeFixture{
		ids:      &changeHooks{Store: identity.NewMemoryStore()},
		sessions: NewMemorySessionStore(),
		resets:   NewMemoryPasswordResetStore(),
		email:    "victim@example.org",
	}
	svc, err := NewService(Config{Store: f.ids, Sessions: f.sessions, Signer: newTestSigner(t), SignupMode: SignupOpen, RefreshTTL: time.Hour, Resets: f.resets})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.svc = svc
	reg, err := svc.Register(ctx, f.email, changeOldPassword, "", "", "victim-laptop", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	f.userID, f.victim = reg.User.ID, reg.RefreshToken
	return f
}

// change runs the named credential flow from the old password to the new one.
func (f *changeFixture) change(t *testing.T, flow string) (LoginResult, error) {
	t.Helper()
	ctx := context.Background()
	switch flow {
	case "password change":
		return f.svc.ChangePassword(ctx, f.userID, changeOldPassword, changeNewPassword, "victim-laptop", "ip")
	case "reset confirm":
		return f.svc.ConfirmPasswordReset(ctx, seedReset(t, f.svc, f.userID), changeNewPassword, "victim-laptop", "ip")
	}
	t.Fatalf("unknown flow %q", flow)
	return LoginResult{}, nil
}

// mustNotSurvive fails the test unless the session login was handed is
// refused at its refresh and its access token at a switch.
func (f *changeFixture) mustNotSurvive(t *testing.T, login LoginResult, what string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.svc.Refresh(ctx, login.RefreshToken, "attacker", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("%s: its session's refresh after the change: got %v, want ErrSessionRevoked", what, err)
	}
	access := presentedAccess(t, f.svc, login.AccessToken)
	if _, err := f.svc.SwitchTeam(ctx, access, login.ActiveTeamID); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("%s: its access token's switch after the change: got %v, want ErrSessionRevoked", what, err)
	}
}

// TestCredentialChange_ASignInWithTheOldPasswordDuringTheChangeEndsWithIt:
// however a sign-in with the old password interleaves with the change, its
// session does not outlive it — a sign-in wholly between the revocation and
// the commit is covered by the revocation after the commit; one that verified
// the old password before the commit and stamps after it is refused by its
// stamp; one whose stamp landed before the whole change read its generation
// before that stamp.
func TestCredentialChange_ASignInWithTheOldPasswordDuringTheChangeEndsWithIt(t *testing.T) {
	ctx := context.Background()
	for _, flow := range []string{"password change", "reset confirm"} {
		t.Run(flow+": a sign-in wholly between the revocation and the commit", func(t *testing.T) {
			f := newChangeFixture(t)
			var attacker LoginResult
			var aerr error
			f.ids.beforeCommit = func() {
				attacker, aerr = f.svc.Login(ctx, f.email, changeOldPassword, "attacker", "ip")
			}
			if _, err := f.change(t, flow); err != nil {
				t.Fatalf("the %s: %v", flow, err)
			}
			if aerr != nil {
				t.Fatalf("the fixture is wrong: the sign-in before the commit failed: %v", aerr)
			}
			f.mustNotSurvive(t, attacker, "a sign-in between the revocation and the commit")
		})
		t.Run(flow+": a sign-in that verified the old password, stamping after the commit", func(t *testing.T) {
			f := newChangeFixture(t)
			f.ids.beforeStamp = func() {
				if _, err := f.change(t, flow); err != nil {
					t.Errorf("the %s: %v", flow, err)
				}
			}
			if res, err := f.svc.Login(ctx, f.email, changeOldPassword, "attacker", "ip"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("a sign-in whose verified password was replaced before its stamp: got %v (a session: %t), want ErrInvalidCredentials", err, res.RefreshToken != "")
			}
		})
		t.Run(flow+": the whole change between a sign-in's stamp and its session", func(t *testing.T) {
			f := newChangeFixture(t)
			f.ids.afterStamp = func() {
				if _, err := f.change(t, flow); err != nil {
					t.Errorf("the %s: %v", flow, err)
				}
			}
			attacker, err := f.svc.Login(ctx, f.email, changeOldPassword, "attacker", "ip")
			if err != nil {
				t.Fatalf("the sign-in whose stamp preceded the change: %v", err)
			}
			f.mustNotSurvive(t, attacker, "a sign-in stamped before the change")
		})
	}
}

// failingCreate is a session store whose CreateSession fails while armed.
type failingCreate struct {
	*MemorySessionStore
	armed bool
}

func (s *failingCreate) CreateSession(ctx context.Context, x Session) error {
	if s.armed {
		return errStoreUnreachable
	}
	return s.MemorySessionStore.CreateSession(ctx, x)
}

// TestCredentialChange_ACommittedPasswordWhoseSessionFailedSaysSo: once the
// new password is committed, a session write that fails is
// ErrPasswordChangedSignIn — the password is changed (the old one refused,
// the new one signs in), the link is spent — never a failure the caller
// retries into "invalid credentials" believing nothing happened.
func TestCredentialChange_ACommittedPasswordWhoseSessionFailedSaysSo(t *testing.T) {
	ctx := context.Background()
	for _, flow := range []string{"password change", "reset confirm", "forced rotation"} {
		t.Run(flow, func(t *testing.T) {
			ids := identity.NewMemoryStore()
			sessions := &failingCreate{MemorySessionStore: NewMemorySessionStore()}
			svc, err := NewService(Config{Store: ids, Sessions: sessions, Signer: newTestSigner(t), SignupMode: SignupOpen, RefreshTTL: time.Hour, Resets: NewMemoryPasswordResetStore()})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			reg, err := svc.Register(ctx, "changer@example.org", changeOldPassword, "", "", "ua", "ip")
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			var link string
			switch flow {
			case "reset confirm":
				link = seedReset(t, svc, reg.User.ID)
			case "forced rotation":
				pending := identity.UserStatusPendingPasswordChange
				if _, err := ids.PatchUser(ctx, reg.User.ID, identity.UserPatch{Status: &pending}); err != nil {
					t.Fatalf("set pending: %v", err)
				}
			}
			sessions.armed = true
			switch flow {
			case "password change":
				_, err = svc.ChangePassword(ctx, reg.User.ID, changeOldPassword, changeNewPassword, "ua", "ip")
			case "reset confirm":
				_, err = svc.ConfirmPasswordReset(ctx, link, changeNewPassword, "ua", "ip")
			case "forced rotation":
				_, err = svc.ChangePasswordPending(ctx, "changer@example.org", changeOldPassword, changeNewPassword, "ua", "ip")
			}
			if !errors.Is(err, ErrPasswordChangedSignIn) {
				t.Fatalf("the %s whose session write failed after the commit: got %v, want ErrPasswordChangedSignIn", flow, err)
			}
			if !errors.Is(err, errStoreUnreachable) {
				t.Fatalf("the answer lost its cause: %v", err)
			}
			sessions.armed = false
			if _, err := svc.Login(ctx, "changer@example.org", changeOldPassword, "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("the old password after the %s: got %v, want refused", flow, err)
			}
			if _, err := svc.Login(ctx, "changer@example.org", changeNewPassword, "ua", "ip"); err != nil {
				t.Fatalf("the new password after the %s: %v", flow, err)
			}
			if flow == "reset confirm" {
				if _, err := svc.ConfirmPasswordReset(ctx, link, "a-third-password-789", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
					t.Fatalf("the link presented again: got %v, want ErrInvalidCredentials (spent)", err)
				}
			}
		})
	}
}

// TestCredentialChange_TwoSubmissionsAnswerNoDeadSession: two submissions of
// one change — a double click, a client's retry, the same reset link in two
// tabs — run one after the other under the account's claim: the second finds
// the password it verified replaced (or the link claimed) and revokes
// nothing, so the session the first answered with stays live.
func TestCredentialChange_TwoSubmissionsAnswerNoDeadSession(t *testing.T) {
	ctx := context.Background()
	for _, flow := range []string{"password change", "reset confirm"} {
		t.Run(flow, func(t *testing.T) {
			f := newChangeFixture(t)
			link := ""
			if flow == "reset confirm" {
				link = seedReset(t, f.svc, f.userID)
			}
			submit := func() (LoginResult, error) {
				if flow == "reset confirm" {
					return f.svc.ConfirmPasswordReset(ctx, link, changeNewPassword, "victim-laptop", "ip")
				}
				return f.svc.ChangePassword(ctx, f.userID, changeOldPassword, changeNewPassword, "victim-laptop", "ip")
			}
			// The second submission starts while the first sits between its
			// revocation and its commit.
			var second LoginResult
			var secondErr error
			done := make(chan struct{})
			f.ids.beforeCommit = func() {
				go func() {
					defer close(done)
					second, secondErr = submit()
				}()
				// Long enough for the second to reach the claim, if nothing
				// held it back.
				time.Sleep(200 * time.Millisecond)
			}
			first, err := submit()
			if err != nil {
				t.Fatalf("the first submission: %v", err)
			}
			<-done
			if !errors.Is(secondErr, ErrInvalidCredentials) {
				t.Fatalf("the second submission: got %v (a session: %t), want ErrInvalidCredentials", secondErr, second.RefreshToken != "")
			}
			if _, err := f.svc.Refresh(ctx, first.RefreshToken, "victim-laptop", "ip"); err != nil {
				t.Fatalf("the session the first submission answered with is dead: %v", err)
			}
			access := presentedAccess(t, f.svc, first.AccessToken)
			if _, err := f.svc.SwitchTeam(ctx, access, first.ActiveTeamID); err != nil {
				t.Fatalf("the first submission's access token is refused by a switch: %v", err)
			}
		})
	}
}

// failingRecord is a session store whose account revocation record fails
// while armed.
type failingRecord struct {
	*MemorySessionStore
	armed bool
}

func (s *failingRecord) RecordAccountRevocation(ctx context.Context, userID string, at time.Time) (int64, error) {
	if s.armed {
		return 0, errStoreUnreachable
	}
	return s.MemorySessionStore.RecordAccountRevocation(ctx, userID, at)
}

// TestConfirmPasswordReset_ARevocationThatFailsGivesTheLinkBack: the link is
// claimed before anything is revoked, and given back when the revocation
// fails: the same link then completes the reset.
func TestConfirmPasswordReset_ARevocationThatFailsGivesTheLinkBack(t *testing.T) {
	ctx := context.Background()
	ids := identity.NewMemoryStore()
	sessions := &failingRecord{MemorySessionStore: NewMemorySessionStore()}
	svc, err := NewService(Config{Store: ids, Sessions: sessions, Signer: newTestSigner(t), SignupMode: SignupOpen, RefreshTTL: time.Hour, Resets: NewMemoryPasswordResetStore()})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	reg, err := svc.Register(ctx, "resetter@example.org", changeOldPassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	link := seedReset(t, svc, reg.User.ID)
	sessions.armed = true
	if _, err := svc.ConfirmPasswordReset(ctx, link, changeNewPassword, "ua", "ip"); !errors.Is(err, errStoreUnreachable) {
		t.Fatalf("a reset whose revocation failed: got %v, want the store's failure", err)
	}
	sessions.armed = false
	if _, err := svc.ConfirmPasswordReset(ctx, link, changeNewPassword, "ua", "ip"); err != nil {
		t.Fatalf("the same link once the store answers: %v", err)
	}
}

// failingPass is a session store whose revocation passes always fail.
type failingPass struct {
	*MemorySessionStore
	mu       sync.Mutex
	attempts int
}

func (s *failingPass) RevokeAccountSessions(context.Context, string, int64, time.Time) (int64, error) {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
	return 0, errStoreUnreachable
}

func (s *failingPass) RevokeLineageSessions(context.Context, string, string, time.Time) (int64, error) {
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
	return 0, errStoreUnreachable
}

// TestRevocation_ThePassIsNotTheAnswer: a revocation answers from its record.
// A pass that fails every attempt — a store slow on a long refresh history,
// or down — fails no sign-out and no password change, and every session the
// record covers is refused all the same; the pass is retried.
func TestRevocation_ThePassIsNotTheAnswer(t *testing.T) {
	saved := revocationPassBackoff
	revocationPassBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { revocationPassBackoff = saved })
	ctx := context.Background()
	ids := identity.NewMemoryStore()
	sessions := &failingPass{MemorySessionStore: NewMemorySessionStore()}
	svc, err := NewService(Config{Store: ids, Sessions: sessions, Signer: newTestSigner(t), SignupMode: SignupOpen, RefreshTTL: time.Hour})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	reg, err := svc.Register(ctx, "pass@example.org", changeOldPassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	other, err := svc.Login(ctx, "pass@example.org", changeOldPassword, "other", "ip")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	changed, err := svc.ChangePassword(ctx, reg.User.ID, changeOldPassword, changeNewPassword, "ua", "ip")
	if err != nil {
		t.Fatalf("a password change whose pass fails: %v", err)
	}
	if _, err := svc.Refresh(ctx, other.RefreshToken, "other", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the other device after the change: got %v, want ErrSessionRevoked", err)
	}
	if err := svc.Logout(ctx, changed.RefreshToken); err != nil {
		t.Fatalf("a sign-out whose pass fails: %v", err)
	}
	if _, err := svc.Refresh(ctx, changed.RefreshToken, "ua", "ip"); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the signed-out session: got %v, want ErrSessionRevoked", err)
	}
	svc.WaitRevocationPasses()
	sessions.mu.Lock()
	attempts := sessions.attempts
	sessions.mu.Unlock()
	if want := 3 * revocationPassAttempts; attempts != want {
		t.Fatalf("the passes ran %d attempt(s), want %d (three revocations, each retried)", attempts, want)
	}
}

// TestLoginWithExternal_RefusesAnAccountPendingAPasswordChange: an SSO
// sign-in of an account an administrator set pending a password change opens
// no session — the returning identity and the auto-linked one alike — and
// links nothing.
func TestLoginWithExternal_RefusesAnAccountPendingAPasswordChange(t *testing.T) {
	ctx := context.Background()
	f := newChangeFixture(t)
	f.svc.trustedAutoLinkProviders = map[string]struct{}{"oidc-test": {}}
	pending := identity.UserStatusPendingPasswordChange
	if _, err := f.ids.Store.PatchUser(ctx, f.userID, identity.UserPatch{Status: &pending}); err != nil {
		t.Fatalf("set pending: %v", err)
	}
	ext := oidc.ExternalUser{Provider: "oidc-test", Subject: "subject-1", Email: f.email}
	if res, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip"); !errors.Is(err, ErrPasswordChangeRequired) {
		t.Fatalf("an auto-linked SSO sign-in of a pending account: got %v (a session: %t), want ErrPasswordChangeRequired", err, res.RefreshToken != "")
	}
	if _, err := f.ids.GetOIDCLink(ctx, "oidc-test", "subject-1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the refused sign-in linked the identity: %v", err)
	}
	if err := f.ids.UpsertOIDCLink(ctx, identity.OIDCLink{Provider: "oidc-test", ProviderUserID: "subject-1", UserID: f.userID, Email: f.email, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("link: %v", err)
	}
	if res, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip"); !errors.Is(err, ErrPasswordChangeRequired) {
		t.Fatalf("a returning SSO sign-in of a pending account: got %v (a session: %t), want ErrPasswordChangeRequired", err, res.RefreshToken != "")
	}
	active := identity.UserStatusActive
	if _, err := f.ids.Store.PatchUser(ctx, f.userID, identity.UserPatch{Status: &active}); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if _, err := f.svc.LoginWithExternal(ctx, ext, "ua", "ip"); err != nil {
		t.Fatalf("the same SSO sign-in once the account is active: %v", err)
	}
}
