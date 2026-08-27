package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botsource"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/store"
)

// roleBots resolves the effective role→bot bindings: constants as defaults,
// the platform record field-by-field on top, effective on this replica
// immediately after a mutation (Invalidate).
func TestRoleBots_DefaultsAndOverride(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.BotRoles]()
	s := New(Config{SkipProjectRegistration: true, BotRolesSettings: st}, iterlog.New(iterlog.LevelError, nil))

	got := s.roleBots()
	if got.Reviewer != defaultWebhookBotReviewPR || got.Brancher != branchImproveBotID ||
		got.Implementer != featureDevBotID || got.ReviConverse != defaultWebhookBotReviConverse {
		t.Fatalf("defaults = %+v", got)
	}

	alt := "my-reviewer"
	if err := st.Put(context.Background(), platformcfg.BotRoles{Reviewer: &alt}); err != nil {
		t.Fatal(err)
	}
	s.botRoles.Invalidate()
	got = s.roleBots()
	if got.Reviewer != "my-reviewer" {
		t.Fatalf("override not applied: %+v", got)
	}
	// The other roles keep their defaults — field-by-field, never wholesale.
	if got.Brancher != branchImproveBotID {
		t.Fatalf("unrelated role clobbered: %+v", got)
	}
}

// The PUT route applies merge semantics (absent field keeps its stored
// state, explicit null clears), validates ids, and echoes effective+origin.
func TestAdminBotRoles_PutMergeAndOrigin(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.BotRoles]()
	s := New(Config{SkipProjectRegistration: true, BotRolesSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})

	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/bot-roles", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutBotRoles(w, r)
		return w
	}

	if w := put(`{"reviewer":"alt-reviewer"}`); w.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", w.Code, w.Body.String())
	}
	if w := put(`{"brancher":"alt-brancher"}`); w.Code != http.StatusOK {
		t.Fatalf("merge put = %d: %s", w.Code, w.Body.String())
	}
	rec, _ := st.Get(context.Background())
	if rec == nil || rec.Reviewer == nil || *rec.Reviewer != "alt-reviewer" || rec.Brancher == nil {
		t.Fatalf("merge semantics lost a field: %+v", rec)
	}

	// Explicit null clears one override, keeping the other.
	if w := put(`{"reviewer":null}`); w.Code != http.StatusOK {
		t.Fatalf("clear put = %d: %s", w.Code, w.Body.String())
	}
	rec, _ = st.Get(context.Background())
	if rec.Reviewer != nil || rec.Brancher == nil {
		t.Fatalf("null-clear semantics wrong: %+v", rec)
	}

	// Invalid ids are refused before any write.
	if w := put(`{"implementer":"Not A Slug"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id = %d, want 400", w.Code)
	}
	if w := put(`{"unknown_role":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", w.Code)
	}

	// GET echoes effective + origin.
	gr := httptest.NewRequest("GET", "/api/admin/settings/bot-roles", nil).WithContext(admin)
	gw := httptest.NewRecorder()
	s.handleAdminGetBotRoles(gw, gr)
	var resp struct {
		Effective effectiveBotRoles `json:"effective"`
		Origin    string            `json:"origin"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Origin != "db" || resp.Effective.Brancher != "alt-brancher" || resp.Effective.Reviewer != defaultWebhookBotReviewPR {
		t.Fatalf("get echo = %+v", resp)
	}
}

// The sandbox family: blank overrides refused, effective value trimmed,
// clearing falls back to "" (inherit env/built-in).
func TestAdminSandboxSettings(t *testing.T) {
	st := platformcfg.NewMemoryStore[platformcfg.Sandbox]()
	s := New(Config{SkipProjectRegistration: true, SandboxSettings: st}, iterlog.New(iterlog.LevelError, nil))
	admin := auth.WithIdentity(context.Background(), auth.Identity{UserID: "root", IsSuperAdmin: true})

	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/admin/settings/sandbox", strings.NewReader(body)).WithContext(admin)
		w := httptest.NewRecorder()
		s.handleAdminPutSandboxSettings(w, r)
		return w
	}
	if w := put(`{"default_image":"  "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("blank image = %d, want 400 (clear = null, never empty)", w.Code)
	}
	if w := put(`{"default_image":"ghcr.io/x/img@sha256:abc"}`); w.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", w.Code, w.Body.String())
	}
	s.sandboxCfg.Invalidate()
	if got := s.effectiveSandboxImageSetting(context.Background()); got != "ghcr.io/x/img@sha256:abc" {
		t.Fatalf("effective = %q", got)
	}
	if w := put(`{"default_image":null}`); w.Code != http.StatusOK {
		t.Fatalf("clear = %d", w.Code)
	}
	s.sandboxCfg.Invalidate()
	if got := s.effectiveSandboxImageSetting(context.Background()); got != "" {
		t.Fatalf("cleared effective = %q, want empty (inherit)", got)
	}
}

// effectiveEntriesWithSchema overlays the platform tier onto the baked
// catalog: a same-slug override REPLACES the baked entry (its metadata is
// what every tenant must see), a new-slug platform bot is appended.
func TestEffectiveEntries_PlatformOverlay(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)

	// Bake a catalog bot on the FS.
	dir := t.TempDir()
	botDir := filepath.Join(dir, "bots", "reviewer")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "main.bot"), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, "manifest.yaml"), []byte("name: reviewer\ndisplay_name: Baked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots = BotsConfig{Paths: []string{filepath.Join(dir, "bots")}}

	// Platform override of the same slug + a brand-new platform bot.
	pctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	for slug, display := range map[string]string{"reviewer": "Overridden", "brand-new": "Fresh"} {
		if _, err := s.botSources.Create(pctx, botsource.BotSource{
			TenantID: botsource.PlatformTenantID, Slug: slug,
			Files: map[string]string{
				botsource.MainBotFile: testBotMain,
				"manifest.yaml":       "name: " + slug + "\ndisplay_name: " + display + "\n",
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.invalidatePlatformBots()

	entries, err := s.effectiveEntriesWithSchema()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, e := range entries {
		byName[e.Name] = e.DisplayName
	}
	if byName["reviewer"] != "Overridden" {
		t.Fatalf("same-slug override must replace the baked entry, got %q", byName["reviewer"])
	}
	if byName["brand-new"] != "Fresh" {
		t.Fatalf("new-slug platform bot must be appended, got %v", byName)
	}
}
