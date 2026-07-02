package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// mockGitLab is a minimal GitLab API the real forge/gitlab client runs
// against, so the full connect→enable→provision→disable path is validated
// over HTTP without a live GitLab.
type mockGitLab struct {
	mu          sync.Mutex
	hooks       map[int]map[string]any // id -> hook body
	nextHookID  int
	createBody  map[string]any
	deletedHook int
}

func newMockGitLab() *mockGitLab { return &mockGitLab{hooks: map[int]map[string]any{}} }

func (m *mockGitLab) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "botuser"})
	})
	mux.HandleFunc("/api/v4/projects", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 194, "path_with_namespace": "group/api", "visibility": "private", "default_branch": "main", "web_url": "https://gl/group/api"},
		})
	})
	// /api/v4/projects/{id}/hooks and /hooks/{hookID}
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{}) // GetHook probe: none yet
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m.createBody = body
			m.nextHookID++
			body["id"] = m.nextHookID
			m.hooks[m.nextHookID] = body
			_ = json.NewEncoder(w).Encode(body)
		}
	})
	mux.HandleFunc("/api/v4/projects/group%2Fapi/hooks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			m.mu.Lock()
			m.deletedHook++
			m.hooks = map[int]map[string]any{}
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return httptest.NewServer(mux)
}

func newForgeTestServer(t *testing.T) *Server {
	t.Helper()
	s := newOrgTestServer(t)
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	s.genericSecrets = secrets.NewMemoryGenericSecretStore()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	s.sealer = sealer
	s.forgeConnections = forge.NewMemoryConnectionStore()
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	s.forgeStates = newForgeStateStore(time.Minute)
	s.forgeOrchestrator = &forge.Orchestrator{
		Connections:  s.forgeConnections,
		Integrations: s.forgeIntegrations,
		Webhooks:     s.webhookConfigs,
		Secrets:      s.genericSecrets,
		Sealer:       sealer,
		Bots:         testForgeBotLookup,
		AdminFor:     s.forgeAdminFor, // the REAL gitlab client, pointed at the mock
		PublicURL:    "https://iterion.example.com",
	}
	return s
}

func testForgeBotLookup(botID string) (*bundle.ForgeRequirements, error) {
	if botID == "review-pr" {
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventPullRequest, bundle.ForgeEventPullRequestComment},
			TokenScopes: map[string]string{"pull_requests": "write"},
			Secret:      "forge_token",
		}, nil
	}
	return nil, nil
}

func forgeReq(ctx context.Context, method, path, body, teamID string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r = r.WithContext(ctx)
	r.SetPathValue("id", teamID)
	return r
}

