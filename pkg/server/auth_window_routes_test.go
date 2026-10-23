package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// logoutWithHeader signs out the way an anonymous HTTP client does: the
// refresh token in the X-Iterion-Refresh header, no cookie, no session.
func logoutWithHeader(s *Server, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/auth/logout", nil)
	r.Header.Set("X-Iterion-Refresh", token)
	w := httptest.NewRecorder()
	s.handleLogout(w, r)
	return w
}

// windowFixture is newAuthRouteFixture with the session store's clock — the
// one the reuse window is measured on — in the test's hands.
func windowFixture(t *testing.T) (*authRouteFixture, *manualClock) {
	t.Helper()
	f := newAuthRouteFixture(t)
	clock := newManualClock(time.Now().UTC())
	f.sessions.SetClock(clock.Now)
	return f, clock
}

// TestHandleLogout_ARetiredTokenStaysReuse drives the bypass through the
// public routes: an anonymous client holding a refresh token a rotation
// retired signs out with it, then presents it past its window. The refresh
// must refuse it as reuse, the session the rotation wrote must be dead, not
// delivered — the sign-out ended the token's lineage — and the account's
// other device, another lineage, keeps its session.
func TestHandleLogout_ARetiredTokenStaysReuse(t *testing.T) {
	f, clock := windowFixture(t)
	rotated := refreshWithHeader(f.s, f.user.RefreshToken)
	successor := cookieValue(rotated, refreshCookieName)
	if rotated.Code != http.StatusOK || successor == "" {
		t.Fatalf("the rotation answered %d", rotated.Code)
	}
	clock.Advance(auth.DefaultRefreshReuseGrace + 10*time.Second)
	if w := logoutWithHeader(f.s, f.user.RefreshToken); w.Code != http.StatusNoContent {
		t.Fatalf("the sign-out answered %d", w.Code)
	}
	if f.session(t, successor).RevokedAt == nil {
		t.Fatal("the sign-out with the retired token left live the session its rotation wrote")
	}
	again := refreshWithHeader(f.s, f.user.RefreshToken)
	if again.Code != http.StatusUnauthorized {
		t.Fatalf("the retired token presented after a sign-out answered %d (delivered the successor: %t), want 401",
			again.Code, cookieValue(again, refreshCookieName) == successor)
	}
	if f.session(t, successor).RevokedAt == nil {
		t.Fatal("the retired token presented after a sign-out found its lineage's successor live")
	}
	if f.session(t, f.other.RefreshToken).RevokedAt != nil {
		t.Fatal("a sign-out and a reuse of one lineage revoked the account's other device")
	}
}

// TestHandleRefresh_AReuseRevokesTheLineageNotTheAccount: a rotated token
// replayed past its window over HTTP is refused 401 and its lineage — the
// successor its rotation wrote — revoked, while the account's other device
// keeps a session that still refreshes.
func TestHandleRefresh_AReuseRevokesTheLineageNotTheAccount(t *testing.T) {
	f, clock := windowFixture(t)
	rotated := refreshWithHeader(f.s, f.user.RefreshToken)
	successor := cookieValue(rotated, refreshCookieName)
	if rotated.Code != http.StatusOK || successor == "" {
		t.Fatalf("the rotation answered %d", rotated.Code)
	}
	clock.Advance(auth.DefaultRefreshReuseGrace + 10*time.Second)
	if again := refreshWithHeader(f.s, f.user.RefreshToken); again.Code != http.StatusUnauthorized {
		t.Fatalf("the rotated token replayed past its window answered %d, want 401", again.Code)
	}
	if f.session(t, successor).RevokedAt == nil {
		t.Fatal("the reuse left the lineage's successor live")
	}
	if f.session(t, f.other.RefreshToken).RevokedAt != nil {
		t.Fatal("the reuse of one lineage revoked the account's other device")
	}
	if w := refreshWithHeader(f.s, f.other.RefreshToken); w.Code != http.StatusOK {
		t.Fatalf("the other device's refresh after the reuse answered %d", w.Code)
	}
}

