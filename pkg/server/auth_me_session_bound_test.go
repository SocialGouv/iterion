package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// meBound is the slice of GET /api/auth/me's answer the session-bound
// witnesses read.
type meBound struct {
	SessionLineage string `json:"session_lineage"`
	ExpiresAt      string `json:"expires_at"`
}

// meFor drives GET /api/auth/me for the caller the identity names and
// decodes the answer.
func meFor(t *testing.T, s *Server, id auth.Identity) (int, meBound) {
	t.Helper()
	r := orgReq(auth.WithIdentity(context.Background(), id), "GET", "/api/auth/me", "", "")
	w := httptest.NewRecorder()
	s.handleMe(w, r)
	var body meBound
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/auth/me body %s: %v", w.Body.String(), err)
	}
	return w.Code, body
}

// TestAuthMe_ASessionCredentialNamesTheSessionsBound: /api/auth/me answers
// the bound of the refresh session the presented access token belongs to —
// the machine's expireStale reads it to drop a pending sign-out's record —
// and never the access token's own short-lived exp: the two live on
// different clocks (the refresh TTL vs the access TTL) and only one of them
// is the bound the session's sign-out record may carry.
func TestAuthMe_ASessionCredentialNamesTheSessionsBound(t *testing.T) {
	f := newAuthRouteFixture(t)
	sess := f.session(t, f.user.RefreshToken)
	id := auth.Identity{
		UserID: f.user.User.ID, OrgID: "o1", OrgRole: identity.OrgRoleAdmin, TeamID: "ta", Role: identity.RoleMember,
		Credential:     auth.CredentialAccessToken,
		SessionLineage: sess.LineageID,
	}
	code, body := meFor(t, f.s, id)
	if code != http.StatusOK {
		t.Fatalf("/api/auth/me answered %d: %s", code, http.StatusText(code))
	}
	if body.SessionLineage != sess.LineageID {
		t.Fatalf("session_lineage = %q, want %q", body.SessionLineage, sess.LineageID)
	}
	if body.ExpiresAt == "" {
		t.Fatal("/api/auth/me named no expires_at for a credential that names a session")
	}
	bound, err := time.Parse(time.RFC3339, body.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not an RFC3339 time: %v", body.ExpiresAt, err)
	}
	// The wire carries the row's own bound at RFC3339 precision — equal to
	// the row's expires_at as formatted, never a fresh "now + something".
	if body.ExpiresAt != sess.ExpiresAt.Format(time.RFC3339) {
		t.Fatalf("expires_at = %v, want the session's own bound %v", body.ExpiresAt, sess.ExpiresAt.Format(time.RFC3339))
	}
	if d := bound.Sub(sess.ExpiresAt); d < -time.Second || d > time.Second {
		t.Fatalf("expires_at = %v, want the session's own bound %v (±1s of wire precision)", bound, sess.ExpiresAt)
	}
	if access := f.user.AccessExpires; !access.Before(bound.Add(-time.Minute)) {
		t.Fatalf("expires_at %v is the access token's own exp (%v), not the session's bound", bound, access)
	}
}

// TestAuthMe_ACredentialThatNamesNoSessionOmitsTheBound: a personal access
// token names no refresh session, so its /api/auth/me answer carries no
// expires_at — an empty string, not a zero time, and never a foreign
// session's bound.
func TestAuthMe_ACredentialThatNamesNoSessionOmitsTheBound(t *testing.T) {
	f := newAuthRouteFixture(t)
	id := auth.Identity{
		UserID: f.user.User.ID, OrgID: "o1", OrgRole: identity.OrgRoleAdmin, TeamID: "ta", Role: identity.RoleMember,
		Credential: auth.CredentialPAT,
	}
	code, body := meFor(t, f.s, id)
	if code != http.StatusOK {
		t.Fatalf("/api/auth/me answered %d: %s", code, http.StatusText(code))
	}
	if body.ExpiresAt != "" {
		t.Fatalf("a credential that names no session answered expires_at %q, want it omitted", body.ExpiresAt)
	}
}

// TestAuthMe_ABoundTheStoreCannotReadIsAFailureNotAnAbsence: a session read
// that failed is answered as the failure it is (500, identity unavailable) —
// never rendered as an absent field, which would read as "the session is
// gone" for a session that may well be live.
func TestAuthMe_ABoundTheStoreCannotReadIsAFailureNotAnAbsence(t *testing.T) {
	f := newAuthRouteFixture(t)
	sess := f.session(t, f.user.RefreshToken)
	r := orgReq(auth.WithIdentity(context.Background(), auth.Identity{
		UserID: f.user.User.ID, OrgID: "o1", OrgRole: identity.OrgRoleAdmin, TeamID: "ta", Role: identity.RoleMember,
		Credential:     auth.CredentialAccessToken,
		SessionLineage: sess.LineageID,
	}), "GET", "/api/auth/me", "", "")
	f.sessions.failLineageReadArmed.Store(true)
	w := httptest.NewRecorder()
	f.s.handleMe(w, r)
	assertIdentityUnavailable(t, w, f.logs, "/api/auth/me with a session read that failed")
}
