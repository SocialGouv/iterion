package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/desktopsso"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestBuildOrgTree_BulkLookup covers the batched org resolution: org
// rows keep the org-membership order (JoinedAt asc), a membership whose
// org is gone is skipped (the old per-row GetOrg ErrNotFound path), org
// admins see ungranted teams with an implied admin role, and plain org
// members see only their granted teams.
func TestBuildOrgTree_BulkLookup(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedOrg(t, s, "o1", "org-one")
	seedOrg(t, s, "o2", "org-two")
	for i, tm := range []identity.Team{
		{ID: "ta", OrgID: "o1", Name: "TA", Slug: "ta"},
		{ID: "tb", OrgID: "o1", Name: "TB", Slug: "tb"},
		{ID: "tc", OrgID: "o2", Name: "TC", Slug: "tc"},
	} {
		tm.CreatedAt = now.Add(time.Duration(i) * time.Second)
		if _, err := s.authStore().CreateTeam(ctx, tm); err != nil {
			t.Fatalf("seed team %s: %v", tm.ID, err)
		}
	}
	// Org-membership order is JoinedAt asc: o1 (admin), a dangling org,
	// o2 (plain member).
	for i, om := range []identity.OrgMembership{
		{UserID: "u1", OrgID: "o1", Role: identity.OrgRoleAdmin},
		{UserID: "u1", OrgID: "o-gone", Role: identity.OrgRoleMember},
		{UserID: "u1", OrgID: "o2", Role: identity.OrgRoleMember},
	} {
		om.JoinedAt = now.Add(time.Duration(i) * time.Second)
		if err := s.authStore().UpsertOrgMembership(ctx, om); err != nil {
			t.Fatalf("seed org membership %s: %v", om.OrgID, err)
		}
	}
	// Team grant: u1 is a member of ta only. tb is visible to them via
	// the org-admin fallback; tc stays hidden (plain member of o2).
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "u1", TeamID: "ta", Role: identity.RoleMember, JoinedAt: now,
	}); err != nil {
		t.Fatalf("seed team membership: %v", err)
	}

	orgs, err := s.buildOrgTree(ctx, "u1")
	if err != nil {
		t.Fatalf("buildOrgTree: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("got %d orgs, want 2 (dangling org membership skipped): %+v", len(orgs), orgs)
	}
	if orgs[0].OrgID != "o1" || orgs[1].OrgID != "o2" {
		t.Fatalf("org-membership order lost: %+v", orgs)
	}
	if orgs[0].OrgRole != "admin" || orgs[0].OrgSlug != "org-one" {
		t.Fatalf("o1 view mismatch: %+v", orgs[0])
	}
	if len(orgs[0].Teams) != 2 {
		t.Fatalf("org admin must see both o1 teams, got %+v", orgs[0].Teams)
	}
	if orgs[0].Teams[0].TeamID != "ta" || orgs[0].Teams[0].Role != "member" {
		t.Fatalf("granted team keeps its granted role: %+v", orgs[0].Teams[0])
	}
	if orgs[0].Teams[1].TeamID != "tb" || orgs[0].Teams[1].Role != "admin" {
		t.Fatalf("ungranted team gets the org-admin implied role: %+v", orgs[0].Teams[1])
	}
	if len(orgs[1].Teams) != 0 {
		t.Fatalf("plain org member must not see ungranted teams: %+v", orgs[1].Teams)
	}
}

// internalHost is the kind of internal name a real store error carries (a
// Mongo topology error names the cluster's members). No answer to a client
// may contain it.
const internalHost = "iterion-mongo-0.iterion-mongo-headless.iterion-prod.svc.cluster.local"

// errClusterUnreachable is shaped like the error a Mongo driver returns when
// it cannot select a server.
var errClusterUnreachable = fmt.Errorf("server selection error: context deadline exceeded, current topology: { Type: Unknown, Servers: [{ Addr: %s:27017 }] }", internalHost)

// storeGate is an identity store whose reads fail on chosen calls, the way a
// saturated pool or a store restart fails ONE read of a request — often not
// its first (the second org's teams, the response writer's user read).
// failOn maps a method to the 1-based call numbers that fail; 0 fails every
// call. Each failure wraps err: a driver-shaped error naming an internal
// host by default, a cancellation-shaped one where a test chooses it.
type storeGate struct {
	identity.Store

	mu     sync.Mutex
	err    error
	failOn map[string]map[int]bool
	calls  map[string]int
}

func newStoreGate(st identity.Store) *storeGate {
	return &storeGate{Store: st, err: errClusterUnreachable, failOn: map[string]map[int]bool{}, calls: map[string]int{}}
}

// fail arms the given calls of method (0 = every call), counted from now.
func (g *storeGate) fail(method string, calls ...int) *storeGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls[method] = 0
	if g.failOn[method] == nil {
		g.failOn[method] = map[int]bool{}
	}
	for _, c := range calls {
		g.failOn[method][c] = true
	}
	return g
}