// TestHandleRefresh_ANeverWrittenSuccessorPastTheHorizonIsReuse: the
// completion of a successor whose write failed is bounded — a retired token
// presented past the completion horizon was abandoned by its holder, and is
// refused as reuse of its lineage; the other device is untouched.
func TestHandleRefresh_ANeverWrittenSuccessorPastTheHorizonIsReuse(t *testing.T) {
	f, clock := windowFixture(t)
	f.sessions.failCreateArmed.Store(true)
	if w := refreshWithHeader(f.s, f.user.RefreshToken); w.Code != http.StatusInternalServerError {
		t.Fatalf("the refresh whose successor write failed answered %d", w.Code)
	}
	clock.Advance(6 * time.Minute)
	again := refreshWithHeader(f.s, f.user.RefreshToken)
	if again.Code != http.StatusUnauthorized || cookieValue(again, refreshCookieName) != "" {
		t.Fatalf("a token abandoned past the completion horizon answered %d with a refresh cookie %t, want 401 and none",
			again.Code, cookieValue(again, refreshCookieName) != "")
	}
	if f.session(t, f.other.RefreshToken).RevokedAt != nil {
		t.Fatal("the reuse of an abandoned token revoked the account's other device")
	}
}

// TestHandleRevokeAllSessions_TakesTheSuccessorNotYetWritten drives "sign
// out everywhere" after a rotation whose successor's write failed: the
// retired token presented afterwards, inside its window, is refused, and the
// account holds no live session.
func TestHandleRevokeAllSessions_TakesTheSuccessorNotYetWritten(t *testing.T) {
	f, clock := windowFixture(t)
	f.sessions.failCreateArmed.Store(true)
	if w := refreshWithHeader(f.s, f.user.RefreshToken); w.Code != http.StatusInternalServerError {
		t.Fatalf("the refresh whose successor write failed answered %d", w.Code)
	}
	clock.Advance(5 * time.Second)
	r := httptest.NewRequest("POST", "/api/me/sessions/revoke-all", nil)
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, TeamID: f.user.ActiveTeamID}))
	w := httptest.NewRecorder()
	f.s.handleRevokeAllSessions(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("sign out everywhere answered %d: %s", w.Code, w.Body.String())
	}
	clock.Advance(5 * time.Second)
	again := refreshWithHeader(f.s, f.user.RefreshToken)
	if again.Code != http.StatusUnauthorized || cookieValue(again, refreshCookieName) != "" {
		t.Fatalf("after sign out everywhere, the retired token answered %d with a refresh cookie %t, want 401 and none",
			again.Code, cookieValue(again, refreshCookieName) != "")
	}
}

// TestHandleRefresh_ANeverWrittenSuccessorIsWrittenPastTheWindow drives a
// store failure on the successor's write answered by a client that comes
// back after the window but within the completion horizon (a store outage
// longer than the window, a browser's next request): the presentation
// completes the rotation — 200, a refresh cookie — and revokes nothing.
func TestHandleRefresh_ANeverWrittenSuccessorIsWrittenPastTheWindow(t *testing.T) {
	f, clock := windowFixture(t)
	f.sessions.failCreateArmed.Store(true)
	if w := refreshWithHeader(f.s, f.user.RefreshToken); w.Code != http.StatusInternalServerError {
		t.Fatalf("the refresh whose successor write failed answered %d", w.Code)
	}
	clock.Advance(auth.DefaultRefreshReuseGrace + 15*time.Second)
	again := refreshWithHeader(f.s, f.user.RefreshToken)
	if again.Code != http.StatusOK || cookieValue(again, refreshCookieName) == "" {
		t.Fatalf("the retry past the window, its successor never written, answered %d: %s", again.Code, again.Body.String())
	}
	if f.session(t, f.other.RefreshToken).RevokedAt != nil {
		t.Fatal("completing a successor that was never written revoked the account's other sessions")
	}
}

// refreshCookie returns the refresh cookie a response sets.
func refreshCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if (c.Name == refreshCookieName || c.Name == hostCookiePrefix+refreshCookieName) && c.MaxAge >= 0 && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no refresh cookie in the answer (%d %s)", w.Code, w.Body.String())
	return nil
}

// TestHandleRefresh_TheRefreshCookieDoesNotDateTheRotation: a refresh token
// presented again inside its window is answered with the session an earlier
// rotation wrote. Its refresh cookie must not say so: a rotation, its
// redelivery seconds later and a fresh rotation at that later instant carry
// the same Expires — the hour the session ends in, never earlier than it.
func TestHandleRefresh_TheRefreshCookieDoesNotDateTheRotation(t *testing.T) {
	f, clock := windowFixture(t)
	start := time.Now().UTC().Truncate(time.Hour).Add(20 * time.Minute)
	clock.Advance(start.Sub(clock.Now()))
	rotation := refreshWithHeader(f.s, f.user.RefreshToken)
	first := refreshCookie(t, rotation)
	clock.Advance(2 * time.Second)
	redelivery := refreshCookie(t, refreshWithHeader(f.s, f.user.RefreshToken))
	fresh := refreshCookie(t, refreshWithHeader(f.s, f.other.RefreshToken))
	if redelivery.Value != first.Value {
		t.Fatal("the presentation inside the window was not answered with the rotation's successor")
	}
	if !redelivery.Expires.Equal(first.Expires) || !fresh.Expires.Equal(first.Expires) {
		t.Fatalf("the cookies date the rotation: rotation %v, its redelivery 2 s later %v, a fresh rotation then %v",
			first.Expires, redelivery.Expires, fresh.Expires)
	}
	if first.Expires.Truncate(time.Hour) != first.Expires {
		t.Fatalf("the refresh cookie expires at %v, not on the hour", first.Expires)
	}
	if session := f.session(t, first.Value); first.Expires.Before(session.ExpiresAt.Truncate(time.Second)) {
		t.Fatalf("the refresh cookie expires at %v, before its session (%v)", first.Expires, session.ExpiresAt)
	}
}

