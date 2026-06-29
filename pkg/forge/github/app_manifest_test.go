package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConvertManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/app-manifests/code123/conversions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 42, "slug": "iterion-forge-abc", "client_id": "Iv1.cid", "client_secret": "ghsec",
		})
	}))
	defer srv.Close()

	// srv.URL is a non-github.com host → APIBaseFor appends /api/v3.
	conv, err := ConvertManifest(context.Background(), srv.Client(), srv.URL, "code123")
	if err != nil {
		t.Fatal(err)
	}
	if conv.ClientID != "Iv1.cid" || conv.ClientSecret != "ghsec" || conv.ID != 42 {
		t.Fatalf("conv = %+v", conv)
	}
}

func TestConvertManifest_ExpiredCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	if _, err := ConvertManifest(context.Background(), srv.Client(), srv.URL, "stale"); err == nil {
		t.Fatal("expected an error for an expired/invalid code")
	}
}

func TestBuildAppManifest(t *testing.T) {
	m := BuildAppManifest("iterion-forge-x", "https://it", "https://it/cb")
	if m.RedirectURL != "https://it/cb" || m.Public {
		t.Fatalf("manifest = %+v", m)
	}
	// The OAuth user-authorization callback must be baked in, else "connect via
	// OAuth" fails with GitHub's "must be configured with a callback URL".
	if len(m.CallbackURLs) != 1 || m.CallbackURLs[0] != "https://it/api/forge/oauth/callback" {
		t.Fatalf("callback_urls = %v, want [https://it/api/forge/oauth/callback]", m.CallbackURLs)
	}
	// setup_url makes GitHub redirect after install so the github_app connection
	// is created (without it GitHub stays put and iterion never sees the install).
	if m.SetupURL != "https://it/api/forge/github/app/callback" {
		t.Fatalf("setup_url = %q, want https://it/api/forge/github/app/callback", m.SetupURL)
	}
	// Least-privilege: webhooks via repository_hooks (the correct GitHub App perm),
	// NOT administration — which would grant repo deletion/settings/teams and is
	// the wrong permission for webhook management on an installation token.
	if m.DefaultPermissions["repository_hooks"] != "write" {
		t.Fatalf("missing repository_hooks:write perm: %+v", m.DefaultPermissions)
	}
	if _, ok := m.DefaultPermissions["administration"]; ok {
		t.Fatalf("administration must NOT be requested (over-privileged): %+v", m.DefaultPermissions)
	}
	// contents:write is required for bots to push branches/commits (read alone
	// would let a feature/review bot read but never open a real PR).
	if m.DefaultPermissions["contents"] != "write" {
		t.Fatalf("contents perm must be write (push), got %q", m.DefaultPermissions["contents"])
	}
	// The App-level webhook must be disabled — iterion creates per-repo hooks.
	if active, _ := m.HookAttributes["active"].(bool); active {
		t.Fatal("hook attributes should disable the app-level webhook")
	}
}
