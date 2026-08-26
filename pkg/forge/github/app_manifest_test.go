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
	// Security-read is opt-in: the baseline must never request it.
	if _, ok := m.DefaultPermissions["vulnerability_alerts"]; ok {
		t.Fatalf("vulnerability_alerts must NOT be in the baseline: %+v", m.DefaultPermissions)
	}
}

func TestBuildAppManifest_AllowSecurityRead(t *testing.T) {
	m := BuildAppManifest("iterion-forge-x", "https://it", "https://it/cb", AppManifestOptions{AllowSecurityRead: true})
	if m.DefaultPermissions["vulnerability_alerts"] != "read" {
		t.Fatalf("vulnerability_alerts = %q, want read: %+v", m.DefaultPermissions["vulnerability_alerts"], m.DefaultPermissions)
	}
}

func TestSecurityReadPermissions(t *testing.T) {
	p := SecurityReadInstallationPermissions()
	if p["vulnerability_alerts"] != "read" || p["metadata"] != "read" || len(p) != 2 {
		t.Fatalf("security-read profile = %v", p)
	}
	// The profile stays disjoint from the runtime baseline — a run's forge
	// token must never quietly gain alert access.
	for name := range RuntimeInstallationPermissions() {
		if name == "metadata" {
			continue // shared mandatory baseline
		}
		if _, ok := p[name]; ok {
			t.Fatalf("security-read profile leaks runtime permission %q", name)
		}
	}
	if got := MissingSecurityPermissions(map[string]string{"contents": "write"}); len(got) != 1 || got[0] != "vulnerability_alerts" {
		t.Fatalf("MissingSecurityPermissions = %v", got)
	}
	if got := MissingSecurityPermissions(map[string]string{"vulnerability_alerts": "read"}); got != nil {
		t.Fatalf("MissingSecurityPermissions = %v, want nil", got)
	}
	// Unknown grant set (pre-dates the field): absence of data is not
	// evidence of a gap.
	if got := MissingSecurityPermissions(nil); got != nil {
		t.Fatalf("MissingSecurityPermissions(nil) = %v, want nil", got)
	}
}

// A watch-only App is the one shape that may be installed on ALL repositories
// of an org without handing out write, so its permission set must be exactly
// the security-read profile — never the runtime baseline plus an extra grant.
func TestBuildAppManifest_SecurityReadOnly(t *testing.T) {
	m := BuildAppManifest("iterion-watch-x", "https://it", "https://it/cb",
		AppManifestOptions{SecurityReadOnly: true})
	want := SecurityReadInstallationPermissions()
	if len(m.DefaultPermissions) != len(want) {
		t.Fatalf("watch-only permissions = %v, want exactly %v", m.DefaultPermissions, want)
	}
	for name, level := range want {
		if m.DefaultPermissions[name] != level {
			t.Fatalf("watch-only %q = %q, want %q (%v)", name, m.DefaultPermissions[name], level, m.DefaultPermissions)
		}
	}
	// The callback wiring is shared with the runtime shape: a watch-only App
	// still has to complete the same install → connection round-trip.
	if m.SetupURL != "https://it/api/forge/github/app/callback" {
		t.Fatalf("SetupURL = %q", m.SetupURL)
	}
}

// The write-granting options must not be able to widen a watch-only App —
// that combination is exactly what the shape exists to make impossible.
func TestBuildAppManifest_SecurityReadOnlyOverridesWriteOptions(t *testing.T) {
	m := BuildAppManifest("iterion-watch-x", "https://it", "https://it/cb",
		AppManifestOptions{
			SecurityReadOnly:  true,
			AllowRepoCreation: true,
			AllowAppDelivery:  true,
		})
	for _, forbidden := range []string{"administration", "contents", "pull_requests", "repository_hooks", "workflows", "packages", "statuses", "issues"} {
		if lvl, ok := m.DefaultPermissions[forbidden]; ok {
			t.Fatalf("watch-only App carries %q=%q — write options widened it: %v", forbidden, lvl, m.DefaultPermissions)
		}
	}
}

// iterion POSTs the manifest through the operator's BROWSER, so the App GitHub
// actually created is the only evidence of what was registered — the request
// iterion built is not. A watch-only App is the one shape whose entire value
// rests on its permissions, and an org owner is asked to install it on every
// repository, so the claim has to be confronted with the response.
func TestManifestConversion_IsSecurityReadOnly(t *testing.T) {
	cases := []struct {
		name  string
		perms map[string]string
		want  bool
	}{
		{"exact profile", map[string]string{"metadata": "read", "vulnerability_alerts": "read"}, true},
		{"carries write too", map[string]string{"metadata": "read", "vulnerability_alerts": "read", "contents": "write"}, false},
		{"missing the alerts grant", map[string]string{"metadata": "read"}, false},
		{"right names, wrong level", map[string]string{"metadata": "read", "vulnerability_alerts": "write"}, false},
		{"empty (GitHub said nothing)", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ManifestConversion{Permissions: tc.perms}
			if got := c.IsSecurityReadOnly(); got != tc.want {
				t.Fatalf("IsSecurityReadOnly(%v) = %v, want %v", tc.perms, got, tc.want)
			}
		})
	}
}
