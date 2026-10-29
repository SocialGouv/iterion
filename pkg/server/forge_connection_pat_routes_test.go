package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// patRotateGitLab is a GitLab whose /user resolves the BEARER TOKEN, so a
// token rotation (same account, new value) can be told from a move to another
// account — the distinction #2488's guard exists to make. Every bearer it
// serves is recorded per endpoint family, which is how the tests prove the
// forge saw the new token after the swap.
type patRotateGitLab struct {
	mu         sync.Mutex
	users      map[string]map[string]any // token → /user body
	whoamiSeen []string
	reposSeen  []string
	hookBearer []string // bearer of every hook write/delete
	hooks      int      // live hooks
}

func (m *patRotateGitLab) bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (m *patRotateGitLab) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		tok := m.bearer(r)
		m.whoamiSeen = append(m.whoamiSeen, tok)
		body, ok := m.users[tok]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		tok := m.bearer(r)
		m.reposSeen = append(m.reposSeen, tok)
		if _, ok := m.users[tok]; !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 194, "path_with_namespace": "group/api", "visibility": "private", "default_branch": "main"},
		})
	})
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		tok := m.bearer(r)
		if _, ok := m.users[tok]; !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case http.MethodPost:
			m.hookBearer = append(m.hookBearer, tok)
			m.hooks++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["id"] = m.hooks
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc("DELETE /api/v4/projects/group%2Fapi/hooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		tok := m.bearer(r)
		if _, ok := m.users[tok]; !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		m.hookBearer = append(m.hookBearer, tok)
		m.hooks--
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}

// seedPATConnectionWithRepo connects a pat connection on gl and enables a
// repo on it, so the connection carries an integration and a managed secret —
// the things a token replacement must keep working.
func seedPATConnectionWithRepo(t *testing.T, s *Server, baseURL, token string) forge.Connection {
	t.Helper()
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"pat","forge_base_url":"`+baseURL+`","pat":"`+token+`"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	connID := resp.Connection.ID
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+connID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	conn, err := s.forgeConnections.Get(context.Background(), connID)
	if err != nil {
		t.Fatal(err)
	}
	if conn.ManagedSecretID == "" {
		t.Fatal("the enable did not stamp the connection's managed secret")
	}
	return conn
}

func patchConnection(t *testing.T, s *Server, connID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := forgeReq(superAdminCtx(), "PATCH", "/api/teams/t1/forge/connections/"+connID, body, "t1")
	r.SetPathValue("conn_id", connID)
	w := httptest.NewRecorder()
	s.handlePatchForgeConnection(w, r)
	return w
}

