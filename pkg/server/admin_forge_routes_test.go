package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func adminForgeReq(ctx context.Context, method, path, body, appID string) *http.Request {
	r := forgeReq(ctx, method, path, body, "")
	if appID != "" {
		r.SetPathValue("app_id", appID)
	}
	return r
}

func TestAdminForgeOAuthApps_PlatformRegistrationServesEveryTeam(t *testing.T) {
	s := newForgeOAuthAppTestServer(t)
	s.forgeConnections = forge.NewMemoryConnectionStore()
	ctx := superAdminCtx()

	for _, tc := range []struct{ name, body, want string }{
		{"missing secret", `{"provider":"gitlab","client_id":"cid"}`, "client_id and client_secret"},
		{"bad provider", `{"provider":"bitbucket","client_id":"cid","client_secret":"x"}`, "unknown provider"},
		{"hook base with a path", `{"provider":"gitlab","client_id":"cid","client_secret":"x","webhook_base_url":"https://h/api"}`, "scheme+host only"},
		{"hook base without a scheme", `{"provider":"gitlab","client_id":"cid","client_secret":"x","webhook_base_url":"iterion.fabrique.example"}`, "absolute http(s) URL"},
	} {
		w := httptest.NewRecorder()
		s.handleAdminRegisterForgeOAuthApp(w, adminForgeReq(ctx, "POST", "/api/admin/forge/oauth-apps", tc.body, ""))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s: code=%d body=%s", tc.name, w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, adminForgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"gitlab","forge_base_url":"https://pic.example/","client_id":"cid","client_secret":"csecret","webhook_base_url":"https://iterion.fabrique.example"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("register: code=%d body=%s", w.Code, w.Body.String())
	}
	var app forge.ForgeOAuthApp
	_ = json.Unmarshal(w.Body.Bytes(), &app)
	if app.TenantID != secrets.PlatformTenantID || !app.Platform || app.ForgeBaseURL != "https://pic.example" || app.WebhookBaseURL != "https://iterion.fabrique.example" {
		t.Fatalf("app = %+v", app)
	}
	if strings.Contains(w.Body.String(), "csecret") {
		t.Fatal("the secret leaked into the response")
	}

	// The admin listing and the TEAM listing both carry it, flagged.
	w = httptest.NewRecorder()
	s.handleAdminListForgeOAuthApps(w, adminForgeReq(ctx, "GET", "/api/admin/forge/oauth-apps", "", ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"platform":true`) {
		t.Fatalf("admin list: code=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleListForgeOAuthApps(w, oauthAppReq(ctx, "GET", "/api/teams/t1/forge/oauth-apps", "", "t1", ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"platform":true`) || !strings.Contains(w.Body.String(), app.ID) {
		t.Fatalf("team list: code=%d body=%s", w.Code, w.Body.String())
	}

	// Resolution: the platform app answers for any team on that instance…
	got, ok := s.forgeOAuthAppRecordFor(context.Background(), "t1", forge.ProviderGitLab, "pic.example")
	if !ok || got.ID != app.ID || !got.Platform {
		t.Fatalf("record for t1 = %+v ok=%v", got, ok)
	}
	if _, ok := s.forgeOAuthAppFor(context.Background(), "t2", forge.ProviderGitLab, "https://pic.example"); !ok {
		t.Fatal("the OAuth client must build for another team too")
	}
	if _, ok := s.forgeOAuthAppRecordFor(context.Background(), "t1", forge.ProviderGitLab, "https://other.example"); ok {
		t.Fatal("another instance must not resolve")
	}
	// …and a team's own app on the same instance wins over it.
	w = httptest.NewRecorder()
	s.handleRegisterForgeOAuthApp(w, oauthAppReq(ctx, "POST", "/api/teams/t1/forge/oauth-apps",
		`{"provider":"gitlab","mode":"manual","forge_base_url":"https://pic.example","client_id":"team-cid","client_secret":"team-secret"}`, "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("team register: code=%d body=%s", w.Code, w.Body.String())
	}
	got, ok = s.forgeOAuthAppRecordFor(context.Background(), "t1", forge.ProviderGitLab, "https://pic.example")
	if !ok || got.Platform || got.ClientID != "team-cid" {
		t.Fatalf("record for t1 after a team app = %+v", got)
	}
	if got, ok := s.forgeOAuthAppRecordFor(context.Background(), "t2", forge.ProviderGitLab, "https://pic.example"); !ok || got.ID != app.ID {
		t.Fatalf("t2 must still get the platform app: %+v", got)
	}
	// By id: the platform app resolves for any team, a team's app only for its own.
	if _, ok, err := s.forgeOAuthAppRecordByID(context.Background(), "t2", app.ID); !ok || err != nil {
		t.Errorf("platform app by id for t2: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.forgeOAuthAppRecordByID(context.Background(), "t2", got.ID); got.ID != app.ID && (ok || err != nil) {
		t.Errorf("a team app must not resolve by id for another team: ok=%v err=%v", ok, err)
	}

	// Deleting while a connection (of any team) authenticates through it is refused.
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{ID: "c1", TenantID: "t9", Provider: forge.ProviderGitLab, Kind: forge.KindOAuthApp, OAuthAppID: app.ID, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleAdminDeleteForgeOAuthApp(w, adminForgeReq(ctx, "DELETE", "/api/admin/forge/oauth-apps/"+app.ID, "", app.ID))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "1 connection(s) across 1 team(s)") {
		t.Fatalf("delete in use: code=%d body=%s", w.Code, w.Body.String())
	}
	if err := s.forgeConnections.Delete(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	// A team's app is not reachable through the platform route.
	w = httptest.NewRecorder()
	s.handleAdminDeleteForgeOAuthApp(w, adminForgeReq(ctx, "DELETE", "/api/admin/forge/oauth-apps/"+got.ID, "", got.ID))
	if w.Code != http.StatusNotFound {
		t.Fatalf("delete a team app via the platform route: code=%d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleAdminDeleteForgeOAuthApp(w, adminForgeReq(ctx, "DELETE", "/api/admin/forge/oauth-apps/"+app.ID, "", app.ID))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s", w.Code, w.Body.String())
	}
	if _, ok := s.forgeOAuthAppRecordFor(context.Background(), "t2", forge.ProviderGitLab, "https://pic.example"); ok {
		t.Fatal("the platform app still resolves after its deletion")
	}
}

// The routes exist and sit behind the super-admin gate: an unauthenticated
// request answers 401 on each of them (never 404 or 405), and the 403 for an
// authenticated non-admin is requireSuperAdmin's own contract, pinned by the
// admin LLM credential tests on the same middleware.
func TestAdminForgeOAuthApps_RoutesAreGated(t *testing.T) {
	s := newForgeOAuthAppTestServer(t)
	s.registerAdminForgeRoutes()
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/admin/forge/oauth-apps"},
		{"POST", "/api/admin/forge/oauth-apps"},
		{"DELETE", "/api/admin/forge/oauth-apps/app-1"},
	} {
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: code=%d, want 401 (registered and auth-gated)", rt.method, rt.path, w.Code)
		}
	}
	_ = auth.Identity{} // the gate reads a signed identity; see admin_llm_routes_test for the 403
}
