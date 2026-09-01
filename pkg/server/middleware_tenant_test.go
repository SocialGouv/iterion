package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A team-less authenticated identity used to sail through requireAuth
// and reach the Mongo store with an EMPTY tenant in ctx — the store's
// fail-closed guard then panicked on every request (a recovered 500,
// Sentry ITERION-13/-1W/-1Z, 1800+ events). The choke point refuses it
// with an explicit 403 on tenant-scoped routes, and still admits the
// tenancy-management surfaces the user needs to GET a team.
func TestRequireAuthRefusesTeamlessOnTenantScopedRoutes(t *testing.T) {
	t.Parallel()
	secret := base64.StdEncoding.EncodeToString(make([]byte, 32))
	signer, err := auth.NewJWTSigner(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{signer: signer}

	mint := func(teamID string) string {
		tok, _, err := signer.IssueAccess(auth.Identity{UserID: "u1", Email: "u@x", TeamID: teamID})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	var sawTenant string
	var sawTenantOK bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTenant, sawTenantOK = store.TenantFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	h := s.requireAuth(inner)

	call := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// Team-less on a tenant-scoped route: explicit 403, handler never runs.
	sawTenantOK = false
	if w := call("/api/runs", mint("")); w.Code != http.StatusForbidden {
		t.Fatalf("teamless GET /api/runs = %d, want 403 — an empty tenant in ctx panics the mongo guard as a silent 500", w.Code)
	}
	if sawTenantOK {
		t.Fatal("handler ran despite the refusal")
	}

	// Team-less on the tenancy-management surface: admitted, NO store
	// tenant stamped (the guard stays the backstop for a stray
	// tenant-scoped call).
	if w := call("/api/auth/me", mint("")); w.Code != http.StatusNoContent {
		t.Fatalf("teamless GET /api/auth/me = %d, want 204 — the user must be able to reach switch-team", w.Code)
	}
	if sawTenantOK {
		t.Fatalf("teamless admitted request carried a store tenant %q — must carry none", sawTenant)
	}

	// Teamful identity: unchanged — tenant stamped, handler runs.
	if w := call("/api/runs", mint("team-9")); w.Code != http.StatusNoContent {
		t.Fatalf("teamful GET /api/runs = %d, want 204", w.Code)
	}
	if !sawTenantOK || sawTenant != "team-9" {
		t.Fatalf("teamful request tenant = (%q, %v), want (team-9, true)", sawTenant, sawTenantOK)
	}
}

// The allowlist is segment-exact: "/api/me/" must not admit
// "/api/memory/…" (tenant-scoped workspace memory) — a bare prefix
// match would silently reopen the empty-tenant hole there.
func TestTenantFreePathIsSegmentExact(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"/api/me":              true,
		"/api/me/oauth/claude": true,
		"/api/memory/usage":    false,
		"/api/memory":          false,
		"/api/orgs":            true,
		"/api/orgsomething":    false,
		"/api/teams/t1/audit":  true,
		"/api/teamsync":        false,
		"/api/auth/me":         true,
		"/api/admin/llm":       true,
		"/api/administrivia":   false,
		"/api/runs":            false,
	}
	for p, want := range cases {
		if got := tenantFreePath(p); got != want {
			t.Errorf("tenantFreePath(%q) = %v, want %v", p, got, want)
		}
	}
}
