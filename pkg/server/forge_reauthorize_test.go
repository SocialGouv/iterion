package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// reauthorize drives POST …/connections/{cid}/reauthorize and returns the
// recorder (whose Set-Cookie carries the agent binding the callback checks).
func reauthorize(t *testing.T, s *Server, ctx context.Context, connID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := forgeReq(ctx, "POST", "/api/teams/t1/forge/connections/"+connID+"/reauthorize", body, "t1")
	r.SetPathValue("conn_id", connID)
	w := httptest.NewRecorder()
	s.handleReauthorizeForgeConnection(w, r)
	return w
}

// driveOAuthCallback completes the flow an authorize_url started, with the
// agent-binding cookies of the response that issued it.
func driveOAuthCallback(t *testing.T, s *Server, w *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	var resp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	authz, err := url.Parse(resp.AuthorizeURL)
	if err != nil || authz.Query().Get("state") == "" {
		t.Fatalf("authorize_url = %q", resp.AuthorizeURL)
	}
	cb := httptest.NewRequest("GET", "/api/forge/oauth/callback?state="+url.QueryEscape(authz.Query().Get("state"))+"&code=abc", nil)
	for _, c := range w.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	s.handleForgeOAuthCallback(out, cb)
	return out
}

// seedReauthWorld connects a GitLab OAuth connection (as alice), enables a
// repo on it (so the connection holds a managed secret), then flips it to
// needs_reauth — the stuck state #2698 names — with the managed secret
// pre-corrupted to the dead token, so a successful re-authorise must REWRITE
// it, not merely leave it.
func seedReauthWorld(t *testing.T) (*Server, *projectBotGitLab, forge.Connection) {
	t.Helper()
	gl := &projectBotGitLab{}
	srv := gl.server()
	t.Cleanup(srv.Close)
	s := newProjectBotTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	ctx := superAdminCtx()

	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, forgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"gitlab","forge_base_url":"`+srv.URL+`","client_id":"cid","client_secret":"csecret"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("register platform app: code=%d body=%s", w.Code, w.Body.String())
	}

	// A PLAIN OAuth connection (posting as alice): the project-token identity
	// keeps no managed secret on the connection, and the rewrite half of the
	// repair is exactly what this test witnesses.
	w = httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"oauth","forge_base_url":"`+srv.URL+`","next":"/integrations"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	cb := driveOAuthCallback(t, s, w)
	if cb.Code != http.StatusFound {
		t.Fatalf("connect callback: code=%d body=%s", cb.Code, cb.Body.String())
	}
	conns, err := s.forgeConnections.ListByTenant(context.Background(), "t1")
	if err != nil || len(conns) != 1 {
		t.Fatalf("connections = %d err=%v", len(conns), err)
	}
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+conns[0].ID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := s.forgeConnections.Get(context.Background(), conns[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ManagedSecretID == "" {
		t.Fatal("the enable did not stamp the connection's managed secret")
	}
	// The dead credential the sweep kept re-trying.
	stored.Status = forge.StatusNeedsReauth
	stored.StatusReason = "refresh failed: invalid_grant"
	past := time.Now().UTC().Add(-time.Hour)
	stored.AccessTokenExpiresAt = &past
	if err := s.forgeConnections.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	// The managed secret holds the DEAD access token; the re-authorise must
	// replace it, since the next scheduled refresh is a token lifetime out.
	gs, err := s.genericSecrets.Get(context.Background(), stored.ManagedSecretID)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := secrets.SealGenericSecret(s.sealer, gs.ID, []byte("dead-access-token"))
	if err != nil {
		t.Fatal(err)
	}
	gs.SealedSecret = dead
	if err := s.genericSecrets.Update(context.Background(), gs); err != nil {
		t.Fatal(err)
	}
	return s, gl, stored
}

// The gesture the ticket asks for: a needs_reauth OAuth connection is
// re-authorised IN PLACE — same row, integrations and managed secret intact,
// status back to active — instead of a second connection next to the dead one.
func TestForgeReauthorize_RepairsTheConnectionInPlace(t *testing.T) {
	s, _, conn := seedReauthWorld(t)

	w := reauthorize(t, s, superAdminCtx(), conn.ID, `{"next":"/integrations"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reauthorize: code=%d body=%s", w.Code, w.Body.String())
	}
	cb := driveOAuthCallback(t, s, w)
	if cb.Code != http.StatusFound {
		t.Fatalf("callback: code=%d body=%s", cb.Code, cb.Body.String())
	}
	if loc := cb.Header().Get("Location"); !strings.Contains(loc, "connected="+conn.ID) || !strings.HasPrefix(loc, "/integrations") {
		t.Fatalf("redirect = %q, want the pinned connection on the next path", loc)
	}

	conns, err := s.forgeConnections.ListByTenant(context.Background(), "t1")
	if err != nil || len(conns) != 1 {
		t.Fatalf("connections = %d err=%v — a re-authorise must not mint a second connection", len(conns), err)
	}
	got := conns[0]
	if got.ID != conn.ID || got.Status != forge.StatusActive || got.StatusReason != "" {
		t.Fatalf("connection = id %q status %q reason %q", got.ID, got.Status, got.StatusReason)
	}
	if got.AccessTokenExpiresAt == nil || !got.AccessTokenExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("AccessTokenExpiresAt = %v, want the fresh grant's expiry", got.AccessTokenExpiresAt)
	}
	// The fresh pair is sealed INTO the existing row…
	tok, err := forge.AdminTokenFor(s.sealer, got)
	if err != nil {
		t.Fatal(err)
	}
	if tok != pbGrant {
		t.Fatalf("sealed admin token = %q, want the fresh grant", tok)
	}
	// …and the managed secret was rewritten, so bot runs stop reading the
	// dead token immediately.
	gs, err := s.genericSecrets.Get(context.Background(), got.ManagedSecretID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := secrets.OpenGenericSecret(s.sealer, gs.ID, gs.SealedSecret)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != pbGrant {
		t.Fatalf("managed secret still serves %q, want the fresh grant", string(raw))
	}
	// The integration the dead connection held survives untouched.
	ints, err := s.forgeIntegrations.ListByTenant(context.Background(), "t1")
	if err != nil || len(ints) != 1 || ints[0].ConnectionID != conn.ID {
		t.Fatalf("integrations = %+v err=%v", ints, err)
	}

	var events []audit.Event
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		events, _ = s.auditStore.ListByTenant(context.Background(), "t1", audit.Page{Action: "forge.connection.reauthorized"})
		if len(events) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if len(events) != 1 || events[0].TargetID != conn.ID {
		t.Fatalf("audit trail = %+v, want one forge.connection.reauthorized on %s", events, conn.ID)
	}
}

// The pin authorises a credential for the connection's ACCOUNT, nobody else:
// the repositories' minted bots hang off the first account's grant.
func TestForgeReauthorize_RefusesADifferentIdentity(t *testing.T) {
	s, gl, conn := seedReauthWorld(t)
	gl.mu.Lock()
	gl.grant = "oauth-grant-of-bob"
	gl.altUser.id, gl.altUser.username = 43, "bob"
	gl.mu.Unlock()

	w := reauthorize(t, s, superAdminCtx(), conn.ID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("reauthorize: code=%d body=%s", w.Code, w.Body.String())
	}
	cb := driveOAuthCallback(t, s, w)
	if cb.Code != http.StatusUnprocessableEntity || !strings.Contains(cb.Body.String(), "@bob") {
		t.Fatalf("callback: code=%d body=%s, want a 422 naming the mismatching account", cb.Code, cb.Body.String())
	}
	got, err := s.forgeConnections.Get(context.Background(), conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != forge.StatusNeedsReauth {
		t.Fatalf("a refused re-authorise still flipped the status to %q", got.Status)
	}
	if string(got.SealedPayload) == "" || !bytes.Equal(got.SealedPayload, conn.SealedPayload) {
		t.Fatal("a refused re-authorise rewrote the sealed payload")
	}
	conns, _ := s.forgeConnections.ListByTenant(context.Background(), "t1")
	if len(conns) != 1 {
		t.Fatalf("a refused re-authorise minted a connection: %d rows", len(conns))
	}
}

// The route's own guards: OAuth-only, tenant-scoped, admin-gated.
func TestForgeReauthorize_Guards(t *testing.T) {
	s, _, conn := seedReauthWorld(t)

	// A PAT connection takes its new token via PATCH, not the OAuth flow.
	sealed, err := forge.SealPAT(s.sealer, "c-pat", "glpat-x")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "c-pat", TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindPAT,
		AccountLogin: "botuser", Status: forge.StatusRevoked, SealedPayload: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	if w := reauthorize(t, s, superAdminCtx(), "c-pat", ""); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("pat connection: code=%d body=%s, want 422", w.Code, w.Body.String())
	}

	// Another team's connection is a 404 (non-enumeration discipline).
	if w := reauthorize(t, s, superAdminCtx(), "missing", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown connection: code=%d, want 404", w.Code)
	}

	// A plain member cannot start the flow.
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{UserID: "u2", TeamID: "t1", Role: identity.RoleMember}); err != nil {
		t.Fatal(err)
	}
	member := auth.WithIdentity(context.Background(), auth.Identity{UserID: "u2", TeamID: "t1", Role: identity.RoleMember})
	if w := reauthorize(t, s, member, conn.ID, ""); w.Code != http.StatusForbidden {
		t.Fatalf("member: code=%d, want 403", w.Code)
	}
}
