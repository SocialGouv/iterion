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
	"github.com/SocialGouv/iterion/pkg/cloudsched"
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
	// enterHook/releaseHook pin a provision mid-flight: when set, the hook
	// CREATE announces itself on enterHook and blocks on releaseHook. A test
	// racing two decisions can then hold one inside the orchestrator's forge
	// side effects while it drives the other.
	enterHook   chan struct{}
	releaseHook chan struct{}
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
		// Park BEFORE taking the lock so the test's other request is free to
		// touch the mock while this one is held.
		if r.Method == http.MethodPost && m.enterHook != nil {
			m.enterHook <- struct{}{}
			<-m.releaseHook
		}
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
		switch r.Method {
		case http.MethodDelete:
			m.mu.Lock()
			m.deletedHook++
			m.hooks = map[int]map[string]any{}
			m.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPut:
			// Re-provisioning an existing integration UPDATES the hook in
			// place rather than creating one; without this arm the client
			// decodes an empty 200 body and fails with EOF.
			m.mu.Lock()
			defer m.mu.Unlock()
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := m.nextHookID
			body["id"] = id
			m.hooks[id] = body
			_ = json.NewEncoder(w).Encode(body)
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
	switch botID {
	case "review-pr":
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventPullRequest, bundle.ForgeEventPullRequestComment},
			TokenScopes: map[string]string{"pull_requests": "write"},
			Secret:      "forge_token",
		}, nil
	case "dep-guard":
		// A SECOND provisionable bot, so a test can exercise ADDING one to a
		// repo that already carries another (the add-bots merge path).
		return &bundle.ForgeRequirements{
			Events:      []string{bundle.ForgeEventPullRequest},
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

// TestListTeamForgeRepos_Aggregator covers the RepoSwitcher data source:
// integration×connection join, URL derivation, deterministic order, and
// tenant isolation.
func TestListTeamForgeRepos_Aggregator(t *testing.T) {
	s := newForgeTestServer(t)
	ctx := superAdminCtx()
	bg := context.Background()

	mkConn := func(id, tenant string) {
		t.Helper()
		if err := s.forgeConnections.Create(bg, forge.Connection{
			ID: id, TenantID: tenant, Provider: forge.ProviderGitHub,
			Kind: forge.KindPAT, Status: forge.StatusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkInt := func(id, tenant, connID, repo string, bots []string) {
		t.Helper()
		if err := s.forgeIntegrations.Create(bg, forge.RepoIntegration{
			ID: id, TenantID: tenant, ConnectionID: connID,
			Provider: forge.ProviderGitHub, RepoFullName: repo, BotIDs: bots,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mkConn("c1", "t1")
	mkConn("c2", "t2")
	mkInt("i1", "t1", "c1", "org/zeta", []string{"review-pr"})
	mkInt("i2", "t1", "c1", "org/alpha", nil)
	mkInt("i3", "t2", "c2", "org/other", nil)

	w := httptest.NewRecorder()
	s.handleListTeamForgeRepos(w, forgeReq(ctx, "GET", "/api/teams/t1/forge/repos", "", "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Repos []forgeTeamRepo `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Repos) != 2 {
		t.Fatalf("want 2 repos for t1, got %d: %+v", len(resp.Repos), resp.Repos)
	}
	if resp.Repos[0].RepoFullName != "org/alpha" || resp.Repos[1].RepoFullName != "org/zeta" {
		t.Fatalf("want sorted [org/alpha org/zeta], got %+v", resp.Repos)
	}
	first := resp.Repos[0]
	if first.CloneURL != "https://github.com/org/alpha.git" || first.WebURL != "https://github.com/org/alpha" {
		t.Fatalf("URL derivation wrong: %+v", first)
	}
	if first.ConnectionStatus != "active" || first.ConnectionID != "c1" || first.IntegrationID != "i2" {
		t.Fatalf("join wrong: %+v", first)
	}
	if first.BotIDs == nil {
		t.Fatalf("bot_ids must be [] not null")
	}
	for _, r := range resp.Repos {
		if r.RepoFullName == "org/other" {
			t.Fatalf("tenant isolation broken: t2 repo leaked into t1: %+v", resp.Repos)
		}
	}

	// Empty tenant → 200 with [].
	w = httptest.NewRecorder()
	s.handleListTeamForgeRepos(w, forgeReq(ctx, "GET", "/api/teams/t9/forge/repos", "", "t9"))
	if w.Code != http.StatusOK {
		t.Fatalf("empty tenant code=%d", w.Code)
	}
	var empty struct {
		Repos []forgeTeamRepo `json:"repos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Repos == nil || len(empty.Repos) != 0 {
		t.Fatalf("want empty [] repos, got %+v", empty.Repos)
	}
}

func TestAppendQueryParam(t *testing.T) {
	cases := []struct{ in, key, val, want string }{
		{"/integrations/connect", "connected", "abc", "/integrations/connect?connected=abc"},
		{"/integrations/connect?step=2&provider=github", "connected", "c-1", "/integrations/connect?connected=c-1&provider=github&step=2"},
		{"/teams/t1", "installed", "x y", "/teams/t1?installed=x+y"},
		{"://bad url", "k", "v", "://bad url"},
	}
	for _, c := range cases {
		if got := appendQueryParam(c.in, c.key, c.val); got != c.want {
			t.Errorf("appendQueryParam(%q,%q,%q)=%q want %q", c.in, c.key, c.val, got, c.want)
		}
	}
}

// TestForgeConnectionHealth_PAT covers the health endpoint's base path
// (stored status + provisioned count, no live GitHub probe) and tenant
// isolation.
func TestForgeConnectionHealth_PAT(t *testing.T) {
	s := newForgeTestServer(t)
	ctx := superAdminCtx()
	bg := context.Background()

	if err := s.forgeConnections.Create(bg, forge.Connection{
		ID: "c1", TenantID: "t1", Provider: forge.ProviderGitLab,
		Kind: forge.KindPAT, Status: forge.StatusActive, AccountLogin: "botuser",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.forgeIntegrations.Create(bg, forge.RepoIntegration{
		ID: "i1", TenantID: "t1", ConnectionID: "c1",
		Provider: forge.ProviderGitLab, RepoFullName: "group/api",
	}); err != nil {
		t.Fatal(err)
	}

	req := forgeReq(ctx, "GET", "/api/teams/t1/forge/connections/c1/health", "", "t1")
	req.SetPathValue("conn_id", "c1")
	w := httptest.NewRecorder()
	s.handleForgeConnectionHealth(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var h forgeConnectionHealth
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if h.Status != "active" || h.Kind != "pat" || h.ProvisionedRepoCount != 1 {
		t.Fatalf("unexpected health: %+v", h)
	}

	// Cross-tenant access → 404 (not 403), matching forgeConnForTenant.
	req2 := forgeReq(ctx, "GET", "/api/teams/t2/forge/connections/c1/health", "", "t2")
	req2.SetPathValue("conn_id", "c1")
	w2 := httptest.NewRecorder()
	s.handleForgeConnectionHealth(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant health must 404, got %d", w2.Code)
	}
}

func TestScheduleRepoCandidates(t *testing.T) {
	sched := []cloudsched.ScheduledBot{
		{RepoURL: "https://github.com/SocialGouv/iterion-veille"}, // on host, new → candidate
		{RepoURL: "https://github.com/SocialGouv/iterion-veille"}, // dup → collapsed
		{RepoURL: "https://github.com/SocialGouv/iterion.git"},    // on host but in base → skip
		{RepoURL: "https://gitlab.com/acme/other"},                // different host → skip
		{RepoURL: ""}, // empty → skip
	}
	got := scheduleRepoCandidates(sched, []string{"iterion"}, "https://github.com")
	if len(got) != 1 || got[0] != "iterion-veille" {
		t.Fatalf("candidates = %v, want [iterion-veille]", got)
	}
	// No schedules on host → nothing added.
	if c := scheduleRepoCandidates(sched, []string{"iterion"}, "https://git.example.org"); len(c) != 0 {
		t.Fatalf("off-host candidates = %v, want none", c)
	}
}

func TestShortRepoName(t *testing.T) {
	cases := map[string]string{
		"SocialGouv/iterion-veille":                    "iterion-veille",
		"https://github.com/SocialGouv/iterion-veille": "iterion-veille",
		"https://github.com/SocialGouv/iterion.git":    "iterion",
		"iterion": "iterion",
		"":        "",
	}
	for in, want := range cases {
		if got := shortRepoName(in); got != want {
			t.Errorf("shortRepoName(%q) = %q, want %q", in, got, want)
		}
	}
}