// with sets the error every failure wraps.
func (g *storeGate) with(err error) *storeGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
	return g
}

func (g *storeGate) disarm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failOn = map[string]map[int]bool{}
}

func (g *storeGate) hit(method string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls[method]++
	if f := g.failOn[method]; f != nil && (f[0] || f[g.calls[method]]) {
		return fmt.Errorf("identity: %s: %w", method, g.err)
	}
	return nil
}

func (g *storeGate) ListOrgMembershipsByUser(ctx context.Context, userID string) ([]identity.OrgMembership, error) {
	if err := g.hit("ListOrgMembershipsByUser"); err != nil {
		return nil, err
	}
	return g.Store.ListOrgMembershipsByUser(ctx, userID)
}

func (g *storeGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	if err := g.hit("ListMembershipsByUser"); err != nil {
		return nil, err
	}
	return g.Store.ListMembershipsByUser(ctx, userID)
}

func (g *storeGate) GetOrgsByIDs(ctx context.Context, ids []string) (map[string]identity.Org, error) {
	if err := g.hit("GetOrgsByIDs"); err != nil {
		return nil, err
	}
	return g.Store.GetOrgsByIDs(ctx, ids)
}

func (g *storeGate) ListTeamsByOrg(ctx context.Context, orgID string) ([]identity.Team, error) {
	if err := g.hit("ListTeamsByOrg"); err != nil {
		return nil, err
	}
	return g.Store.ListTeamsByOrg(ctx, orgID)
}

func (g *storeGate) GetTeamsByIDs(ctx context.Context, ids []string) (map[string]identity.Team, error) {
	if err := g.hit("GetTeamsByIDs"); err != nil {
		return nil, err
	}
	return g.Store.GetTeamsByIDs(ctx, ids)
}

func (g *storeGate) GetUsersByIDs(ctx context.Context, ids []string) (map[string]identity.User, error) {
	if err := g.hit("GetUsersByIDs"); err != nil {
		return nil, err
	}
	return g.Store.GetUsersByIDs(ctx, ids)
}

func (g *storeGate) GetUser(ctx context.Context, userID string) (identity.User, error) {
	if err := g.hit("GetUser"); err != nil {
		return identity.User{}, err
	}
	return g.Store.GetUser(ctx, userID)
}

func (g *storeGate) GetUserByEmail(ctx context.Context, email string) (identity.User, error) {
	if err := g.hit("GetUserByEmail"); err != nil {
		return identity.User{}, err
	}
	return g.Store.GetUserByEmail(ctx, email)
}

func (g *storeGate) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	if err := g.hit("GetTeam"); err != nil {
		return identity.Team{}, err
	}
	return g.Store.GetTeam(ctx, id)
}

