package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// flakyOAuthApps answers Get with a store failure while it is armed.
type flakyOAuthApps struct {
	forge.OAuthAppStore
	fail bool
}

func (f *flakyOAuthApps) Get(ctx context.Context, id string) (forge.ForgeOAuthApp, error) {
	if f.fail {
		return forge.ForgeOAuthApp{}, errors.New("store unavailable")
	}
	return f.OAuthAppStore.Get(ctx, id)
}

// A connection that records its OAuth app refreshes through THAT app and
// no other: the refresh token was issued to its client, and a refresh signed
// by a sibling app is refused by the forge and would mark a healthy
// connection needs_reauth. So a store blip yields no client for the tick,
// never the instance's other app.
func TestForgeOAuthAppForConnection_PinnedAppResolvesByIDOnly(t *testing.T) {
	s := newForgeOAuthAppTestServer(t)
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, forgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"gitlab","forge_base_url":"https://pic.example","client_id":"platform-cid","client_secret":"ps"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("platform register: %d %s", w.Code, w.Body.String())
	}
	var platform forge.ForgeOAuthApp
	decodeBody(t, w, &platform)
	w = httptest.NewRecorder()
	s.handleRegisterForgeOAuthApp(w, oauthAppReq(ctx, "POST", "/api/teams/t1/forge/oauth-apps",
		`{"provider":"gitlab","mode":"manual","forge_base_url":"https://pic.example","client_id":"team-cid","client_secret":"ts"}`, "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("team register: %d %s", w.Code, w.Body.String())
	}

	pinned := forge.Connection{ID: "c1", TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindOAuthApp, ForgeBaseURL: "https://pic.example", OAuthAppID: platform.ID}
	clientIDOf := func(x forge.OAuthExchanger) string {
		u := x.AuthorizeURL("https://iterion.test/cb", "st", "", nil)
		for _, kv := range strings.Split(strings.SplitN(u, "?", 2)[1], "&") {
			if strings.HasPrefix(kv, "client_id=") {
				return strings.TrimPrefix(kv, "client_id=")
			}
		}
		return ""
	}
	x, ok := s.forgeOAuthAppForConnection(store.WithTenant(context.Background(), "t1"), pinned)
	if !ok || clientIDOf(x) != "platform-cid" {
		t.Fatalf("pinned connection resolved client %q ok=%v, want the platform app it was made through, not the team's newer one", clientIDOf(x), ok)
	}
	// A connection that predates the pin keeps the instance resolution (the team's app first).
	legacy := pinned
	legacy.OAuthAppID = ""
	if x, ok := s.forgeOAuthAppForConnection(store.WithTenant(context.Background(), "t1"), legacy); !ok || clientIDOf(x) != "team-cid" {
		t.Errorf("legacy connection resolved %q ok=%v, want the team's app", clientIDOf(x), ok)
	}

	flaky := &flakyOAuthApps{OAuthAppStore: s.forgeOAuthApps, fail: true}
	s.forgeOAuthApps = flaky
	if x, ok := s.forgeOAuthAppForConnection(store.WithTenant(context.Background(), "t1"), pinned); ok {
		t.Fatalf("a store blip resolved client %q: a pinned connection must get NO client rather than another app's", clientIDOf(x))
	}
	flaky.fail = false
	pinned.OAuthAppID = "gone"
	if _, ok := s.forgeOAuthAppForConnection(store.WithTenant(context.Background(), "t1"), pinned); ok {
		t.Fatal("a connection whose app is gone must not fall back to the instance's app")
	}
}

// failingIntegrations refuses every tenant listing.
type failingIntegrations struct {
	forge.RepoIntegrationStore
}

func (failingIntegrations) ListByTenant(context.Context, string) ([]forge.RepoIntegration, error) {
	return nil, errors.New("store unavailable")
}

// The merge path opens the run's pinned forge token only after the host
// check; a listing it cannot read is a refusal, not a skipped check.
func TestForgeTokenForRun_FailsWhenTheIntegrationListingFails(t *testing.T) {
	s := newForgeTestServer(t)
	s.forgeIntegrations = failingIntegrations{RepoIntegrationStore: s.forgeIntegrations}
	s.forgeOrchestrator.Integrations = s.forgeIntegrations
	_, err := s.forgeTokenForRun(context.Background(), &store.Run{
		ID: "run-1", TenantID: "t1", RepoURL: "https://evil.example/t/r.git",
		SecretOverrides: map[string]string{"forge_token": "sec-unknown"},
	})
	if err == nil || !strings.Contains(err.Error(), "list forge integrations") {
		t.Fatalf("err = %v, want the listing failure to refuse the merge", err)
	}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
}
