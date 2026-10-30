package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestAuthRoutes_StoreFailuresAnswerTheGenericBody drives the identity
// routes that answered a store failure with the driver's own text — the
// public invitation lookup among them, reachable with any token string. Each
// must answer 500 with the generic body and log the cause.
func TestAuthRoutes_StoreFailuresAnswerTheGenericBody(t *testing.T) {
	authed := func(f *authRouteFixture, r *http.Request) *http.Request {
		return r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, TeamID: "ta", Role: identity.RoleAdmin, OrgID: "o1", OrgRole: identity.OrgRoleAdmin}))
	}
	for _, tc := range []struct {
		name string
		arm  func(f *authRouteFixture)
		call func(f *authRouteFixture, w *httptest.ResponseRecorder)
	}{
		{"GET /api/auth/invitations/lookup", func(f *authRouteFixture) { f.ids.fail("GetInvitationByTokenHash", 0) },
			func(f *authRouteFixture, w *httptest.ResponseRecorder) {
				f.s.handleInvitationLookup(w, httptest.NewRequest("GET", "/api/auth/invitations/lookup?token=any-string", nil))
			}},
		{"POST /api/auth/invitations/accept", func(f *authRouteFixture) { f.ids.fail("GetInvitationByTokenHash", 0) },
			func(f *authRouteFixture, w *httptest.ResponseRecorder) {
				f.s.handleInvitationAcceptForLoggedIn(w, authed(f, httptest.NewRequest("POST", "/api/auth/invitations/accept", strings.NewReader(`{"token":"any-string"}`))))
			}},
		{"POST /api/me/sessions/revoke-all", func(f *authRouteFixture) { f.sessions.failRevokeAllArmed.Store(true) },
			func(f *authRouteFixture, w *httptest.ResponseRecorder) {
				f.s.handleRevokeAllSessions(w, authed(f, httptest.NewRequest("POST", "/api/me/sessions/revoke-all", nil)))
			}},
		{"POST /api/teams", func(f *authRouteFixture) { f.ids.fail("GetOrg", 0) },
			func(f *authRouteFixture, w *httptest.ResponseRecorder) {
				f.s.handleCreateTeam(w, authed(f, httptest.NewRequest("POST", "/api/teams", strings.NewReader(`{"name":"New team","org_id":"o1"}`))))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			tc.arm(f)
			w := httptest.NewRecorder()
			tc.call(f, w)
			assertIdentityUnavailable(t, w, f.logs, tc.name+" with a failed store")
		})
	}
}

// TestHandleDesktopExchange_AStoreFailureIsNotAnInvalidTicket: the desktop
// reads a 401 from the exchange as "sign in again". A ticket store that
// fails must answer what it is — 500, the cause logged — so the desktop
// retries the ticket, which is still valid; an unknown ticket stays 401.
func TestHandleDesktopExchange_AStoreFailureIsNotAnInvalidTicket(t *testing.T) {
	f := newAuthRouteFixture(t)
	ticket := mintDesktopTicket(t, f, f.user.User.ID)
	exchange := func(ticket string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		f.s.handleDesktopExchange(w, httptest.NewRequest("POST", "/api/auth/desktop/exchange", strings.NewReader(fmt.Sprintf(`{"ticket":%q}`, ticket))))
		return w
	}
	f.tickets.failRedeemArmed.Store(true)
	assertIdentityUnavailable(t, exchange(ticket), f.logs, "an exchange whose ticket store failed")
	if w := exchange(ticket); w.Code != http.StatusOK {
		t.Fatalf("the exchange retried with the same ticket answered %d: %s", w.Code, w.Body.String())
	}
	if w := exchange("no-such-ticket"); w.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown ticket answered %d, want 401", w.Code)
	}
}