func (g *storeGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if err := g.hit("GetOrgMembership"); err != nil {
		return identity.OrgMembership{}, err
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

func (g *storeGate) GetOrg(ctx context.Context, id string) (identity.Org, error) {
	if err := g.hit("GetOrg"); err != nil {
		return identity.Org{}, err
	}
	return g.Store.GetOrg(ctx, id)
}

func (g *storeGate) GetInvitationByTokenHash(ctx context.Context, hash string) (identity.Invitation, error) {
	if err := g.hit("GetInvitationByTokenHash"); err != nil {
		return identity.Invitation{}, err
	}
	return g.Store.GetInvitationByTokenHash(ctx, hash)
}

func (g *storeGate) ListMembershipsByTeam(ctx context.Context, teamID string) ([]identity.Membership, error) {
	if err := g.hit("ListMembershipsByTeam"); err != nil {
		return nil, err
	}
	return g.Store.ListMembershipsByTeam(ctx, teamID)
}

// sessionGate is the in-memory session store with a per-operation latency
// (what makes two refreshes of one token overlap), and one-shot failures of
// the read a refresh starts with, of the successor's write, and of the
// revocations of an account and of a lineage.
type sessionGate struct {
	*auth.MemorySessionStore
	latency                atomic.Int64
	failGetArmed           atomic.Bool
	failCreateArmed        atomic.Bool
	failRevokeAllArmed     atomic.Bool
	failRevokeLineageArmed atomic.Bool
	failLineageReadArmed   atomic.Bool
}

func (s *sessionGate) sleep() {
	if d := time.Duration(s.latency.Load()); d > 0 {
		time.Sleep(d)
	}
}

func (s *sessionGate) GetSessionByTokenHash(ctx context.Context, h string) (auth.Session, error) {
	s.sleep()
	if s.failGetArmed.CompareAndSwap(true, false) {
		return auth.Session{}, fmt.Errorf("auth: get session: %w", errClusterUnreachable)
	}
	return s.MemorySessionStore.GetSessionByTokenHash(ctx, h)
}

func (s *sessionGate) LiveLineageSession(ctx context.Context, userID, lineageID string) (auth.Session, bool, error) {
	s.sleep()
	if s.failLineageReadArmed.CompareAndSwap(true, false) {
		return auth.Session{}, false, fmt.Errorf("auth: read the live session of lineage %s: %w", lineageID, errClusterUnreachable)
	}
	return s.MemorySessionStore.LiveLineageSession(ctx, userID, lineageID)
}

func (s *sessionGate) CreateSession(ctx context.Context, x auth.Session) error {
	s.sleep()
	if s.failCreateArmed.CompareAndSwap(true, false) {
		return fmt.Errorf("auth: insert session: %w", errClusterUnreachable)
	}
	return s.MemorySessionStore.CreateSession(ctx, x)
}

func (s *sessionGate) RotateSession(ctx context.Context, id string, rot auth.Rotation) (time.Time, bool, error) {
	s.sleep()
	return s.MemorySessionStore.RotateSession(ctx, id, rot)
}

func (s *sessionGate) RecordAccountRevocation(ctx context.Context, userID string, at time.Time) (int64, error) {
	s.sleep()
	if s.failRevokeAllArmed.CompareAndSwap(true, false) {
		return 0, fmt.Errorf("auth: revoke user sessions: %w", errClusterUnreachable)
	}
	return s.MemorySessionStore.RecordAccountRevocation(ctx, userID, at)
}

func (s *sessionGate) RecordLineageRevocation(ctx context.Context, userID, lineageID string, at time.Time) error {
	s.sleep()
	if s.failRevokeLineageArmed.CompareAndSwap(true, false) {
		return fmt.Errorf("auth: revoke lineage %s: record the revocation: %w", lineageID, errClusterUnreachable)
	}
	return s.MemorySessionStore.RecordLineageRevocation(ctx, userID, lineageID, at)
}

// resetGate is the in-memory reset store with one-shot failures of the
// record's read and of its consumption.
type resetGate struct {
	*auth.MemoryPasswordResetStore
	failGetArmed     atomic.Bool
	failConsumeArmed atomic.Bool
}

func (s *resetGate) GetByTokenHash(ctx context.Context, hash string) (auth.PasswordReset, error) {
	if s.failGetArmed.CompareAndSwap(true, false) {
		return auth.PasswordReset{}, fmt.Errorf("auth: get reset: %w", errClusterUnreachable)
	}
	return s.MemoryPasswordResetStore.GetByTokenHash(ctx, hash)
}

func (s *resetGate) Consume(ctx context.Context, id string, at time.Time) (bool, error) {
	if s.failConsumeArmed.CompareAndSwap(true, false) {
		return false, fmt.Errorf("auth: consume reset: %w", errClusterUnreachable)
	}
	return s.MemoryPasswordResetStore.Consume(ctx, id, at)
}

// ticketGate is the in-memory desktop ticket store with a one-shot failure
// of the redemption.
type ticketGate struct {
	*desktopsso.MemoryStore
	failRedeemArmed atomic.Bool
}

func (s *ticketGate) Redeem(ctx context.Context, ticket string) (auth.LoginResult, error) {
	if s.failRedeemArmed.CompareAndSwap(true, false) {
		return auth.LoginResult{}, fmt.Errorf("desktopsso: redeem: %w", errClusterUnreachable)
	}
	return s.MemoryStore.Redeem(ctx, ticket)
}

// authRouteFixture is a server on gated stores with a captured log, and one
// account: a personal org and team (registration), an admin membership of
// org o1 with team ta, and a live session on another device.
type authRouteFixture struct {
	s        *Server
	svc      *auth.Service
	ids      *storeGate
	sessions *sessionGate
	resets   *resetGate
	tickets  *ticketGate
	logs     *syncBuffer
	user     auth.LoginResult // the registration's session
	other    auth.LoginResult // another device's session
}

const (
	fixtureEmail    = "u1@example.com"
	fixturePassword = "password-forever-9"
)

func newAuthRouteFixture(t *testing.T) *authRouteFixture {
	t.Helper()
	return newAuthRouteFixtureWith(t, nil)
}

// newAuthRouteFixtureWith is newAuthRouteFixture whose server Config the
// caller completes before the server is built (routes such as the personal
// access tokens' are only mounted when their store is configured).
func newAuthRouteFixtureWith(t *testing.T, configure func(*Config)) *authRouteFixture {
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
	f := &authRouteFixture{
		ids:      newStoreGate(identity.NewMemoryStore()),
		sessions: &sessionGate{MemorySessionStore: auth.NewMemorySessionStore()},
		resets:   &resetGate{MemoryPasswordResetStore: auth.NewMemoryPasswordResetStore()},
		tickets:  &ticketGate{MemoryStore: desktopsso.NewMemoryStore(time.Minute)},
		logs:     &syncBuffer{},
	}
	svc, err := auth.NewService(auth.Config{
		Store: f.ids, Sessions: f.sessions, Signer: signer,
		SignupMode: auth.SignupOpen, RefreshTTL: time.Hour, Resets: f.resets,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	f.svc = svc
	cfg := Config{AuthService: svc, AuthSigner: signer, DesktopTickets: f.tickets}
	if configure != nil {
		configure(&cfg)
	}
	f.s = New(cfg, iterlog.New(iterlog.LevelError, f.logs))
	if f.user, err = svc.Register(ctx, fixtureEmail, fixturePassword, "", "", "ua", "ip"); err != nil {
		t.Fatalf("register: %v", err)
	}
	seedOrgTreeFor(t, f.s, f.user.User.ID)
	if f.other, err = svc.Login(ctx, fixtureEmail, fixturePassword, "other-device", "ip2"); err != nil {
		t.Fatalf("login (other device): %v", err)
	}
	return f
}

// session reads the stored row of a raw refresh token.
func (f *authRouteFixture) session(t *testing.T, raw string) auth.Session {
	t.Helper()
	// The rows a revocation's detached pass writes, once it has.
	f.s.authSvc.WaitRevocationPasses()
	s, err := f.sessions.MemorySessionStore.GetSessionByTokenHash(context.Background(), auth.HashRefreshToken(raw))
	if err != nil {
		t.Fatalf("read the session of a token: %v", err)
	}
	return s
}

// seedOrgTreeFor gives userID an org→team tree beside whatever it holds:
// org o1, team ta inside it, an admin org membership and a member grant.
func seedOrgTreeFor(t *testing.T, s *Server, userID string) {
	t.Helper()
	seedOrg(t, s, "o1", "org-one")
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: "ta", OrgID: "o1", Name: "TA", Slug: "ta", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team ta: %v", err)
	}
	grantOrgTree(t, s, userID)
}

// grantOrgTree grants userID the admin membership of org o1 and a member
// grant on its team ta (both seeded by seedOrgTreeFor).
func grantOrgTree(t *testing.T, s *Server, userID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: userID, OrgID: "o1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed org membership: %v", err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: userID, TeamID: "ta", Role: identity.RoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team membership: %v", err)
	}
}

func seedActiveUser(t *testing.T, s *Server, id, email string) {
	t.Helper()
	if _, err := s.authStore().CreateUser(context.Background(), identity.User{
		ID: id, Email: email, Status: identity.UserStatusActive,
	}); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// assertIdentityUnavailable fails the test unless the recorder holds the
// answer to a store failure: exactly 500, the generic body, and none of the
// failure's internal text — which the server log carries instead.
func assertIdentityUnavailable(t *testing.T, w *httptest.ResponseRecorder, logs *syncBuffer, what string) {
	t.Helper()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("%s answered %d, want 500: %s", what, w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != identityUnavailable || len(body) != 1 {
		t.Fatalf("%s answered the body %s, want {\"error\":%q}", what, w.Body.String(), identityUnavailable)
	}
	if strings.Contains(w.Body.String(), internalHost) {
		t.Fatalf("%s leaked the store's internal host to the client: %s", what, w.Body.String())
	}
	if logs != nil && !strings.Contains(logs.String(), internalHost) {
		t.Fatalf("%s: the failure's cause is not in the server log: %q", what, logs.String())
	}
}

// assertNoSetCookie fails the test when the response writes any cookie:
// an error answer neither delivers a session nor clears the one the
// client holds.
func assertNoSetCookie(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if got := w.Result().Header.Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("%s wrote cookies under an error answer: %q", what, got)
	}
}

// cookieValue returns the value the response sets for the named cookie
// (either spelling), "" when it sets none.
func cookieValue(w *httptest.ResponseRecorder, base string) string {
	for _, c := range w.Result().Cookies() {
		if (c.Name == base || c.Name == hostCookiePrefix+base) && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

// TestHandleMe_StoreFailureIsNotAnEmptyTree drives the real /api/auth/me
// handler — the one route that builds the org tree — with each of its reads
// failing, on the call a real blip would hit. A read that failed is not a
// fact about the user: the answer is the failure, never a 200 whose empty
// or partial tree claims "you belong to no organization".
func TestHandleMe_StoreFailureIsNotAnEmptyTree(t *testing.T) {
	for _, tc := range []struct {
		name, read string
		call       int
		err        error
	}{
		{"org memberships read fails", "ListOrgMembershipsByUser", 1, errClusterUnreachable},
		{"team memberships read fails", "ListMembershipsByUser", 1, errClusterUnreachable},
		{"bulk org read fails", "GetOrgsByIDs", 1, errClusterUnreachable},
		{"first org's teams read fails", "ListTeamsByOrg", 1, errClusterUnreachable},
		{"second org's teams read fails", "ListTeamsByOrg", 2, errClusterUnreachable},
		{"user read is cancelled", "GetUser", 1, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			f.ids.with(tc.err).fail(tc.read, tc.call)
			r := orgReq(auth.WithIdentity(context.Background(), auth.Identity{
				UserID: f.user.User.ID, OrgID: "o1", OrgRole: identity.OrgRoleAdmin, TeamID: "ta", Role: identity.RoleMember,
			}), "GET", "/api/auth/me", "", "")
			w := httptest.NewRecorder()
			f.s.handleMe(w, r)
			if tc.err == context.Canceled {
				assertIdentityUnavailable(t, w, nil, "/api/auth/me with "+tc.name)
				return
			}
			assertIdentityUnavailable(t, w, f.logs, "/api/auth/me with "+tc.name)
		})
	}
}

// TestHandleMe_LegitimateEmptyTreeStaysOK pins the other half of the
// distinction: a user the store SUCCESSFULLY reports as belonging to no
// organization still answers 200 with an empty tree.
func TestHandleMe_LegitimateEmptyTreeStaysOK(t *testing.T) {
	s := newOrgTestServer(t)
	seedActiveUser(t, s, "u2", "u2@example.com")
	r := orgReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: "u2"}), "GET", "/api/auth/me", "", "")
	w := httptest.NewRecorder()
	s.handleMe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a memberless account must stay 200, got %d: %s", w.Code, w.Body.String())
	}
	var res AuthMeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode /api/auth/me body: %v", err)
	}
	if res.Orgs == nil || len(res.Orgs) != 0 {
		t.Fatalf("expected an empty org tree, got %#v", res.Orgs)
	}
}

