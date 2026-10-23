package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// A revocation ends sessions; the routes below must neither extend an
// access token past one (the switches) nor answer one that failed as done
// (the sign-out, the password flows, the admin flows, reuse detection).

// switchOverHTTP posts a team switch with the bearer, through requireAuth,
// and answers the status and the access token and expiry it carries.
func switchOverHTTP(t *testing.T, srv *httptest.Server, teamID, bearer string) (int, string, time.Time) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/auth/me/team/"+teamID, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var v struct {
		AccessToken string `json:"access_token"`
		ExpiresAt   string `json:"expires_at"`
	}
	_ = json.Unmarshal(body, &v)
	var exp time.Time
	if v.ExpiresAt != "" {
		if exp, err = time.Parse(time.RFC3339, v.ExpiresAt); err != nil {
			t.Fatalf("switch answered expires_at %q: %v", v.ExpiresAt, err)
		}
	}
	return resp.StatusCode, v.AccessToken, exp
}

// accessExpiry reads the expiry an access token carries.
func accessExpiry(t *testing.T, f *authRouteFixture, token string) time.Time {
	t.Helper()
	id, err := f.s.signer.Verify(token)
	if err != nil {
		t.Fatalf("verify an access token: %v", err)
	}
	return id.ExpiresAt
}

// TestSwitch_ExtendsNothing chains switches the way a client renewing its
// access through them would: every token minted expires when the first one
// does, never later.
func TestSwitch_ExtendsNothing(t *testing.T) {
	f := newAuthRouteFixture(t)
	srv := httptest.NewServer(f.s.Handler())
	t.Cleanup(srv.Close)
	original := accessExpiry(t, f, f.user.AccessToken)
	// Past the next whole second: a switch minting a fresh lifetime would
	// expire after the presented token (expiries are whole seconds).
	time.Sleep(1100 * time.Millisecond)
	tok := f.user.AccessToken
	for i, team := range []string{"ta", f.user.ActiveTeamID, "ta"} {
		code, next, exp := switchOverHTTP(t, srv, team, tok)
		if code != http.StatusOK || next == "" {
			t.Fatalf("switch #%d answered %d", i+1, code)
		}
		if got := accessExpiry(t, f, next); !got.Equal(original) || !exp.Equal(original) {
			t.Fatalf("switch #%d minted a token expiring %v (answer says %v), want the presented token's %v", i+1, got, exp, original)
		}
		tok = next
	}
}

// TestSwitch_ARevokedSessionsAccessTokenMintsNothing: once the session an
// access token was minted for is revoked — signed out, signed out
// everywhere, its account disabled or reset by an administrator — a switch
// with that token answers 401 and sets nothing, so the token ends at its own
// expiry.
func TestSwitch_ARevokedSessionsAccessTokenMintsNothing(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"sign-out", "sign out everywhere", "admin disable", "admin password reset", "password change on another device"} {
		t.Run(how, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			srv := httptest.NewServer(f.s.Handler())
			t.Cleanup(srv.Close)
			root := auth.WithIdentity(ctx, auth.Identity{UserID: "root", IsSuperAdmin: true})
			switch how {
			case "sign-out":
				if w := logoutWithHeader(f.s, f.user.RefreshToken); w.Code != http.StatusNoContent {
					t.Fatalf("the sign-out answered %d", w.Code)
				}
			case "sign out everywhere":
				if err := f.svc.RevokeUserSessions(ctx, f.user.User.ID); err != nil {
					t.Fatalf("RevokeUserSessions: %v", err)
				}
			case "admin disable":
				r := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+f.user.User.ID, strings.NewReader(`{"status":"disabled"}`)).WithContext(root)
				r.SetPathValue("id", f.user.User.ID)
				w := httptest.NewRecorder()
				f.s.handleAdminUpdateUser(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("the disable answered %d: %s", w.Code, w.Body.String())
				}
			case "admin password reset":
				r := httptest.NewRequest(http.MethodPost, "/api/admin/users/"+f.user.User.ID+"/reset-password", nil).WithContext(root)
				r.SetPathValue("id", f.user.User.ID)
				w := httptest.NewRecorder()
				f.s.handleAdminResetUserPassword(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("the reset answered %d: %s", w.Code, w.Body.String())
				}
			case "password change on another device":
				if _, err := f.svc.ChangePassword(ctx, f.user.User.ID, fixturePassword, "a-brand-new-password-4", "other-device", "ip2"); err != nil {
					t.Fatalf("ChangePassword: %v", err)
				}
			}
			code, minted, _ := switchOverHTTP(t, srv, "ta", f.user.AccessToken)
			if code != http.StatusUnauthorized || minted != "" {
				t.Fatalf("a switch with the access token of a revoked session answered %d (a token minted: %t), want 401 and none", code, minted != "")
			}
		})
	}
}

