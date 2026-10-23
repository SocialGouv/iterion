package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
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
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pat"
)

// storeGatePlus is storeGate with the two identity calls the shared gate
// does not intercept: GetMembership — the read the PAT middleware and the
// team-authorization helpers make — and PatchUser, the write the GitHub-orgs
// callback owns.
type storeGatePlus struct{ *storeGate }

func newStoreGatePlus(st identity.Store) *storeGatePlus {
	return &storeGatePlus{storeGate: newStoreGate(st)}
}

func (g *storeGatePlus) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if err := g.hit("GetMembership"); err != nil {
		return identity.Membership{}, err
	}
	return g.Store.GetMembership(ctx, userID, teamID)
}

func (g *storeGatePlus) PatchUser(ctx context.Context, id string, p identity.UserPatch) (identity.User, error) {
	if err := g.hit("PatchUser"); err != nil {
		return identity.User{}, err
	}
	return g.Store.PatchUser(ctx, id, p)
}

// patGate is a PAT store whose token-row lookup fails, the way a saturated
// pool fails the first read of an authenticated request.
type patGate struct {
	pat.Store
	failLookup bool
}

func (g *patGate) GetByTokenHash(ctx context.Context, hash string) (pat.Token, error) {
	if g.failLookup {
		return pat.Token{}, fmt.Errorf("pat: GetByTokenHash: %w", errClusterUnreachable)
	}
	return g.Store.GetByTokenHash(ctx, hash)
}

// patRouteFixture is a real HTTP server (the full mux, so a request goes
// through requireAuth) on gated stores, holding one active account with a
// team in an org and one PAT minted over the wire, pinned to that team.
type patRouteFixture struct {
	s     *Server
	ids   *storeGatePlus
	pats  *patGate
	logs  *syncBuffer
	srv   *httptest.Server
	token string // the PAT plaintext, as a client holds it
}