// TestHandlePasswordResetConfirm_AStoreFailureLeavesTheLinkUsable: a reset
// whose record read, or whose consumption, fails is answered as a failure —
// 500, the cause logged — and the link still resets the password once the
// store answers. An unknown link stays 401.
func TestHandlePasswordResetConfirm_AStoreFailureLeavesTheLinkUsable(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(f *authRouteFixture)
	}{
		{"the record's read", func(f *authRouteFixture) { f.resets.failGetArmed.Store(true) }},
		{"its consumption", func(f *authRouteFixture) { f.resets.failConsumeArmed.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			now := time.Now().UTC()
			if err := f.resets.Create(context.Background(), auth.PasswordReset{
				ID: "reset-1", UserID: f.user.User.ID, TokenHash: auth.HashRefreshToken("iar_reset-token-1"),
				CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			}); err != nil {
				t.Fatalf("seed reset: %v", err)
			}
			confirm := func(token string) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				f.s.handlePasswordResetConfirm(w, httptest.NewRequest("POST", "/api/auth/password/reset/confirm",
					strings.NewReader(fmt.Sprintf(`{"token":%q,"password":"a-new-password-3"}`, token))))
				return w
			}
			tc.arm(f)
			assertIdentityUnavailable(t, confirm("iar_reset-token-1"), f.logs, "a reset confirm whose "+tc.name+" failed")
			if w := confirm("iar_reset-token-1"); w.Code != http.StatusOK {
				t.Fatalf("the link retried once the store answers: %d %s", w.Code, w.Body.String())
			}
			if w := confirm("iar_unknown"); w.Code != http.StatusUnauthorized {
				t.Fatalf("an unknown link answered %d, want 401", w.Code)
			}
		})
	}
}

// TestHandleChangePassword_ALookupFailureIsAFailure: the forced password
// change answers a failed account lookup as the failure it is — as the
// login does, since it fails whether or not the address exists — and logs
// it; an unknown address stays the opaque 401.
func TestHandleChangePassword_ALookupFailureIsAFailure(t *testing.T) {
	f := newAuthRouteFixture(t)
	change := func(email string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		f.s.handleChangePassword(w, httptest.NewRequest("POST", "/api/auth/password/change",
			strings.NewReader(fmt.Sprintf(`{"email":%q,"current_password":"temporary-password-1","new_password":"a-new-password-2"}`, email))))
		return w
	}
	f.ids.fail("GetUserByEmail", 1)
	assertIdentityUnavailable(t, change(fixtureEmail), f.logs, "a forced password change whose lookup failed")
	if w := change("nobody@example.com"); w.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown address answered %d, want 401", w.Code)
	}
}

// TestHandleInvitationAccept_AFailedOrgReadKeepsTheOwner drives an org owner
// accepting a member invitation to a team of their own org through the
// route, while the read of their org membership fails: 500, the owner still
// owner, the invitation unspent — and the retry accepts it.
func TestHandleInvitationAccept_AFailedOrgReadKeepsTheOwner(t *testing.T) {
	f := newAuthRouteFixture(t)
	ctx := context.Background()
	if err := f.s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{UserID: f.user.User.ID, OrgID: "o1", Role: identity.OrgRoleOwner, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("make the user owner: %v", err)
	}
	if _, err := f.s.authStore().CreateTeam(ctx, identity.Team{ID: "tb", OrgID: "o1", Name: "TB", Slug: "tb", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("seed team tb: %v", err)
	}
	token, _, err := f.svc.CreateInvitation(testMintVerdictContext(ctx), "tb", fixtureEmail, identity.RoleMember, "someone-else")
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	accept := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/auth/invitations/accept", strings.NewReader(fmt.Sprintf(`{"token":%q}`, token)))
		r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: f.user.User.ID}))
		w := httptest.NewRecorder()
		f.s.handleInvitationAcceptForLoggedIn(w, r)
		return w
	}
	role := func() identity.OrgRole {
		om, err := f.s.authStore().GetOrgMembership(ctx, f.user.User.ID, "o1")
		if err != nil {
			t.Fatalf("read the org membership: %v", err)
		}
		return om.Role
	}
	f.ids.fail("GetOrgMembership", 0)
	assertIdentityUnavailable(t, accept(), f.logs, "an accept whose org-membership read failed")
	f.ids.disarm()
	if got := role(); got != identity.OrgRoleOwner {
		t.Fatalf("the failed accept left the owner %q", got)
	}
	if w := accept(); w.Code != http.StatusOK {
		t.Fatalf("the accept retried once the store answers: %d %s", w.Code, w.Body.String())
	}
	if got := role(); got != identity.OrgRoleOwner {
		t.Fatalf("accepting a member invitation left the owner %q", got)
	}
}