func TestForgeIntegration_PATConnectEnableDisable(t *testing.T) {
	gl := newMockGitLab()
	srv := gl.server()
	defer srv.Close()

	s := newForgeTestServer(t)
	ctx := superAdminCtx()

	// 1. Connect via PAT (validated against the mock /user).
	body := `{"provider":"gitlab","mode":"pat","forge_base_url":"` + srv.URL + `","pat":"glpat-token"}`
	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/connections", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	var connResp forgeConnectResp
	if err := json.Unmarshal(w.Body.Bytes(), &connResp); err != nil {
		t.Fatal(err)
	}
	if connResp.Connection == nil || connResp.Connection.AccountLogin != "botuser" {
		t.Fatalf("connection not created with identity: %+v", connResp.Connection)
	}
	if connResp.Connection.SealedPayload != nil {
		t.Error("sealed payload must never be serialised")
	}
	connID := connResp.Connection.ID

	// 2. Enable review-pr on group/api → provisions the forge hook + config.
	enableBody := `{"connection_id":"` + connID + `","repo":"group/api","bot_ids":["review-pr"]}`
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots", enableBody, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Error("expected Created=true")
	}

	// the mock received a POST /hooks with the boolean event shape.
	gl.mu.Lock()
	cb := gl.createBody
	gl.mu.Unlock()
	if cb["merge_requests_events"] != true || cb["note_events"] != true {
		t.Errorf("forge hook body wrong: %v", cb)
	}
	if cb["token"] == nil || cb["token"] == "" {
		t.Error("forge hook got no secret token")
	}

	// the iterion webhook config is managed + scoped to the repo.
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProvisionedBy != "forge:"+connID {
		t.Errorf("provisioned_by = %q", cfg.ProvisionedBy)
	}
	// ManagedSecretID is an internal store ref (json:"-"), so read it from the
	// connection rather than the API response.
	provConn, err := s.forgeConnections.Get(ctx, connID)
	if err != nil {
		t.Fatal(err)
	}
	if provConn.ManagedSecretID == "" || cfg.SecretOverrides["forge_token"] != provConn.ManagedSecretID {
		t.Errorf("secret override not pinned to managed secret: override=%v managed=%q", cfg.SecretOverrides, provConn.ManagedSecretID)
	}

	// 3. A managed webhook cannot be deleted via the webhook CRUD (409).
	w = httptest.NewRecorder()
	delReq := forgeReq(ctx, "DELETE", "/api/teams/t1/webhooks/"+res.WebhookID, "", "t1")
	delReq.SetPathValue("webhook_id", res.WebhookID)
	s.handleDeleteWebhook(w, delReq)
	if w.Code != http.StatusConflict {
		t.Errorf("managed webhook delete should 409, got %d", w.Code)
	}

	// 4. List integrations.
	w = httptest.NewRecorder()
	s.handleListForgeRepoBots(w, forgeReq(ctx, "GET", "/api/teams/t1/forge/repo-bots", "", "t1"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "group/api") {
		t.Fatalf("list integrations: code=%d body=%s", w.Code, w.Body.String())
	}

	// 5. Disable → forge hook deleted, webhook config gone.
	w = httptest.NewRecorder()
	disReq := forgeReq(ctx, "DELETE", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, "", "t1")
	disReq.SetPathValue("integration_id", res.IntegrationID)
	s.handleDisableForgeRepoBots(w, disReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("disable: code=%d body=%s", w.Code, w.Body.String())
	}
	gl.mu.Lock()
	deleted := gl.deletedHook
	gl.mu.Unlock()
	if deleted != 1 {
		t.Errorf("forge hook not deleted on disable: %d", deleted)
	}
	if _, err := s.webhookConfigs.Get(context.Background(), res.WebhookID); err == nil {
		t.Error("webhook config should be gone after disable")
	}
}

func TestForgeConnect_RejectsBadProvider(t *testing.T) {
	s := newForgeTestServer(t)
	w := httptest.NewRecorder()
	s.handleConnectForge(w, forgeReq(superAdminCtx(), "POST", "/api/teams/t1/forge/connections", `{"provider":"bitbucket","mode":"pat","pat":"x"}`, "t1"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad provider should 400, got %d", w.Code)
	}
}

func TestForgePreview_FlagsBotsWithoutForgeBlock(t *testing.T) {
	s := newForgeTestServer(t)
	ctx := superAdminCtx()
	// seed a connection directly.
	sealed, _ := forge.SealPAT(s.sealer, "c1", "tok")
	_ = s.forgeConnections.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, AccountLogin: "u", SealedPayload: sealed})

	w := httptest.NewRecorder()
	s.handlePreviewForgeEnable(w, forgeReq(ctx, "GET", "/api/teams/t1/forge/repo-bots/preview?connection_id=c1&repo=group/api&bots=review-pr,ghost-bot", "", "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("preview: code=%d body=%s", w.Code, w.Body.String())
	}
	var p forgeEnablePreview
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.ForgeNativeEvents) == 0 {
		t.Error("expected native events for review-pr")
	}
	if len(p.Conflicts) != 1 || !strings.Contains(p.Conflicts[0], "ghost-bot") {
		t.Errorf("expected a conflict for ghost-bot, got %v", p.Conflicts)
	}
}
