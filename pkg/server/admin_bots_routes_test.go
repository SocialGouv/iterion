package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Platform bot overrides: the super-admin surface that makes the baked bot
// catalog a DB row instead of an image rebuild. These tests run the whole
// admission chain: authz, sentinel-tenant scoping, size limits, audit, and
// the launch-resolution precedence the override exists for.

// adminBotsServer boots the org test server with a bot-source store, an
// audit store, one team, and a super-admin + plain-editor identity.
func adminBotsServer(t *testing.T) (*Server, auth.Identity, auth.Identity) {
	t.Helper()
	s, editor, _ := newBotSourceTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	admin := auth.Identity{UserID: "root", IsSuperAdmin: true, TeamID: "t1"}
	return s, admin, editor
}

func adminBotsPut(s *Server, id auth.Identity, slug, body string) *httptest.ResponseRecorder {
	ctx := auth.WithIdentity(context.Background(), id)
	r := httptest.NewRequest("PUT", "/api/admin/bots/"+slug, strings.NewReader(body)).WithContext(ctx)
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	s.handleAdminPutPlatformBot(w, r)
	return w
}

func TestAdminBots_PlatformOverrideLifecycle(t *testing.T) {
	s, admin, _ := adminBotsServer(t)
	adminCtx := auth.WithIdentity(context.Background(), admin)

	// ---- push an override ----
	body, _ := json.Marshal(botSourcePutReq{Files: map[string]string{
		botsource.MainBotFile: testBotMain,
		"skills/probe.md":     "# probe skill",
	}})
	if w := adminBotsPut(s, admin, "reviewer", string(body)); w.Code != http.StatusOK {
		t.Fatalf("platform put = %d: %s", w.Code, w.Body.String())
	}

	// The row lives under the SENTINEL tenant, not the caller's team.
	pctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	bs, err := s.botSources.GetBySlug(pctx, botsource.PlatformTenantID, "reviewer")
	if err != nil {
		t.Fatalf("platform row missing: %v", err)
	}
	if bs.TenantID != botsource.PlatformTenantID {
		t.Fatalf("row tenant = %q, want the sentinel", bs.TenantID)
	}

	// ---- audit carries the content digest (the provenance record) ----
	// The insert is detached (goSafe), so poll briefly.
	var audited bool
	for i := 0; i < 50 && !audited; i++ {
		rows, lerr := s.auditStore.ListPlatform(context.Background(), audit.Page{})
		if lerr != nil {
			t.Fatalf("list audit: %v", lerr)
		}
		for _, ev := range rows {
			if ev.Action == "platform.bot.created" && ev.Meta["digest"] == botsource.Digest(bs.Files) {
				audited = true
			}
		}
		if !audited {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !audited {
		t.Fatal("no platform.bot.created audit row carrying the bundle digest")
	}

	// ---- every tenant-context-free launch surface resolves the override ----
	lb, err := s.resolveBotSource(adminCtx, "reviewer")
	if err != nil {
		t.Fatalf("resolveBotSource: %v", err)
	}
	defer lb.Cleanup()
	if lb.Origin != "platform" || !strings.Contains(lb.Source, "printf ok") {
		t.Fatalf("resolved = %+v, want the platform override", lb)
	}
	if lb.Ref == nil || lb.Ref.TenantID != botsource.PlatformTenantID || lb.Ref.Version != bs.Version {
		t.Fatalf("ref = %+v, want the stored row's identity", lb.Ref)
	}

	// ---- a TEAM row of the same slug outranks the platform one ----
	teamCtx := store.WithTenant(context.Background(), "t1")
	if _, err := s.botSources.Create(teamCtx, botsource.BotSource{
		TenantID: "t1", Slug: "reviewer",
		Files: map[string]string{botsource.MainBotFile: strings.Replace(testBotMain, "printf ok", "printf team", 1)},
	}); err != nil {
		t.Fatalf("seed team row: %v", err)
	}
	tlb, err := s.resolveBotTiered(context.Background(), "t1", "reviewer", "")
	if err != nil || tlb == nil || tlb.Origin != "team" || !strings.Contains(tlb.Source, "printf team") {
		t.Fatalf("team tier must outrank platform: %+v, %v", tlb, err)
	}
	tlb.Cleanup()

	// ---- delete reverts to "not found" (no baked catalog in this server) ----
	dr := httptest.NewRequest("DELETE", "/api/admin/bots/reviewer", nil).WithContext(adminCtx)
	dr.SetPathValue("slug", "reviewer")
	dw := httptest.NewRecorder()
	s.handleAdminDeletePlatformBot(dw, dr)
	if dw.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", dw.Code, dw.Body.String())
	}
	if _, err := s.resolveBotSource(context.Background(), "reviewer"); err == nil {
		t.Fatal("override must be gone after delete (fallback to baked = not-found here)")
	}
}

// newAdminBotsHTTPServer boots a cloud-shaped server through the real
// New()/routes() path with a bot-source store, so /api/admin/bots is
// reached through the production auth middleware.
func newAdminBotsHTTPServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	_, _, _, _, hs, adminTok, userTok := newAdminLLMServerWith(t, func(cfg *Config) {
		cfg.BotSources = botsource.NewMemoryStore()
	})
	return hs, adminTok, userTok
}

// The admin routes are registered behind requireSuperAdmin; this pins the
// middleware wiring through the PRODUCTION route stack (New with an auth
// service): a plain team member gets 403, a super-admin gets 200.
func TestAdminBots_SuperAdminOnly(t *testing.T) {
	hs, adminTok, userTok := newAdminBotsHTTPServer(t)
	for _, tc := range []struct {
		token string
		want  int
	}{
		{userTok, http.StatusForbidden},
		{adminTok, http.StatusOK},
	} {
		req, _ := http.NewRequest("GET", hs.URL+"/api/admin/bots", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("list as %q = %d, want %d", tc.token[:8], resp.StatusCode, tc.want)
		}
	}
}

// Oversized bundles fail loudly at admission — a row must never approach
// the Mongo document cap.
func TestAdminBots_SizeLimits(t *testing.T) {
	s, admin, _ := adminBotsServer(t)

	big := strings.Repeat("x", botsource.MaxBundleBytes+1)
	body, _ := json.Marshal(botSourcePutReq{Files: map[string]string{
		botsource.MainBotFile: testBotMain,
		"skills/huge.md":      big,
	}})
	w := adminBotsPut(s, admin, "huge", string(body))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "byte limit") {
		t.Fatalf("oversize put = %d %s, want 400 naming the byte limit", w.Code, w.Body.String())
	}

	files := map[string]string{botsource.MainBotFile: testBotMain}
	for i := 0; i <= botsource.MaxBundleFiles; i++ {
		files[fmt.Sprintf("skills/s%04d.md", i)] = "s"
	}
	body, _ = json.Marshal(botSourcePutReq{Files: files})
	w = adminBotsPut(s, admin, "many", string(body))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "file limit") {
		t.Fatalf("too-many-files put = %d %s, want 400 naming the file limit", w.Code, w.Body.String())
	}
}