func newPATRouteFixture(t *testing.T) *patRouteFixture {
	t.Helper()
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	f := &patRouteFixture{
		ids:  newStoreGatePlus(identity.NewMemoryStore()),
		pats: &patGate{Store: pat.NewMemoryStore()},
		logs: &syncBuffer{},
	}
	svc, err := auth.NewService(auth.Config{
		Store: f.ids, Sessions: auth.NewMemorySessionStore(), Signer: signer,
		SignupMode: auth.SignupOpen, RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	f.s = New(Config{
		WorkDir: t.TempDir(), Bind: "127.0.0.1", SkipProjectRegistration: true,
		AuthService: svc, AuthSigner: signer, PATs: f.pats,
	}, iterlog.New(iterlog.LevelError, f.logs))
	seedOrg(t, f.s, "o1", "org-one")
	if _, err := f.s.authStore().CreateTeam(ctx, identity.Team{
		ID: "t1", OrgID: "o1", Name: "T1", Slug: "t1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := f.s.authStore().CreateUser(ctx, identity.User{
		ID: "u1", Email: "u1@example.com", Status: identity.UserStatusActive, DefaultTeamID: "t1",
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := f.s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "u1", TeamID: "t1", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	f.srv = httptest.NewServer(f.s.Handler())
	t.Cleanup(f.srv.Close)
	// The token is minted over HTTP, by the route a client calls: no
	// hand-made row proves the plaintext a client presents is the one the
	// store holds.
	access, _, err := signer.IssueAccess(auth.Identity{UserID: "u1", Email: "u1@example.com", TeamID: "t1", Role: identity.RoleMember})
	if err != nil {
		t.Fatalf("access token: %v", err)
	}
	code, body := f.call(t, "POST", "/api/me/tokens", access, `{"name":"cli","team_id":"t1"}`)
	if code != http.StatusCreated {
		t.Fatalf("mint a PAT over HTTP: %d %s", code, body)
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &minted); err != nil || minted.Token == "" {
		t.Fatalf("the mint answered no plaintext: %s (err %v)", body, err)
	}
	f.token = minted.Token
	return f
}

func (f *patRouteFixture) call(t *testing.T, method, path, bearer, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, f.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(out))
}

// TestRequireAuth_APATIdentityReadThatFailsIsAFailure: the PAT middleware
// resolves four rows — the token, its owner, the owner's membership of the
// pinned team, and that team — and a read it could not COMPLETE is a failure
// of the server, not a verdict on the credential. Each of the four answers
// 500 with the generic body and its cause in the server log: a 401 there
// tells a client "this token is dead", and the one client that calls
// /api/auth/me right after minting a token throws it away on that answer
// (cmd/iterion runRemoteLogin), while the raw driver text names the identity
// cluster's hosts to anyone holding any token.
func TestRequireAuth_APATIdentityReadThatFailsIsAFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(f *patRouteFixture)
	}{
		{"the token row", func(f *patRouteFixture) { f.pats.failLookup = true }},
		{"the token's owner", func(f *patRouteFixture) { f.ids.fail("GetUser", 0) }},
		{"the owner's membership of the pinned team", func(f *patRouteFixture) { f.ids.fail("GetMembership", 0) }},
		{"the pinned team's org scope", func(f *patRouteFixture) { f.ids.fail("GetTeam", 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPATRouteFixture(t)
			tc.arm(f)
			code, body := f.call(t, "GET", "/api/auth/me", f.token, "")
			if code != http.StatusInternalServerError {
				t.Fatalf("a failed read of %s answered %d %s, want 500", tc.name, code, body)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(body), &got); err != nil || got["error"] != identityUnavailable || len(got) != 1 {
				t.Fatalf("a failed read of %s answered the body %s, want {\"error\":%q}", tc.name, body, identityUnavailable)
			}
			if strings.Contains(body, internalHost) {
				t.Fatalf("a failed read of %s leaked the store's internal host to the client: %s", tc.name, body)
			}
			if !strings.Contains(f.logs.String(), internalHost) {
				t.Fatalf("a failed read of %s is not in the server log: %q", tc.name, f.logs.String())
			}
		})
	}
}

// TestRequireAuth_APATTheServerRefusesKeepsIts401: the refusals the same
// middleware owes — a token no row matches, a revoked one, an owner no longer
// active, a membership the owner no longer holds, a pinned team that is gone —
// stay 401 with their own message, and write no ERROR line (which an operator
// reads as a store outage).
func TestRequireAuth_APATTheServerRefusesKeepsIts401(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// arm prepares the refusal and returns the bearer to present.
		arm func(t *testing.T, f *patRouteFixture) string
		msg string
	}{
		{"a token no row matches", func(t *testing.T, f *patRouteFixture) string {
			return pat.TokenPrefix + strings.Repeat("a", 43)
		}, "token invalid"},
		{"a revoked token", func(t *testing.T, f *patRouteFixture) string {
			toks, err := f.pats.ListByUser(ctx, "u1")
			if err != nil || len(toks) != 1 {
				t.Fatalf("list the minted token: %v (%d rows)", err, len(toks))
			}
			if err := f.pats.Revoke(ctx, toks[0].ID, time.Now().UTC()); err != nil {
				t.Fatalf("revoke: %v", err)
			}
			return f.token
		}, "token revoked or expired"},
		{"an owner no longer active", func(t *testing.T, f *patRouteFixture) string {
			disabled := identity.UserStatusDisabled
			if _, err := f.s.authStore().PatchUser(ctx, "u1", identity.UserPatch{Status: &disabled}); err != nil {
				t.Fatalf("disable the owner: %v", err)
			}
			return f.token
		}, "token owner unavailable"},
		{"a membership the owner no longer holds", func(t *testing.T, f *patRouteFixture) string {
			if err := f.s.authStore().DeleteMembership(ctx, "u1", "t1"); err != nil {
				t.Fatalf("remove the membership: %v", err)
			}
			return f.token
		}, "token team unavailable"},
		{"a pinned team that no longer exists", func(t *testing.T, f *patRouteFixture) string {
			if err := f.s.authStore().DeleteTeam(ctx, "t1"); err != nil {
				t.Fatalf("delete the team: %v", err)
			}
			return f.token
		}, "token org scope unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPATRouteFixture(t)
			bearer := tc.arm(t, f)
			code, body := f.call(t, "GET", "/api/auth/me", bearer, "")
			if code != http.StatusUnauthorized || !strings.Contains(body, tc.msg) {
				t.Fatalf("%s answered %d %s, want 401 naming %q", tc.name, code, body, tc.msg)
			}
			if logged := f.logs.String(); logged != "" {
				t.Fatalf("%s wrote to the server log: %q", tc.name, logged)
			}
		})
	}
}