// credentialRoute is one route that mints a credential, driven through its
// real handler against the fixture's account.
type credentialRoute struct {
	name string
	// prepare readies the request; it runs before the gate is armed.
	prepare func(t *testing.T, f *authRouteFixture) *http.Request
	handle  func(s *Server) http.HandlerFunc
	// refresh: the route mints a refresh session (every route but a switch).
	refresh bool
	// pickReadsTeams: the operation itself lists an org's teams (an org
	// switch picks its landing team that way), so that read is not the
	// tree's to fail.
	pickReadsTeams bool
}

func credentialRoutes() []credentialRoute {
	jsonReq := func(path, body string) *http.Request {
		return httptest.NewRequest("POST", path, strings.NewReader(body))
	}
	authed := func(f *authRouteFixture, r *http.Request) *http.Request {
		return r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, TeamID: f.user.ActiveTeamID, Credential: auth.CredentialAccessToken}))
	}
	return []credentialRoute{
		{name: "login", refresh: true, handle: func(s *Server) http.HandlerFunc { return s.handleLogin },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				return jsonReq("/api/auth/login", fmt.Sprintf(`{"email":%q,"password":%q}`, fixtureEmail, fixturePassword))
			}},
		{name: "register", refresh: true, handle: func(s *Server) http.HandlerFunc { return s.handleRegister },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				return jsonReq("/api/auth/register", `{"email":"newcomer@example.com","password":"password-forever-9"}`)
			}},
		{name: "refresh", refresh: true, handle: func(s *Server) http.HandlerFunc { return s.handleRefresh },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				r := httptest.NewRequest("POST", "/api/auth/refresh", nil)
				r.Header.Set("X-Iterion-Refresh", f.user.RefreshToken)
				return r
			}},
		{name: "pending password change", refresh: true, handle: func(s *Server) http.HandlerFunc { return s.handleChangePassword },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				hash, err := auth.HashPassword("temporary-password-1")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.authStore().CreateUser(context.Background(), identity.User{
					ID: "pending", Email: "pending@example.com", PasswordHash: hash, Status: identity.UserStatusPendingPasswordChange,
				}); err != nil {
					t.Fatalf("seed pending user: %v", err)
				}
				grantOrgTree(t, f.s, "pending")
				return jsonReq("/api/auth/password/change", `{"email":"pending@example.com","current_password":"temporary-password-1","new_password":"a-new-password-2"}`)
			}},
		{name: "password reset confirm", refresh: true, handle: func(s *Server) http.HandlerFunc { return s.handlePasswordResetConfirm },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				now := time.Now().UTC()
				if err := f.resets.Create(context.Background(), auth.PasswordReset{
					ID: "reset-1", UserID: f.user.User.ID, TokenHash: auth.HashRefreshToken("iar_reset-token-1"),
					CreatedAt: now, ExpiresAt: now.Add(time.Hour),
				}); err != nil {
					t.Fatalf("seed reset: %v", err)
				}
				return jsonReq("/api/auth/password/reset/confirm", `{"token":"iar_reset-token-1","password":"a-new-password-3"}`)
			}},
		{name: "change my password", refresh: true, handle: func(s *Server) http.HandlerFunc { return s.handleChangeMyPassword },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				return authed(f, jsonReq("/api/me/password", fmt.Sprintf(`{"current_password":%q,"new_password":"another-password-7"}`, fixturePassword)))
			}},
		{name: "desktop exchange", refresh: true, handle: func(s *Server) http.HandlerFunc { return s.handleDesktopExchange },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				res, err := f.s.authSvc.Login(context.Background(), fixtureEmail, fixturePassword, "desktop", "ip")
				if err != nil {
					t.Fatalf("login for the ticket: %v", err)
				}
				ticket, err := f.s.desktopTickets.Mint(context.Background(), res)
				if err != nil {
					t.Fatalf("mint ticket: %v", err)
				}
				return jsonReq("/api/auth/desktop/exchange", fmt.Sprintf(`{"ticket":%q}`, ticket))
			}},
		{name: "switch team", handle: func(s *Server) http.HandlerFunc { return s.handleSwitchTeam },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				r := authed(f, httptest.NewRequest("POST", "/api/auth/me/team/ta", nil))
				r.SetPathValue("team_id", "ta")
				return r
			}},
		{name: "switch org", pickReadsTeams: true, handle: func(s *Server) http.HandlerFunc { return s.handleSwitchOrg },
			prepare: func(t *testing.T, f *authRouteFixture) *http.Request {
				r := authed(f, httptest.NewRequest("POST", "/api/auth/me/org/o1", nil))
				r.SetPathValue("org_id", "o1")
				return r
			}},
	}
}

