package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// projectBotGitLab is a GitLab that serves the OAuth bootstrap end to end:
// the token endpoint, /user as the human (the grant) or as the bot (a minted
// token), the project hooks, and the project-access-token API. Every token it
// minted is remembered so the test reads which identity each call used.
type projectBotGitLab struct {
	mu          sync.Mutex
	mintStatus  int            // 0 = mint; 400 = GitLab's permission refusal
	mintBody    map[string]any // the last mint request body
	mintAuth    string         // Authorization header of the last mint
	minted      map[string]bool
	revoked     map[string]bool
	selfRevokes int
	avatarAuth  []string // Authorization header of every avatar upload
	hookBody    map[string]any
	hookDeletes int
	// grant, when non-empty, is the access token /oauth/token answers with
	// instead of pbGrant — a re-authorise as another account is witnessed by
	// pointing it at a token /user resolves to someone else (#2698).
	grant string
	// altUser is who /user reports for `grant` (zero value: nobody — a 401).
	altUser struct {
		id       int
		username string
	}
}

const (
	pbGrant  = "oauth-grant-of-alice"
	pbMinted = "glpat-minted-1"
	pbBot    = "project_194_bot_a7c08cc4"
)

func (m *projectBotGitLab) bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (m *projectBotGitLab) server() *httptest.Server {
	m.minted, m.revoked = map[string]bool{}, map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		grant := m.grant
		m.mu.Unlock()
		if grant == "" {
			grant = pbGrant
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": grant, "refresh_token": "refresh-1", "expires_in": 7200, "scope": "api", "token_type": "bearer"})
	})
	mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch tok := m.bearer(r); {
		case tok == pbGrant:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "username": "alice", "bot": false})
		case m.grant != "" && tok == m.grant:
			if m.altUser.id == 0 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": m.altUser.id, "username": m.altUser.username, "bot": false})
		case m.minted[tok] && !m.revoked[tok]:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 575, "username": pbBot, "bot": true})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	mux.HandleFunc("PUT /api/v4/user/avatar", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.avatarAuth = append(m.avatarAuth, m.bearer(r))
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"avatar_url": "https://gl/uploads/bot.png"})
	})
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.hookBody = body
			body["id"] = 9
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc("DELETE /api/v4/projects/group%2Fapi/hooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.hookDeletes++
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v4/projects/group%2Fapi/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.mintAuth = m.bearer(r)
		if m.mintAuth != pbGrant {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if m.mintStatus != 0 {
			w.WriteHeader(m.mintStatus)
			_, _ = w.Write([]byte(`{"message":"400 Bad request - User does not have permission to create project access token"}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&m.mintBody)
		// Like a GitLab that enforces token expiry (the PIC): the key must
		// travel, and the date may not pass the 365-day ceiling.
		expires, _ := m.mintBody["expires_at"].(string)
		if expires == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"400 Bad request - expires_at is missing"}`))
			return
		}
		if d, err := time.Parse("2006-01-02", expires); err != nil || d.After(time.Now().UTC().AddDate(0, 0, 365)) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Expires at must be before ` + time.Now().UTC().AddDate(0, 0, 365).Format("2006-01-02") + `"}`))
			return
		}
		m.minted[pbMinted] = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 58, "user_id": 575, "name": "iterion-bot", "token": pbMinted, "scopes": []string{"api"}, "access_level": 40, "expires_at": expires})
	})
	mux.HandleFunc("DELETE /api/v4/projects/group%2Fapi/access_tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.revoked[pbMinted] = true
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/v4/personal_access_tokens/self", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		tok := m.bearer(r)
		if !m.minted[tok] || m.revoked[tok] {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		m.selfRevokes++
		m.revoked[tok] = true
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}

func newProjectBotTestServer(t *testing.T) *Server {
	t.Helper()
	s := newForgeTestServer(t)
	s.forgeOAuthApps = forge.NewMemoryOAuthAppStore()
	s.cfg.PublicURL = "https://iterion.example.com"
	s.forgeOrchestrator.AdminForToken = s.forgeAdminForToken
	s.forgeOrchestrator.BotAvatar = []byte("png-bytes")
	return s
}

// oauthConnectProjectBot drives the connect + callback round trip of a
// GitLab OAuth connection posting as project bots, and returns the connection.
func oauthConnectProjectBot(t *testing.T, s *Server, base string) forge.Connection {
	t.Helper()
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections",
		`{"provider":"gitlab","mode":"oauth","forge_base_url":"`+base+`","bot_identity":"project_token","next":"/integrations"}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
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
	w = httptest.NewRecorder()
	s.handleForgeOAuthCallback(w, cb)
	if w.Code != http.StatusFound {
		t.Fatalf("callback: code=%d body=%s", w.Code, w.Body.String())
	}
	conns, err := s.forgeConnections.ListByTenant(context.Background(), "t1")
	if err != nil || len(conns) != 1 {
		t.Fatalf("connections = %d err=%v", len(conns), err)
	}
	return conns[0]
}

func TestForgeIntegration_OAuthProjectBot_ConnectEnableDisable(t *testing.T) {
	gl := &projectBotGitLab{}
	srv := gl.server()
	defer srv.Close()
	s := newProjectBotTestServer(t)
	ctx := superAdminCtx()

	// The PLATFORM registration of the instance's application, carrying the
	// hook base the instance accepts.
	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, forgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"gitlab","forge_base_url":"`+srv.URL+`","client_id":"cid","client_secret":"csecret","webhook_base_url":"https://iterion.fabrique.example/"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("register platform app: code=%d body=%s", w.Code, w.Body.String())
	}
	var app forge.ForgeOAuthApp
	_ = json.Unmarshal(w.Body.Bytes(), &app)
	if !app.Platform || app.WebhookBaseURL != "https://iterion.fabrique.example" {
		t.Fatalf("platform app = %+v", app)
	}

	conn := oauthConnectProjectBot(t, s, srv.URL)
	if conn.Kind != forge.KindOAuthApp || conn.BotIdentity != forge.BotIdentityProjectToken || conn.AccountLogin != "alice" {
		t.Fatalf("connection = %+v", conn)
	}
	if conn.OAuthAppID != app.ID {
		t.Errorf("OAuthAppID = %q, want the platform app %q", conn.OAuthAppID, app.ID)
	}
	if conn.WebhookBaseURL != "https://iterion.fabrique.example" {
		t.Errorf("WebhookBaseURL = %q: the app's default must be inherited", conn.WebhookBaseURL)
	}
	if !conn.MintsProjectBots() {
		t.Fatal("the connection must mint project bots")
	}

	// Before the enable, the preview announces a bot that does not exist yet.
	preview := func() forgeEnablePreview {
		w := httptest.NewRecorder()
		s.handlePreviewForgeEnable(w, forgeReq(ctx, "GET", "/api/teams/t1/forge/repo-bots/preview?connection_id="+conn.ID+"&bots=review-pr&repo=group%2Fapi", "", "t1"))
		if w.Code != http.StatusOK {
			t.Fatalf("preview: code=%d body=%s", w.Code, w.Body.String())
		}
		var pv forgeEnablePreview
		_ = json.Unmarshal(w.Body.Bytes(), &pv)
		return pv
	}
	if pv := preview(); !pv.Identity.ProjectBot || pv.Identity.Handle != "" || pv.HookBaseURL != "https://iterion.fabrique.example" {
		t.Errorf("preview before enable = %+v", pv.Identity)
	}

	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+conn.ID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.BotLogin != pbBot {
		t.Errorf("bot_login = %q", res.BotLogin)
	}

	gl.mu.Lock()
	mintAuth, mintBody, hookBody, avatarAuth := gl.mintAuth, gl.mintBody, gl.hookBody, append([]string(nil), gl.avatarAuth...)
	gl.mu.Unlock()
	if mintAuth != pbGrant {
		t.Errorf("the mint rode %q, want the OAuth grant", mintAuth)
	}
	if mintBody["name"] != "iterion-bot" || mintBody["access_level"] != float64(40) {
		t.Errorf("mint body = %v", mintBody)
	}
	if hookURL, _ := hookBody["url"].(string); !strings.HasPrefix(hookURL, "https://iterion.fabrique.example/api/webhooks/gitlab/") {
		t.Errorf("hook url = %q: the forge must be given the inherited base", hookURL)
	}
	if len(avatarAuth) != 1 || avatarAuth[0] != pbMinted {
		t.Errorf("avatar uploads = %v, want one with the minted token", avatarAuth)
	}

	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BotLogin != pbBot {
		t.Errorf("cfg.BotLogin = %q", cfg.BotLogin)
	}
	secID := cfg.SecretOverrides["forge_token"]
	gs, err := s.genericSecrets.Get(ctx, secID)
	if err != nil {
		t.Fatalf("pinned secret %q: %v", secID, err)
	}
	raw, _ := secrets.OpenGenericSecret(s.sealer, gs.ID, gs.SealedSecret)
	if string(raw) != pbMinted {
		t.Errorf("pinned token = %q, want the minted token, never the grant", raw)
	}
	stored, _ := s.forgeConnections.Get(ctx, conn.ID)
	if stored.ManagedSecretID != "" {
		t.Errorf("the connection grew a managed secret of its own: %q", stored.ManagedSecretID)
	}

	// The identity guard: alice's own MRs are reviewed, the bot's echoes are not.
	if s.realIterionBotAuthor(context.Background(), cfg, "alice") {
		t.Error("the authorising human reads as the bot")
	}
	if !s.realIterionBotAuthor(context.Background(), cfg, pbBot) {
		t.Error("the minted bot is not recognised as iterion's identity")
	}

	if pv := preview(); !pv.Identity.ProjectBot || pv.Identity.Handle != pbBot {
		t.Errorf("preview after enable = %+v", pv.Identity)
	}
	w = httptest.NewRecorder()
	s.handleListTeamForgeRepos(w, forgeReq(ctx, "GET", "/api/teams/t1/forge/repos", "", "t1"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"bot_login":"`+pbBot+`"`) {
		t.Errorf("repos: code=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	disReq := forgeReq(ctx, "DELETE", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, "", "t1")
	disReq.SetPathValue("integration_id", res.IntegrationID)
	s.handleDisableForgeRepoBots(w, disReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disable: code=%d body=%s", w.Code, w.Body.String())
	}
	gl.mu.Lock()
	revoked, selfRevokes, hookDeletes := gl.revoked[pbMinted], gl.selfRevokes, gl.hookDeletes
	gl.mu.Unlock()
	if !revoked || selfRevokes != 1 || hookDeletes != 1 {
		t.Errorf("after disable: revoked=%v self=%d hookDeletes=%d", revoked, selfRevokes, hookDeletes)
	}
	if _, err := s.genericSecrets.Get(ctx, secID); !errors.Is(err, secrets.ErrGenericSecretNotFound) {
		t.Errorf("the bot's secret survived the disable: %v", err)
	}

	// A refused mint is typed for the operator, with both remedies.
	gl.mu.Lock()
	gl.mintStatus = http.StatusBadRequest
	gl.mu.Unlock()
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+conn.ID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"project_bot_not_permitted"`) {
		t.Fatalf("refused mint: code=%d body=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{"User does not have permission", "Maintainer", "Project access tokens"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("refusal lacks %q: %s", want, w.Body.String())
		}
	}
}

func TestConnectForge_BotIdentityValidation(t *testing.T) {
	s := newProjectBotTestServer(t)
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, forgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"github","client_id":"cid","client_secret":"csecret"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	cases := []struct {
		name, body, want string
	}{
		{"project token on github", `{"provider":"github","mode":"oauth","bot_identity":"project_token"}`, "GitLab-only"},
		{"project token with a pasted token", `{"provider":"gitlab","mode":"pat","pat":"x","bot_identity":"project_token"}`, "applies to mode=oauth"},
		{"unknown identity", `{"provider":"gitlab","mode":"oauth","bot_identity":"group_token"}`, "unknown bot_identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections", tc.body, "t1"))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("code=%d body=%s, want 400 mentioning %q", w.Code, w.Body.String(), tc.want)
			}
		})
	}
}

func TestWriteForgeProvisionError_ProjectBot(t *testing.T) {
	s := newForgeTestServer(t)
	w := httptest.NewRecorder()
	s.writeForgeProvisionError(w, fmt.Errorf("forge: mint project bot on g/p: %w: gitlab answered HTTP 400: User does not have permission", forge.ErrProjectBotNotPermitted))
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d", w.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "project_bot_not_permitted" {
		t.Errorf("error = %q", body["error"])
	}
	for _, want := range []string{"User does not have permission", "Maintainer", "Project access tokens", "personal-token"} {
		if !strings.Contains(body["detail"], want) {
			t.Errorf("detail lacks %q: %s", want, body["detail"])
		}
	}
	w = httptest.NewRecorder()
	s.writeForgeProvisionError(w, fmt.Errorf("x: %w", forge.ErrProjectBotUnsupported))
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unsupported: code = %d", w.Code)
	}
}