// TestHandleLogout_AFailedSignOutKeepsTheCookies: a sign-out whose session
// read or revocation fails ends nothing, so it must not answer as if it did
// nor clear the cookies the client would present again. Once the store
// answers, the same token signs out.
func TestHandleLogout_AFailedSignOutKeepsTheCookies(t *testing.T) {
	for _, step := range []string{"the session read", "the lineage revocation"} {
		t.Run(step, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			if step == "the session read" {
				f.sessions.failGetArmed.Store(true)
			} else {
				f.sessions.failRevokeLineageArmed.Store(true)
			}
			w := logoutWithHeader(f.s, f.user.RefreshToken)
			assertIdentityUnavailable(t, w, f.logs, "a sign-out whose "+step+" fails")
			assertNoSetCookie(t, w, "a sign-out whose "+step+" fails")
			if f.session(t, f.user.RefreshToken).RevokedAt != nil {
				t.Fatal("the failed sign-out is reported a failure, yet it revoked the session")
			}
			again := logoutWithHeader(f.s, f.user.RefreshToken)
			if again.Code != http.StatusNoContent || !clearsRefreshCookie(again) {
				t.Fatalf("the sign-out retried once the store answers: %d (cookies cleared: %t), want 204 and cleared", again.Code, clearsRefreshCookie(again))
			}
			if r := refreshWithHeader(f.s, f.user.RefreshToken); r.Code != http.StatusUnauthorized {
				t.Fatalf("the token of the retried sign-out still refreshes: %d", r.Code)
			}
			if f.session(t, f.other.RefreshToken).RevokedAt != nil {
				t.Fatal("the sign-out revoked the account's other device")
			}
		})
	}
}

// TestHandleChangeMyPassword_ARevocationThatFailsChangesNothing: the
// self-service password change revokes every session before the new
// password lands. A revocation that fails answers the failure and changes
// nothing: the old password signs in, the other device keeps its session.
func TestHandleChangeMyPassword_ARevocationThatFailsChangesNothing(t *testing.T) {
	ctx := context.Background()
	f := newAuthRouteFixture(t)
	f.sessions.failRevokeAllArmed.Store(true)
	r := httptest.NewRequest(http.MethodPost, "/api/me/password", strings.NewReader(fmt.Sprintf(`{"current_password":%q,"new_password":"a-brand-new-password-4"}`, fixturePassword)))
	r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: f.user.User.ID}))
	w := httptest.NewRecorder()
	f.s.handleChangeMyPassword(w, r)
	assertIdentityUnavailable(t, w, f.logs, "a password change whose revocation fails")
	assertNoSetCookie(t, w, "a password change whose revocation fails")
	if _, err := f.svc.Login(ctx, fixtureEmail, fixturePassword, "ua", "ip"); err != nil {
		t.Fatalf("the old password no longer signs in after a change answered as a failure: %v", err)
	}
	if f.session(t, f.other.RefreshToken).RevokedAt != nil {
		t.Fatal("a change answered as a failure revoked the other device")
	}
}

// TestHandleAdminResetPassword_ARevocationThatFailsCommitsNothing: the admin
// reset revokes every session before the temporary password lands. A
// revocation that fails answers the failure, emits no temporary password,
// and leaves the account as it was.
func TestHandleAdminResetPassword_ARevocationThatFailsCommitsNothing(t *testing.T) {
	ctx := context.Background()
	f := newAuthRouteFixture(t)
	f.sessions.failRevokeAllArmed.Store(true)
	r := httptest.NewRequest(http.MethodPost, "/api/admin/users/"+f.user.User.ID+"/reset-password", nil)
	r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "root", IsSuperAdmin: true}))
	r.SetPathValue("id", f.user.User.ID)
	w := httptest.NewRecorder()
	f.s.handleAdminResetUserPassword(w, r)
	assertIdentityUnavailable(t, w, f.logs, "an admin reset whose revocation fails")
	if strings.Contains(w.Body.String(), "temp_password") {
		t.Fatalf("an admin reset answered as a failure emitted a temporary password: %s", w.Body.String())
	}
	u, err := f.s.authStore().GetUser(ctx, f.user.User.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Status != identity.UserStatusActive {
		t.Fatalf("an admin reset answered as a failure moved the account to %q", u.Status)
	}
	if _, err := f.svc.Login(ctx, fixtureEmail, fixturePassword, "ua", "ip"); err != nil {
		t.Fatalf("the password changed under an admin reset answered as a failure: %v", err)
	}
	if f.session(t, f.other.RefreshToken).RevokedAt != nil {
		t.Fatal("an admin reset answered as a failure revoked a session")
	}
}