// TestCredentialRoutes_DeliverTheSessionWithoutTheTree drives every route
// that mints a credential with every read only the org tree makes failing.
// None of them builds the tree: each delivers its session — 200, its
// cookies, no "orgs" in the body — so a failed tree read can no longer
// answer an error for a credential the route has already minted (a
// registration retried into "email already taken", a reset link burnt by a
// blip, a switch persisted under an error answer).
func TestCredentialRoutes_DeliverTheSessionWithoutTheTree(t *testing.T) {
	for _, rt := range credentialRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			r := rt.prepare(t, f)
			f.ids.fail("GetOrgsByIDs", 0)
			if !rt.pickReadsTeams {
				f.ids.fail("ListTeamsByOrg", 0)
			}
			w := httptest.NewRecorder()
			rt.handle(f.s)(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("%s with the tree's reads failing answered %d: %s", rt.name, w.Code, w.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s body: %v", rt.name, err)
			}
			if _, has := body["orgs"]; has {
				t.Fatalf("%s answered an org tree: only GET /api/auth/me builds it: %s", rt.name, w.Body.String())
			}
			if _, has := body["user"]; !has {
				t.Fatalf("%s answered no user: %s", rt.name, w.Body.String())
			}
			if cookieValue(w, authCookieName) == "" {
				t.Fatalf("%s delivered no access cookie", rt.name)
			}
			if rt.refresh && cookieValue(w, refreshCookieName) == "" {
				t.Fatalf("%s delivered no refresh cookie", rt.name)
			}
		})
	}
}

