package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// The current-password proof of POST /api/me/password stands in for the
// sign-in: a pinned, non-minting credential gets no session from it, and it
// is throttled and lockout-counted like the login it proves.

// TestAPersonalAccessTokenChangesNoPassword: a personal access token is
// refused (403) at POST /api/me/password — the route answers with a session,
// and a PAT must not be upgraded into one that mints. The answer carries no
// access token and sets no cookie, and the password is unchanged.
func TestAPersonalAccessTokenChangesNoPassword(t *testing.T) {
	f := newDerivationFixture(t)
	code, pat := f.mintPAT(t, f.user.AccessToken, "ci")
	if code != http.StatusCreated {
		t.Fatalf("mint a PAT: %d", code)
	}
	code, header, body := f.call(t, http.MethodPost, "/api/me/password", pat,
		`{"current_password":"`+fixturePassword+`","new_password":"new-password-forever-1"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a PAT changed the account password: %d %s", code, body)
	}
	if header.Get("Set-Cookie") != "" {
		t.Fatalf("the refusal set a cookie: %q", header.Get("Set-Cookie"))
	}
	if strings.Contains(body, "access_token") {
		t.Fatalf("the refusal carried an access token: %s", body)
	}
	if _, err := f.svc.Login(context.Background(), fixtureEmail, fixturePassword, "ua", "ip"); err != nil {
		t.Fatalf("the old password stopped working after the refused change: %v", err)
	}

	// Control: the account's own live session still changes the password,
	// and its answer IS a session (cookies + access token).
	code, header, body = f.call(t, http.MethodPost, "/api/me/password", f.user.AccessToken,
		`{"current_password":"`+fixturePassword+`","new_password":"new-password-forever-2"}`)
	if code != http.StatusOK {
		t.Fatalf("control: the live session could not change its password: %d %s", code, body)
	}
	if header.Get("Set-Cookie") == "" || !strings.Contains(body, "access_token") {
		t.Fatalf("control: the changed password answered no session: %d %s", code, body)
	}
	if _, err := f.svc.Login(context.Background(), fixtureEmail, "new-password-forever-2", "ua", "ip"); err != nil {
		t.Fatalf("control: the new password does not sign in: %v", err)
	}
}

// TestTheCurrentPasswordProofFeedsTheLoginLockout: wrong current-password
// proofs count into the brute-force counter the sign-in reads — after the
// threshold, the CORRECT password is refused at /api/auth/login until the
// lockout lifts, and the route itself throttles (429) like the login does.
func TestTheCurrentPasswordProofFeedsTheLoginLockout(t *testing.T) {
	f := newDerivationFixture(t)
	wrong := `{"current_password":"not-the-password","new_password":"new-password-forever-3"}`
	for i := 0; i < auth.LockoutThreshold; i++ {
		code, _, body := f.call(t, http.MethodPost, "/api/me/password", f.user.AccessToken, wrong)
		if code != http.StatusUnauthorized {
			t.Fatalf("wrong proof #%d answered %d (%s), want 401", i+1, code, body)
		}
	}
	// The counter the login reads is full: the correct password is refused.
	if _, err := f.svc.Login(context.Background(), fixtureEmail, fixturePassword, "ua", "ip"); err == nil {
		t.Fatal("five wrong current-password proofs did not lock the account: the correct password still signs in")
	}
	// The route throttles past its bucket, the way the login does.
	code, _, body := f.call(t, http.MethodPost, "/api/me/password", f.user.AccessToken, wrong)
	if code != http.StatusTooManyRequests && code != http.StatusUnauthorized {
		t.Fatalf("the proof route answered %d past its bucket (%s), want 429 (or the lockout's 401)", code, body)
	}
}

// failingPATStore revokes nothing and answers the failure, to put "sign out
// everywhere" in the state its docs name: the record landed (the caller's
// session is already refused) and a later step failed.
type failingPATStore struct {
	pat.Store
}

func (f failingPATStore) RevokeAllByUser(ctx context.Context, userID string, now time.Time) (int64, error) {
	return 0, errors.New("pat: the revocation pass did not complete")
}

// TestSignOutEverywhereNamesACleanupThatFailed: a cleanup step failing after
// the session revocation's record landed answers 503 signout_cleanup_failed
// — the caller's session is gone, and the body says to sign in again and
// repeat — and the record still stands: the presented session is dead.
func TestSignOutEverywhereNamesACleanupThatFailed(t *testing.T) {
	f := newDerivationFixture(t)
	f.s.pats = failingPATStore{Store: f.pats}
	code, _, body := f.call(t, http.MethodPost, "/api/me/sessions/revoke-all", f.user.AccessToken, "")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "signout_cleanup_failed") {
		t.Fatalf("a cleanup failing after the record answered %d %s, want 503 signout_cleanup_failed", code, body)
	}
	if _, err := f.svc.Refresh(context.Background(), f.user.RefreshToken, "ua", "ip"); err == nil {
		t.Fatal("the session revocation's record did not stand behind the failed cleanup")
	}
}
