package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// A personal access token exists for ONE promise: `iterion remote …`, a CI
// job or any script sends `Authorization: Bearer iap_…` to a cloud instance
// and is served as its owner, scoped to the team the token pins. That promise
// lives in the auth MIDDLEWARE (pkg/server/middleware.go), not in the PAT
// store — cut the `iap_` branch out of requireAuth and every store-level and
// handler-level PAT test still passes while every programmatic client on the
// planet gets a 401.
//
// So this test drives the token the way a client does: minted over real HTTP
// through the mux, then presented as a bearer to protected endpoints on a
// server built by the real New() wiring, asserting what the CALLER observes —
// the identity served back, the team boundary held, and the 401 that follows
// a revoke.

// patE2E is a real server (routes + middleware) with PATs enabled, plus the
// two teams and the user the assertions below need.
type patE2E struct {
	s   *Server
	hs  *httptest.Server
	jwt string
}

func newPATE2E(t *testing.T) *patE2E {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	s := New(Config{
		WorkDir:                 t.TempDir(),
		Bind:                    "127.0.0.1",
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
		PATs:                    pat.NewMemoryStore(),
	}, iterlog.New(iterlog.LevelError, nil))

	ctx := context.Background()
	seedTeam(t, s, "t1", "acme")
	seedTeam(t, s, "t2", "other")
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "u1", Email: "u1@x", Status: identity.UserStatusActive, DefaultTeamID: "t1",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "u2", Email: "u2@x", Status: identity.UserStatusActive, DefaultTeamID: "t2",
	}); err != nil {
		t.Fatalf("create other user: %v", err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "u1", TeamID: "t1", Role: identity.RoleMember,
	}); err != nil {
		t.Fatalf("membership: %v", err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "u2", TeamID: "t2", Role: identity.RoleMember,
	}); err != nil {
		t.Fatalf("other membership: %v", err)
	}

	// Guard the premise: with no PAT store (or no auth service) the routes
	// are never registered and every assertion below would be about a 404.
	if s.pats == nil {
		t.Fatal("server built without a PAT store")
	}

	hs := httptest.NewServer(s.handler)
	t.Cleanup(hs.Close)

	access, _, err := signer.IssueAccess(auth.Identity{
		UserID: "u1", Email: "u1@x", TeamID: "t1", Role: identity.RoleMember,
	})
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}
	return &patE2E{s: s, hs: hs, jwt: access}
}