// TestHandleRefresh_FailureKeepsTheCookies: a refresh whose store read fails
// before anything rotated answers the failure and leaves the client's
// cookies alone — the token it presented is still its to present, and does
// refresh once the store answers.
func TestHandleRefresh_FailureKeepsTheCookies(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(f *authRouteFixture)
	}{
		{"the session read fails", func(f *authRouteFixture) { f.sessions.failGetArmed.Store(true) }},
		{"the account read fails", func(f *authRouteFixture) { f.ids.fail("GetUser", 1) }},
		{"a login read fails", func(f *authRouteFixture) { f.ids.fail("ListOrgMembershipsByUser", 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			tc.arm(f)
			w := refreshWithHeader(f.s, f.user.RefreshToken)
			assertIdentityUnavailable(t, w, f.logs, "a refresh whose "+tc.name)
			assertNoSetCookie(t, w, "a refresh whose "+tc.name)
			f.ids.disarm()
			if again := refreshWithHeader(f.s, f.user.RefreshToken); again.Code != http.StatusOK {
				t.Fatalf("the token presented to the failed refresh no longer refreshes (%d: %s)", again.Code, again.Body.String())
			}
		})
	}
}

// TestHandleRefresh_DeadCredentialIs401AndClearsTheCookies: a token the store
// has no row for (Mongo's TTL index deletes an expired session) is a dead
// credential — 401, which the desktop reads as "sign in again" — and the
// cookies carrying it are cleared. A 500 there would be retried forever.
func TestHandleRefresh_DeadCredentialIs401AndClearsTheCookies(t *testing.T) {
	f := newAuthRouteFixture(t)
	w := refreshWithHeader(f.s, "a-token-no-session-was-ever-issued-for")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown refresh token answered %d, want 401: %s", w.Code, w.Body.String())
	}
	cleared := 0
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared++
		}
	}
	if cleared != 4 {
		t.Fatalf("a dead refresh token cleared %d cookies, want both spellings of both cookies (4)", cleared)
	}
}

