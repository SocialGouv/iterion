package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The tenant-facing connector-connection CRUD (#2759). The properties pinned
// here are the ticket's done-when: a tenant adds, lists and revokes; a
// cross-tenant read answers not-found; no response carries a credential.

func newConnectionTestServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	s.connectionStore = connection.NewMemoryStore()
	s.connectorCatalog = connection.BakedCatalog()
	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	s.sealer = sealer
	ctx := context.Background()
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{
		ID: "t1", Name: "t1", Slug: "t1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{
		ID: "t2", Name: "t2", Slug: "t2", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []identity.Membership{
		{UserID: "admin1", TeamID: "t1", Role: identity.RoleAdmin},
		{UserID: "viewer1", TeamID: "t1", Role: identity.RoleViewer},
		{UserID: "admin2", TeamID: "t2", Role: identity.RoleAdmin},
	} {
		if err := s.authStore().UpsertMembership(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func connReq(ctx context.Context, method, path, body, teamID, connID string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ctx)
	r.SetPathValue("id", teamID)
	if connID != "" {
		r.SetPathValue("conn_id", connID)
	}
	return r
}

func connUserCtx(userID string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: userID, Credential: auth.CredentialAccessToken})
}

func createTestConnection(t *testing.T, s *Server, ctx context.Context, teamID string) connectionDTO {
	t.Helper()
	w := httptest.NewRecorder()
	body := `{"connector":"forgejo","alias":"main","base_url":"https://git.example.com","scheme_id":"token","token":"probe-token"}`
	s.handleCreateConnection(w, connReq(ctx, "POST", "/api/teams/"+teamID+"/connections", body, teamID, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var got connectionDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The credential NEVER travels: not the value, not the sealed bytes.
	if strings.Contains(w.Body.String(), "probe-token") {
		t.Fatalf("the response carries the credential: %s", w.Body.String())
	}
	return got
}

func TestConnectionRoutesCreateListRevokeDelete(t *testing.T) {
	s := newConnectionTestServer(t)
	ctx := connUserCtx("admin1")

	created := createTestConnection(t, s, ctx, "t1")
	if created.ID == "" || created.Connector != "forgejo" || created.Alias != "main" {
		t.Fatalf("created = %+v", created)
	}
	if created.Status != string(connection.StatusActive) {
		t.Errorf("status = %q, want active", created.Status)
	}
	// The placement is pinned from the package at creation — where the
	// credential goes on the wire is decided here, not at call time.
	if created.Placement == "" {
		t.Error("placement is empty: the origin half is pinned but the how half is not")
	}

	// LIST, as the team's viewer (reads need membership, not admin).
	w := httptest.NewRecorder()
	s.handleListConnections(w, connReq(connUserCtx("viewer1"), "GET", "/api/teams/t1/connections", "", "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "probe-token") {
		t.Fatalf("the listing carries the credential: %s", w.Body.String())
	}
	var listed struct {
		Connections []connectionDTO `json:"connections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed.Connections) != 1 || listed.Connections[0].ID != created.ID {
		t.Fatalf("listed = %+v, want the one created", listed.Connections)
	}

	// REVOKE via PATCH, then the record reads revoked.
	w = httptest.NewRecorder()
	s.handlePatchConnection(w, connReq(ctx, "PATCH", "/api/teams/t1/connections/"+created.ID,
		`{"status":"revoked","status_reason":"rotated off the vendor"}`, "t1", created.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: code=%d body=%s", w.Code, w.Body.String())
	}
	var revoked connectionDTO
	if err := json.Unmarshal(w.Body.Bytes(), &revoked); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if revoked.Status != string(connection.StatusRevoked) || revoked.StatusReason == "" {
		t.Errorf("revoked = %+v, want status revoked with its reason", revoked)
	}

	// DELETE, and the record is gone for the tenant itself.
	w = httptest.NewRecorder()
	s.handleDeleteConnection(w, connReq(ctx, "DELETE", "/api/teams/t1/connections/"+created.ID, "", "t1", created.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleGetConnection(w, connReq(ctx, "GET", "/api/teams/t1/connections/"+created.ID, "", "t1", created.ID))
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete: code=%d, want 404", w.Code)
	}
}

// TestConnectionRoutesCrossTenantIsNotFound pins the first of the four
// properties pkg/connection's doc holds by construction, at the ROUTE: a
// caller holding another tenant's id must read the same answer as a record
// that does not exist — never a distinguishable "forbidden".
func TestConnectionRoutesCrossTenantIsNotFound(t *testing.T) {
	s := newConnectionTestServer(t)
	created := createTestConnection(t, s, connUserCtx("admin1"), "t1")

	other := connUserCtx("admin2") // an admin of t2 holding t1's connection id
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		method  string
		body    string
	}{
		{"get", s.handleGetConnection, "GET", ""},
		{"patch", s.handlePatchConnection, "PATCH", `{"display_name":"x"}`},
		{"delete", s.handleDeleteConnection, "DELETE", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.handler(w, connReq(other, tc.method, "/api/teams/t2/connections/"+created.ID, tc.body, "t2", created.ID))
			if w.Code != http.StatusNotFound {
				t.Errorf("cross-tenant %s: code=%d body=%s, want 404 — the two answers must read the same",
					tc.name, w.Code, w.Body.String())
			}
		})
	}
	// And t2's listing must not contain it.
	w := httptest.NewRecorder()
	s.handleListConnections(w, connReq(other, "GET", "/api/teams/t2/connections", "", "t2", ""))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), created.ID) {
		t.Errorf("cross-tenant list: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestConnectionRoutesRefuseWhatNoCallCouldUse(t *testing.T) {
	s := newConnectionTestServer(t)
	ctx := connUserCtx("admin1")
	for _, tc := range []struct {
		name, body, wantIn string
		code               int
	}{
		{"no connector", `{"token":"x"}`, "connector", http.StatusBadRequest},
		{"unknown connector", `{"connector":"nope","token":"x"}`, "nope", http.StatusBadRequest},
		{"several schemes and none named", `{"connector":"forgejo","base_url":"https://git.example.com","token":"x"}`, "scheme_id", http.StatusBadRequest},
		{"a token for a basic scheme", `{"connector":"forgejo","scheme_id":"basic","base_url":"https://git.example.com","token":"x"}`, "not a token", http.StatusBadRequest},
		{"a basic scheme without its username", `{"connector":"forgejo","scheme_id":"basic","base_url":"https://git.example.com","password":"pw"}`, "needs a username", http.StatusBadRequest},
		{"a basic password that cannot authenticate", `{"connector":"forgejo","scheme_id":"basic","base_url":"https://git.example.com","username":"jo","password":"bad\npw"}`, "newline", http.StatusBadRequest},
		{"a username for a token scheme", `{"connector":"forgejo","scheme_id":"token","base_url":"https://git.example.com","token":"x","username":"jo"}`, "takes a token", http.StatusBadRequest},
		{"a credential that cannot authenticate", `{"connector":"forgejo","scheme_id":"token","base_url":"https://git.example.com","token":"bad\ntoken"}`, "newline", http.StatusBadRequest},
		{"an unknown capability", `{"connector":"forgejo","scheme_id":"token","base_url":"https://git.example.com","token":"x","capabilities":["root"]}`, "unknown capability", http.StatusBadRequest},
		{"a duplicate alias", `{"connector":"forgejo","scheme_id":"token","base_url":"https://git.example.com","token":"x"}`, "already exists", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "a duplicate alias" {
				createTestConnection(t, s, ctx, "t1")
			}
			w := httptest.NewRecorder()
			s.handleCreateConnection(w, connReq(ctx, "POST", "/api/teams/t1/connections", tc.body, "t1", ""))
			if w.Code != tc.code {
				t.Fatalf("code=%d body=%s, want %d", w.Code, w.Body.String(), tc.code)
			}
			if !strings.Contains(w.Body.String(), tc.wantIn) {
				t.Errorf("the refusal must say what to fix — want %q in: %s", tc.wantIn, w.Body.String())
			}
		})
	}
}

// TestConnectionPatchKeepsTheCredentialByDefault pins the kept-credential
// rule at the ROUTE: a PATCH that edits the display name must leave the
// sealed payload untouched, and one carrying a token rotates it.
func TestConnectionPatchKeepsTheCredentialByDefault(t *testing.T) {
	s := newConnectionTestServer(t)
	ctx := connUserCtx("admin1")
	created := createTestConnection(t, s, ctx, "t1")

	before, err := s.connectionStore.Get(context.Background(), "t1", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	w := httptest.NewRecorder()
	s.handlePatchConnection(w, connReq(ctx, "PATCH", "/api/teams/t1/connections/"+created.ID,
		`{"display_name":"renamed"}`, "t1", created.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", w.Code, w.Body.String())
	}
	after, err := s.connectionStore.Get(context.Background(), "t1", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(after.SealedPayload) != string(before.SealedPayload) {
		t.Error("a display-name edit rewrote the credential — the connection now 401s at its next call")
	}

	w = httptest.NewRecorder()
	s.handlePatchConnection(w, connReq(ctx, "PATCH", "/api/teams/t1/connections/"+created.ID,
		`{"token":"rotated-token"}`, "t1", created.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("rotate: code=%d body=%s", w.Code, w.Body.String())
	}
	rotated, err := s.connectionStore.Get(context.Background(), "t1", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(rotated.SealedPayload) == string(before.SealedPayload) {
		t.Error("a patch carrying a token must rotate the credential")
	}
	if strings.Contains(w.Body.String(), "rotated-token") {
		t.Errorf("the rotation response carries the credential: %s", w.Body.String())
	}
}

// TestConnectionRoutesRequireMembership: a viewer may read but not write; an
// account with no membership gets nothing at all.
func TestConnectionRoutesRequireMembership(t *testing.T) {
	s := newConnectionTestServer(t)
	created := createTestConnection(t, s, connUserCtx("admin1"), "t1")

	w := httptest.NewRecorder()
	s.handlePatchConnection(w, connReq(connUserCtx("viewer1"), "PATCH", "/api/teams/t1/connections/"+created.ID,
		`{"status":"revoked"}`, "t1", created.ID))
	if w.Code != http.StatusForbidden {
		t.Errorf("a viewer writing: code=%d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	s.handleListConnections(w, connReq(connUserCtx("stranger"), "GET", "/api/teams/t1/connections", "", "t1", ""))
	if w.Code != http.StatusForbidden {
		t.Errorf("a stranger listing: code=%d, want 403", w.Code)
	}
}

// A connection's instance URL is pinned trimmed of its trailing slash — the
// executor joins each operation's path after it — except for a package that
// sends a request to the URL itself, where the slash is the operator's:
// WordPress serves its feed at `/feed/`, and `/feed` answers a redirect the
// guarded client does not follow.
func TestAResourceURLKeepsItsTrailingSlashWhenPinned(t *testing.T) {
	ordinary := &spec.Package{Connector: spec.Connector{ID: "forge"}}
	feed := &spec.Package{Connector: spec.Connector{ID: "rss"}, Ops: []spec.OpsFile{{Operations: []spec.Operation{
		{ID: "rss.feed.read", HTTP: spec.HTTPBinding{Method: "GET", AtBaseURL: true}},
	}}}}
	for _, tc := range []struct {
		name string
		pkg  *spec.Package
		in   string
		want string
	}{
		{"an API origin", ordinary, " https://git.example.com/ ", "https://git.example.com"},
		{"a feed URL", feed, " https://blog.example.org/feed/ ", "https://blog.example.org/feed/"},
		{"a feed URL with a query", feed, "https://example.org/feed.xml?lang=fr", "https://example.org/feed.xml?lang=fr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveConnectionBaseURL(tc.pkg, tc.in)
			if err != nil || got != tc.want {
				t.Fatalf("resolveConnectionBaseURL(%q) = (%q, %v), want %q", tc.in, got, err, tc.want)
			}
		})
	}
}

// An HTTP basic connection is created through the route like a token one,
// and what it seals is what the resolver hands the executor: the username
// and password the operator typed, never a token-shaped blob.
func TestABasicConnectionSealsItsUsernameAndPassword(t *testing.T) {
	s := newConnectionTestServer(t)
	ctx := connUserCtx("admin1")
	w := httptest.NewRecorder()
	body := `{"connector":"forgejo","alias":"basic","base_url":"https://git.example.com","scheme_id":"basic","username":"jo","password":"probe-pw","capabilities":["action"]}`
	s.handleCreateConnection(w, connReq(ctx, "POST", "/api/teams/t1/connections", body, "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "probe-pw") {
		t.Fatalf("the response carries the password: %s", w.Body.String())
	}
	r := &connection.Resolver{Catalog: s.connectorCatalog, Store: s.connectionStore, Sealer: s.sealer,
		TenantID: "t1", Trust: connection.TrustOfRun(store.RunTrustDefault)}
	_, _, cred, _, err := r.ResolveAction(context.Background(), "forgejo.package.get_package", "basic")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cred.SchemeID != "basic" || cred.Username != "jo" || cred.Password != "probe-pw" || cred.Value != "" {
		t.Fatalf("resolved credential = scheme %q user %q (password set %v, token set %v), want basic jo/probe-pw",
			cred.SchemeID, cred.Username, cred.Password != "", cred.Value != "")
	}

	// A rotation names both halves; a token is not a basic credential.
	for _, tc := range []struct{ body, want string }{
		{`{"password":"pw2"}`, "together"},
		{`{"token":"x"}`, "together"},
		{`{"username":"jo","password":"pw2","token":"x"}`, "not a token"},
	} {
		var created connectionDTO
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		pw := httptest.NewRecorder()
		s.handlePatchConnection(pw, connReq(ctx, "PATCH", "/api/teams/t1/connections/"+created.ID, tc.body, "t1", created.ID))
		if pw.Code != http.StatusBadRequest || !strings.Contains(pw.Body.String(), tc.want) {
			t.Errorf("patch %s: code=%d body=%s, want a 400 naming %q", tc.body, pw.Code, pw.Body.String(), tc.want)
		}
	}
}

// A scheme of kind none presents nothing: its connection seals the empty
// credential, and any value sent for it is refused rather than sealed and
// never sent.
func TestASchemeThatPresentsNothingSealsNothing(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sealConnectionCredential(sealer, "conn_1", spec.AuthNone, "", "", ""); err != nil {
		t.Fatalf("an empty credential for kind none = %v", err)
	}
	for name, v := range map[string][3]string{
		"a token": {"x", "", ""}, "a username": {"", "jo", ""}, "a password": {"", "", "pw"},
	} {
		if _, err := sealConnectionCredential(sealer, "conn_1", spec.AuthNone, v[0], v[1], v[2]); err == nil || !strings.Contains(err.Error(), "presents no credential") {
			t.Errorf("%s for kind none = %v, want a refusal", name, err)
		}
	}
}

// A credential never rides the URL — it would be stored in the clear on the
// record every member reads — a basic password may hold spaces, a username
// no colon, and a sealer that fails is the server's, not the request's.
func TestAConnectionCredentialGoesWhereItsSchemeSays(t *testing.T) {
	s := newConnectionTestServer(t)
	ctx := connUserCtx("admin1")
	create := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleCreateConnection(w, connReq(ctx, "POST", "/api/teams/t1/connections", body, "t1", ""))
		return w
	}
	if w := create(`{"connector":"forgejo","alias":"u","base_url":"https://alice:hunter2@git.example.com","scheme_id":"token","token":"x"}`); w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("a URL carrying a user and password = %d %s, want 400 that does not echo it", w.Code, w.Body.String())
	}
	if w := create(`{"connector":"forgejo","alias":"b1","base_url":"https://git.example.com","scheme_id":"basic","username":" jo ","password":"correct horse battery staple"}`); w.Code != http.StatusOK {
		t.Fatalf("a password with spaces = %d %s, want it accepted", w.Code, w.Body.String())
	}
	if w := create(`{"connector":"forgejo","alias":"b2","base_url":"https://git.example.com","scheme_id":"basic","username":"jo:x","password":"pw"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "colon") {
		t.Fatalf("a username holding a colon = %d %s, want 400 naming it", w.Code, w.Body.String())
	}
	s.sealer = failingSealer{}
	if w := create(`{"connector":"forgejo","alias":"t","base_url":"https://git.example.com","scheme_id":"token","token":"x"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("a sealer failure = %d %s, want 500", w.Code, w.Body.String())
	}
}

// failingSealer is a sealer whose backend is unreachable.
type failingSealer struct{ secrets.Sealer }

func (failingSealer) Seal([]byte, []byte) ([]byte, error) { return nil, errors.New("kms unreachable") }