// call sends a real HTTP request with the given bearer (empty = none).
func (p *patE2E) call(t *testing.T, method, path, bearer, body string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, p.hs.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestPATBearer_AuthenticatesProgrammaticClientsThroughTheRealServer(t *testing.T) {
	p := newPATE2E(t)

	// Mint through the front door, as `iterion remote login` does.
	status, body := p.call(t, http.MethodPost, "/api/me/tokens", p.jwt, `{"name":"ci","team_id":"t1"}`)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d body=%s", status, body)
	}
	var minted struct {
		PAT   pat.Token `json:"pat"`
		Token string    `json:"token"`
	}
	if err := json.Unmarshal(body, &minted); err != nil {
		t.Fatalf("decode mint: %v (%s)", err, body)
	}
	if minted.Token == "" || minted.PAT.ID == "" {
		t.Fatalf("mint returned no usable token: %s", body)
	}

	t.Run("the bearer is served as its owner, on its pinned team", func(t *testing.T) {
		status, body := p.call(t, http.MethodGet, "/api/auth/me", minted.Token, "")
		if status != http.StatusOK {
			t.Fatalf("GET /api/auth/me with PAT = %d body=%s", status, body)
		}
		var me authResponse
		if err := json.Unmarshal(body, &me); err != nil {
			t.Fatalf("decode me: %v (%s)", err, body)
		}
		if me.User.ID != "u1" || me.User.Email != "u1@x" {
			t.Fatalf("PAT served as %+v, want u1", me.User)
		}
		if me.ActiveTeam != "t1" {
			t.Fatalf("active team = %q, want t1 (the token's pin)", me.ActiveTeam)
		}
		if me.ActiveRole != string(identity.RoleMember) {
			t.Fatalf("active role = %q, want member (inherited from the membership)", me.ActiveRole)
		}
	})

	t.Run("a protected endpoint is reachable with the bearer alone", func(t *testing.T) {
		status, body := p.call(t, http.MethodGet, "/api/teams/t1/members", minted.Token, "")
		if status != http.StatusOK {
			t.Fatalf("GET team members with PAT = %d body=%s", status, body)
		}
		var got struct {
			Members []struct {
				UserID string `json:"user_id"`
				Role   string `json:"role"`
			} `json:"members"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode members: %v (%s)", err, body)
		}
		if len(got.Members) != 1 || got.Members[0].UserID != "u1" {
			t.Fatalf("members = %+v, want just u1", got.Members)
		}
	})

	t.Run("no credential is refused", func(t *testing.T) {
		if status, body := p.call(t, http.MethodGet, "/api/auth/me", "", ""); status != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET = %d body=%s, want 401", status, body)
		}
	})

	t.Run("an unknown iap_ bearer fails closed, never falling through to JWT parsing", func(t *testing.T) {
		status, body := p.call(t, http.MethodGet, "/api/auth/me", pat.TokenPrefix+"deadbeefdeadbeef", "")
		if status != http.StatusUnauthorized {
			t.Fatalf("bogus PAT = %d body=%s, want 401", status, body)
		}
	})

	t.Run("the token grants its team and no other", func(t *testing.T) {
		status, body := p.call(t, http.MethodGet, "/api/teams/t2/members", minted.Token, "")
		if status != http.StatusForbidden {
			t.Fatalf("cross-team read = %d body=%s, want 403", status, body)
		}
	})

	// Last: revocation is terminal for this token, so nothing may follow it.
	t.Run("revoking through the API kills programmatic access", func(t *testing.T) {
		// Revoked WITH the token itself — a write path proving the bearer
		// authenticates mutations, not just reads.
		if status, body := p.call(t, http.MethodDelete, "/api/me/tokens/"+minted.PAT.ID, minted.Token, ""); status != http.StatusNoContent {
			t.Fatalf("revoke = %d body=%s, want 204", status, body)
		}
		if status, body := p.call(t, http.MethodGet, "/api/auth/me", minted.Token, ""); status != http.StatusUnauthorized {
			t.Fatalf("revoked PAT still authenticates: %d body=%s", status, body)
		}
		// The JWT session is untouched by the revoke — only the token died.
		if status, body := p.call(t, http.MethodGet, "/api/auth/me", p.jwt, ""); status != http.StatusOK {
			t.Fatalf("session broken by PAT revoke: %d body=%s", status, body)
		}
	})
}

// A PAT owner who loses the membership the token resolves through must lose
// programmatic access at the NEXT call — the check is per-use, so a token
// minted while a member cannot outlive the membership.
func TestPATBearer_DiesWithTheMembershipItResolvesThrough(t *testing.T) {
	p := newPATE2E(t)

	status, body := p.call(t, http.MethodPost, "/api/me/tokens", p.jwt, `{"name":"ci"}`)
	if status != http.StatusCreated {
		t.Fatalf("mint status = %d body=%s", status, body)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &minted); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	if status, body := p.call(t, http.MethodGet, "/api/auth/me", minted.Token, ""); status != http.StatusOK {
		t.Fatalf("fresh PAT = %d body=%s, want 200", status, body)
	}

	if err := p.s.authStore().DeleteMembership(context.Background(), "u1", "t1"); err != nil {
		t.Fatalf("delete membership: %v", err)
	}
	if status, body := p.call(t, http.MethodGet, "/api/auth/me", minted.Token, ""); status != http.StatusUnauthorized {
		t.Fatalf("PAT survived the membership removal: %d body=%s", status, body)
	}
}