// TestHandleRefresh_ConcurrentRefreshesShareTheSuccessor drives two refreshes
// of one token through the real HTTP stack at once — two tabs whose access
// token expired together, a desktop's concurrent 401s. Both are answered
// with the same rotated token, and the account's other sessions survive.
func TestHandleRefresh_ConcurrentRefreshesShareTheSuccessor(t *testing.T) {
	for trial := 0; trial < 10; trial++ {
		f := newAuthRouteFixture(t)
		srv := httptest.NewServer(f.s.Handler())
		f.sessions.latency.Store(int64(time.Millisecond))
		var (
			wg       sync.WaitGroup
			statuses [2]int
			tokens   [2]string
		)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				time.Sleep(time.Duration(i) * 500 * time.Microsecond)
				req, _ := http.NewRequest("POST", srv.URL+"/api/auth/refresh", nil)
				req.Header.Set("X-Iterion-Refresh", f.user.RefreshToken)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Errorf("refresh %d: %v", i, err)
					return
				}
				defer resp.Body.Close()
				statuses[i] = resp.StatusCode
				for _, c := range resp.Cookies() {
					if strings.HasSuffix(c.Name, refreshCookieName) && c.MaxAge >= 0 {
						tokens[i] = c.Value
					}
				}
			}(i)
		}
		wg.Wait()
		f.sessions.latency.Store(0)
		srv.Close()
		if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
			t.Fatalf("trial %d: concurrent refreshes of one token answered %v", trial, statuses)
		}
		if tokens[0] == "" || tokens[0] != tokens[1] {
			t.Fatalf("trial %d: concurrent refreshes delivered different refresh tokens", trial)
		}
		if f.session(t, f.other.RefreshToken).RevokedAt != nil {
			t.Fatalf("trial %d: concurrent refreshes revoked the account's other sessions", trial)
		}
	}
}

// TestHandleRefresh_LostSuccessorIsRecoverable drives a store blip landing
// after the rotation: the successor's write fails, the client is answered
// 500 with its cookies untouched — and the token it still holds, presented
// again (the desktop retries every 15 s), completes the refresh instead of
// revoking every session of the account.
func TestHandleRefresh_LostSuccessorIsRecoverable(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.sessions.failCreateArmed.Store(true)
	w := refreshWithHeader(f.s, f.user.RefreshToken)
	assertIdentityUnavailable(t, w, f.logs, "a refresh whose successor write failed")
	assertNoSetCookie(t, w, "a refresh whose successor write failed")
	again := refreshWithHeader(f.s, f.user.RefreshToken)
	if again.Code != http.StatusOK {
		t.Fatalf("the retry with the token the failed refresh left answered %d: %s", again.Code, again.Body.String())
	}
	if f.session(t, f.other.RefreshToken).RevokedAt != nil {
		t.Fatal("the retry after a lost successor revoked the account's other sessions")
	}
}

func refreshWithHeader(s *Server, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/auth/refresh", nil)
	r.Header.Set("X-Iterion-Refresh", token)
	w := httptest.NewRecorder()
	s.handleRefresh(w, r)
	return w
}

// TestHandleLogin_StoreFailureIsGeneric: a login whose store read fails is
// answered 500 with the generic body — the driver's text, which names the
// cluster's hosts, goes to the log — and delivers no session.
func TestHandleLogin_StoreFailureIsGeneric(t *testing.T) {
	for _, read := range []string{"GetUserByEmail", "ListMembershipsByUser", "GetTeam"} {
		t.Run(read, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			f.ids.fail(read, 1)
			r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, fixtureEmail, fixturePassword)))
			w := httptest.NewRecorder()
			f.s.handleLogin(w, r)
			assertIdentityUnavailable(t, w, f.logs, "a login whose "+read+" fails")
			assertNoSetCookie(t, w, "a login whose "+read+" fails")
		})
	}
}