func managedSecretPlaintext(t *testing.T, s *Server, secretID string) string {
	t.Helper()
	gs, err := s.genericSecrets.Get(context.Background(), secretID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := secrets.OpenGenericSecret(s.sealer, gs.ID, gs.SealedSecret)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func auditEvents(t *testing.T, s *Server, action string) []audit.Event {
	t.Helper()
	var events []audit.Event
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		events, _ = s.auditStore.ListByTenant(context.Background(), "t1", audit.Page{Action: action})
		if len(events) > 0 || time.Now().After(deadline) {
			break
		}
	}
	return events
}

// The ticket's acceptance: a PAT connection whose token is swapped for one of
// the SAME account keeps its integration, its hook and its managed secret —
// the forge serves the new token — and a revoked connection is active again.
func TestForgeConnectionReplaceToken_SameAccountKeepsEverythingWorking(t *testing.T) {
	gl := &patRotateGitLab{users: map[string]map[string]any{
		"glpat-old": {"id": 7, "username": "botuser"},
		"glpat-new": {"id": 7, "username": "botuser"},
	}}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	s.auditStore = audit.NewMemoryStore()

	conn := seedPATConnectionWithRepo(t, s, srv.URL, "glpat-old")

	// The expired token's death, as the avatar probe would have recorded it.
	conn.Status = forge.StatusRevoked
	conn.StatusReason = "credential rejected (401)"
	if err := s.forgeConnections.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	w := patchConnection(t, s, conn.ID, `{"pat":"glpat-new"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "glpat-new") {
		t.Fatal("the token value leaked into the response body")
	}

	got, err := s.forgeConnections.Get(context.Background(), conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != forge.StatusActive || got.StatusReason != "" {
		t.Fatalf("status = %q reason %q, want active again", got.Status, got.StatusReason)
	}
	if got.ID != conn.ID || got.AccountLogin != "botuser" {
		t.Fatalf("the row must be replaced in place: %+v", got)
	}
	tok, err := forge.AdminTokenFor(s.sealer, got)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "glpat-new" {
		t.Fatalf("sealed payload still serves %q", tok)
	}
	if plain := managedSecretPlaintext(t, s, conn.ManagedSecretID); plain != "glpat-new" {
		t.Fatalf("managed secret still serves %q — bot runs would read the dead token forever", plain)
	}
	// The forge itself was WhoAmI'd with the NEW token before anything moved.
	gl.mu.Lock()
	whoami := append([]string(nil), gl.whoamiSeen...)
	gl.mu.Unlock()
	if len(whoami) == 0 || whoami[len(whoami)-1] != "glpat-new" {
		t.Fatalf("the forge was not probed with the new token: %v", whoami)
	}

	// Hooks keep working: the disable's hook delete rides the new token.
	var listResp struct {
		Integrations []struct {
			ID string `json:"id"`
		} `json:"integrations"`
	}
	w = httptest.NewRecorder()
	s.handleListForgeRepoBots(w, forgeReq(superAdminCtx(), "GET", "/api/teams/t1/forge/repo-bots", "", "t1"))
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil || len(listResp.Integrations) != 1 {
		t.Fatalf("list integrations: %v %s", err, w.Body.String())
	}
	w = httptest.NewRecorder()
	delReq := forgeReq(superAdminCtx(), "DELETE", "/api/teams/t1/forge/repo-bots/"+listResp.Integrations[0].ID, "", "t1")
	delReq.SetPathValue("integration_id", listResp.Integrations[0].ID)
	s.handleDisableForgeRepoBots(w, delReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disable: code=%d body=%s", w.Code, w.Body.String())
	}
	gl.mu.Lock()
	hookCalls := append([]string(nil), gl.hookBearer...)
	gl.mu.Unlock()
	if last := hookCalls[len(hookCalls)-1]; last != "glpat-new" {
		t.Fatalf("the hook delete rode %q, want the replacement token", last)
	}

	events := auditEvents(t, s, "forge.connection.token_replaced")
	if len(events) != 1 || events[0].TargetID != conn.ID {
		t.Fatalf("audit trail = %+v", events)
	}
	if b, _ := json.Marshal(events[0]); strings.Contains(string(b), "glpat-new") {
		t.Fatalf("the token value leaked into the audit row: %s", b)
	}
}

// A token of ANOTHER account is refused 422 and nothing changes: the
// connection's hooks and minted secrets hang off the first account.
func TestForgeConnectionReplaceToken_DifferentAccountIsRefused(t *testing.T) {
	gl := &patRotateGitLab{users: map[string]map[string]any{
		"glpat-old":   {"id": 7, "username": "botuser"},
		"glpat-other": {"id": 99, "username": "mallory"},
	}}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	s.auditStore = audit.NewMemoryStore()

	conn := seedPATConnectionWithRepo(t, s, srv.URL, "glpat-old")
	conn.Status = forge.StatusRevoked
	if err := s.forgeConnections.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}

	w := patchConnection(t, s, conn.ID, `{"pat":"glpat-other"}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "@mallory") {
		t.Fatalf("code=%d body=%s, want a 422 naming the mismatching account", w.Code, w.Body.String())
	}
	got, err := s.forgeConnections.Get(context.Background(), conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != forge.StatusRevoked || got.AccountLogin != "botuser" {
		t.Fatalf("a refused swap moved the row: %+v", got)
	}
	tok, _ := forge.AdminTokenFor(s.sealer, got)
	if tok != "glpat-old" {
		t.Fatalf("a refused swap replaced the sealed token with %q", tok)
	}
	if plain := managedSecretPlaintext(t, s, conn.ManagedSecretID); plain != "glpat-old" {
		t.Fatalf("a refused swap rewrote the managed secret to %q", plain)
	}
	if events := auditEvents(t, s, "forge.connection.token_replaced"); len(events) != 0 {
		t.Fatalf("a refused swap was audited as done: %+v", events)
	}
}

// The deliberate account move: the operator's explicit flag carries it, the
// identity is re-stamped, and the audit row says the account changed.
func TestForgeConnectionReplaceToken_AllowAccountChangeIsAudited(t *testing.T) {
	gl := &patRotateGitLab{users: map[string]map[string]any{
		"glpat-old":   {"id": 7, "username": "botuser"},
		"glpat-other": {"id": 99, "username": "mallory"},
	}}
	srv := gl.server()
	defer srv.Close()
	s := newForgeTestServer(t)
	s.auditStore = audit.NewMemoryStore()

	conn := seedPATConnectionWithRepo(t, s, srv.URL, "glpat-old")
	w := patchConnection(t, s, conn.ID, `{"pat":"glpat-other","allow_account_change":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("patch: code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := s.forgeConnections.Get(context.Background(), conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLogin != "mallory" || got.AccountID != "99" {
		t.Fatalf("the account move was not stamped: %+v", got)
	}
	events := auditEvents(t, s, "forge.connection.token_replaced")
	if len(events) != 1 || events[0].Meta["account_changed"] != true {
		t.Fatalf("audit trail = %+v, want account_changed: true", events)
	}
}

// The guard rails of the new field: OAuth connections are re-authorised, not
// patched; the token field travels alone.
func TestForgeConnectionReplaceToken_Guards(t *testing.T) {
	s := newForgeTestServer(t)
	sealed, err := forge.SealOAuthTokens(s.sealer, "c-oauth", "acc", "ref", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "c-oauth", TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindOAuthApp,
		AccountLogin: "alice", Status: forge.StatusNeedsReauth, SealedPayload: sealed,
	}); err != nil {
		t.Fatal(err)
	}
	if w := patchConnection(t, s, "c-oauth", `{"pat":"glpat-x"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("oauth connection: code=%d body=%s, want 422", w.Code, w.Body.String())
	}

	gl := &patRotateGitLab{users: map[string]map[string]any{"glpat-old": {"id": 7, "username": "botuser"}}}
	srv := gl.server()
	defer srv.Close()
	conn := seedPATConnectionWithRepo(t, s, srv.URL, "glpat-old")
	if w := patchConnection(t, s, conn.ID, `{"pat":"glpat-old","webhook_base_url":"https://x.example"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("combined body: code=%d body=%s, want 400", w.Code, w.Body.String())
	}
	if w := patchConnection(t, s, conn.ID, `{"pat":"  "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty token: code=%d body=%s, want 400", w.Code, w.Body.String())
	}
}