// TestHandleAdminDisable_ARevocationThatFailsIsAFailure: the admin disable
// lands the status first — a sign-in committed before it dies with it — then
// revokes every session. A revocation that fails is answered as the failure
// it is: the account is disabled (nothing of it signs in, refreshes or
// switches), its sessions are not revoked yet, and the console's retry
// revokes them.
func TestHandleAdminDisable_ARevocationThatFailsIsAFailure(t *testing.T) {
	ctx := context.Background()
	f := newAuthRouteFixture(t)
	disable := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+f.user.User.ID, strings.NewReader(`{"status":"disabled"}`))
		r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "root", IsSuperAdmin: true}))
		r.SetPathValue("id", f.user.User.ID)
		w := httptest.NewRecorder()
		f.s.handleAdminUpdateUser(w, r)
		return w
	}
	f.sessions.failRevokeAllArmed.Store(true)
	assertIdentityUnavailable(t, disable(), f.logs, "an admin disable whose revocation fails")
	u, err := f.s.authStore().GetUser(ctx, f.user.User.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Status != identity.UserStatusDisabled {
		t.Fatalf("the disable answered as a failed revocation left the account %q, want disabled", u.Status)
	}
	if r := refreshWithHeader(f.s, f.other.RefreshToken); r.Code != http.StatusUnauthorized {
		t.Fatalf("a session of the disabled account refreshed: %d", r.Code)
	}
	if w := disable(); w.Code != http.StatusOK {
		t.Fatalf("the retried disable answered %d: %s", w.Code, w.Body.String())
	}
	if f.session(t, f.user.RefreshToken).RevokedAt == nil {
		t.Fatal("the retried disable left a session of the account unrevoked")
	}
}

// TestHandleRefresh_AReuseWhoseRevocationFailsKeepsTheToken: reuse
// detection whose lineage revocation fails answers the failure and keeps the
// cookies; the same token presented once the store answers is reuse again,
// revokes the lineage and is refused.
func TestHandleRefresh_AReuseWhoseRevocationFailsKeepsTheToken(t *testing.T) {
	f, clock := windowFixture(t)
	rotated := refreshWithHeader(f.s, f.user.RefreshToken)
	successor := cookieValue(rotated, refreshCookieName)
	if rotated.Code != http.StatusOK || successor == "" {
		t.Fatalf("the rotation answered %d", rotated.Code)
	}
	clock.Advance(auth.DefaultRefreshReuseGrace + 10*time.Second)
	f.sessions.failRevokeLineageArmed.Store(true)
	w := refreshWithHeader(f.s, f.user.RefreshToken)
	assertIdentityUnavailable(t, w, f.logs, "a reuse whose revocation fails")
	assertNoSetCookie(t, w, "a reuse whose revocation fails")
	again := refreshWithHeader(f.s, f.user.RefreshToken)
	if again.Code != http.StatusUnauthorized {
		t.Fatalf("the reused token presented once the store answers: %d, want 401", again.Code)
	}
	if f.session(t, successor).RevokedAt == nil {
		t.Fatal("the retried reuse detection left the lineage's successor live")
	}
	if f.session(t, f.other.RefreshToken).RevokedAt != nil {
		t.Fatal("the reuse revoked the account's other device")
	}
}

// TestHandleCreateTeam_ARefusalRestingOnAFailedReadIsAFailure: an org admin
// whose org-membership read fails is not refused as a non-admin — the "no"
// rests on a read that failed — but answered the identity 500, logged.
func TestHandleCreateTeam_ARefusalRestingOnAFailedReadIsAFailure(t *testing.T) {
	f := newAuthRouteFixture(t)
	create := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/teams", strings.NewReader(`{"org_id":"o1","name":"Created by an org admin"}`))
		r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, OrgID: "o1"}))
		w := httptest.NewRecorder()
		f.s.handleCreateTeam(w, r)
		return w
	}
	f.ids.fail("GetOrgMembership", 0)
	assertIdentityUnavailable(t, create(), f.logs, "a team creation whose org-membership read fails")
	f.ids.disarm()
	if w := create(); w.Code != http.StatusOK {
		t.Fatalf("the org admin's team creation once the store answers: %d %s, want 200", w.Code, w.Body.String())
	}
}