// TestHandleSwitchTeam_FailedReadMovesNothing: a team switch whose org
// resolution cannot read answers the failure, sets no cookie, and persists
// no default — so the next refresh mints the session on the team the page
// still shows, not on the one the failed switch named.
func TestHandleSwitchTeam_FailedReadMovesNothing(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.ids.fail("GetOrgMembership", 1)
	r := httptest.NewRequest("POST", "/api/auth/me/team/ta", nil)
	r.SetPathValue("team_id", "ta")
	r = r.WithContext(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, Credential: auth.CredentialAccessToken}))
	w := httptest.NewRecorder()
	f.s.handleSwitchTeam(w, r)
	assertIdentityUnavailable(t, w, f.logs, "a team switch whose org read fails")
	assertNoSetCookie(t, w, "a team switch whose org read fails")
	f.ids.disarm()
	res, err := f.s.authSvc.Refresh(context.Background(), f.user.RefreshToken, "ua", "ip")
	if err != nil {
		t.Fatalf("refresh after the failed switch: %v", err)
	}
	if res.ActiveTeamID != f.user.ActiveTeamID {
		t.Fatalf("the next refresh landed on team %q, want %q: the failed switch moved the session", res.ActiveTeamID, f.user.ActiveTeamID)
	}
}

// TestHandleSwitchOrg_NoTeamInTheOrgStaysOK pins the legitimate landing:
// a plain member the store SUCCESSFULLY reports as holding no team in the
// org still switches, org-scoped with an empty active team.
func TestHandleSwitchOrg_NoTeamInTheOrgStaysOK(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	seedActiveUser(t, s, "u3", "u3@example.com")
	seedOrg(t, s, "o2", "org-two")
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{
		ID: "tb", OrgID: "o2", Name: "TB", Slug: "tb", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed team tb: %v", err)
	}
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "u3", OrgID: "o2", Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed org membership: %v", err)
	}
	r := httptest.NewRequest("POST", "/api/auth/me/org/o2", nil)
	r.SetPathValue("org_id", "o2")
	r = r.WithContext(auth.WithIdentity(ctx, auth.Identity{UserID: "u3", Credential: auth.CredentialAccessToken}))
	w := httptest.NewRecorder()
	s.handleSwitchOrg(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a member with no team in the org must still switch, got %d: %s", w.Code, w.Body.String())
	}
	var res AuthSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode switch body: %v", err)
	}
	if res.ActiveOrg != "o2" || res.ActiveTeam != "" {
		t.Fatalf("expected an org-scoped landing with no active team, got org=%q team=%q", res.ActiveOrg, res.ActiveTeam)
	}
}

// TestHandleListTeams_TeamReadFailureIsNotAnEmptyList covers the sibling
// of the tree on the same route family: a failed read behind GET
// /api/teams must not answer an empty list, which reads as "you hold no
// team grant".
func TestHandleListTeams_TeamReadFailureIsNotAnEmptyList(t *testing.T) {
	for _, read := range []string{"ListMembershipsByUser", "GetTeamsByIDs"} {
		t.Run(read, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			f.ids.fail(read, 1)
			r := orgReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID}), "GET", "/api/teams", "", "")
			w := httptest.NewRecorder()
			f.s.handleListTeams(w, r)
			assertIdentityUnavailable(t, w, f.logs, "GET /api/teams with a failed "+read)
		})
	}
}

// TestHandleListTeamMembers_AccountReadFailureIsNotNamelessMembers covers
// the members list of the same family: a failed read of the members or of
// their accounts must not answer every member as an account with no email
// or name.
func TestHandleListTeamMembers_AccountReadFailureIsNotNamelessMembers(t *testing.T) {
	for _, read := range []string{"ListMembershipsByTeam", "GetUsersByIDs"} {
		t.Run(read, func(t *testing.T) {
			f := newAuthRouteFixture(t)
			f.ids.fail(read, 1)
			r := orgReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: f.user.User.ID, TeamID: "ta", Role: identity.RoleMember}),
				"GET", "/api/teams/ta/members", "", "ta")
			w := httptest.NewRecorder()
			f.s.handleListTeamMembers(w, r)
			assertIdentityUnavailable(t, w, f.logs, "GET /api/teams/{id}/members with a failed "+read)
		})
	}
}

// TestAuthError_RefusalsKeepTheirMessage pins the other half of authError: a
// refusal is not a failure, and keeps its own status and message.
func TestAuthError_RefusalsKeepTheirMessage(t *testing.T) {
	s := newOrgTestServer(t)
	for _, tc := range []struct {
		err    error
		status int
	}{
		{auth.ErrSessionRevoked, http.StatusUnauthorized},
		{auth.ErrNotAMember, http.StatusForbidden},
		{identity.ErrEmailAlreadyTaken, http.StatusConflict},
	} {
		w := httptest.NewRecorder()
		s.authError(w, httptest.NewRequest("POST", "/api/auth/x", nil), tc.err)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.err.Error()) {
			t.Errorf("authError(%v) answered %d %s, want %d with its message", tc.err, w.Code, w.Body.String(), tc.status)
		}
	}
	w := httptest.NewRecorder()
	s.authError(w, httptest.NewRequest("POST", "/api/auth/x", nil), errors.New("anything else"))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "anything else") {
		t.Errorf("an unmapped error answered %d %s, want 500 without its text", w.Code, w.Body.String())
	}
}