// clearsRefreshCookie reports whether a response expires the refresh cookie,
// under either spelling.
func clearsRefreshCookie(w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if (c.Name == refreshCookieName || c.Name == hostCookiePrefix+refreshCookieName) && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

// TestHandleRefresh_TheRefusalsNameTheirCause pins the refresh's 401s as the
// REST page lists them: the body names the cause, and every refusal of a
// presented credential clears the cookies — while a request that presented
// nothing gets its own body and keeps whatever cookies it has.
func TestHandleRefresh_TheRefusalsNameTheirCause(t *testing.T) {
	ctx := context.Background()
	f, _ := windowFixture(t)
	seed := func(t *testing.T, userID string, expires time.Time) string {
		t.Helper()
		raw, _, err := auth.GenerateRandomToken(48)
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		id := "seeded-" + raw[:8]
		if err := f.sessions.MemorySessionStore.CreateSession(ctx, auth.Session{
			ID: id, UserID: userID, TokenHash: auth.HashRefreshToken(raw), LineageID: id,
			IssuedAt: expires.Add(-time.Hour), ExpiresAt: expires,
		}); err != nil {
			t.Fatalf("seed a session: %v", err)
		}
		return raw
	}

	none := httptest.NewRecorder()
	f.s.handleRefresh(none, httptest.NewRequest("POST", "/api/auth/refresh", nil))
	if none.Code != http.StatusUnauthorized || !strings.Contains(none.Body.String(), `"no refresh token"`) {
		t.Fatalf("a refresh presenting nothing answered %d %s, want 401 no refresh token", none.Code, none.Body.String())
	}
	if len(none.Result().Cookies()) != 0 {
		t.Fatalf("a refresh presenting nothing touched the cookies: %v", none.Result().Cookies())
	}

	revokedTok := seed(t, f.user.User.ID, time.Now().Add(time.Hour))
	if w := logoutWithHeader(f.s, revokedTok); w.Code != http.StatusNoContent {
		t.Fatalf("the sign-out answered %d", w.Code)
	}
	disabledUser, err := f.svc.Register(ctx, "disabled@example.org", fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("register the account to disable: %v", err)
	}
	disabled := identity.UserStatusDisabled
	if _, err := f.ids.PatchUser(ctx, disabledUser.User.ID, identity.UserPatch{Status: &disabled}); err != nil {
		t.Fatalf("disable the account: %v", err)
	}
	pendingUser, err := f.svc.Register(ctx, "pending@example.org", fixturePassword, "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("register the account to set pending: %v", err)
	}
	pending := identity.UserStatusPendingPasswordChange
	if _, err := f.ids.PatchUser(ctx, pendingUser.User.ID, identity.UserPatch{Status: &pending}); err != nil {
		t.Fatalf("set the account pending a password change: %v", err)
	}
	for _, tc := range []struct {
		name, token, body string
	}{
		{"a signed-out session", revokedTok, `"auth: session revoked"`},
		{"an expired session", seed(t, f.user.User.ID, time.Now().Add(-time.Minute)), `"auth: session expired"`},
		{"an unknown token", "a-token-no-session-was-ever-issued-for", `"auth: session not found"`},
		{"a session whose account is gone", seed(t, "no-such-account", time.Now().Add(time.Hour)), `"auth: session not found: its account no longer exists"`},
		{"a disabled account", disabledUser.RefreshToken, `"auth: account disabled"`},
		{"an account pending a password change", pendingUser.RefreshToken, `"auth: session revoked: the account must change its password"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := refreshWithHeader(f.s, tc.token)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("answered %d %s, want 401 %s", w.Code, w.Body.String(), tc.body)
			}
			if !clearsRefreshCookie(w) {
				t.Fatal("a refused credential's cookies were not cleared")
			}
		})
	}
}
