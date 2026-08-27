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

	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"

	"github.com/SocialGouv/iterion/pkg/botregistry"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func writeBotFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const testBotSrc = `## ---
## name: feature_dev
## description: Plans and ships a feature.
## triggers: [feature]
## ---

workflow w:
  vars:
    workspace_dir: string = "/tmp"
    loop_cap: int = 5
    mode: string [enum: "autonomous", "interview"] = "autonomous"
  agent a:
    model: "test"
  a -> done

agent a:
  model: "test"
`

func TestBotsListRoute(t *testing.T) {
	botregistry.ClearSchemaCache()
	dir := t.TempDir()
	writeBotFile(t, filepath.Join(dir, "feature_dev.bot"), testBotSrc)

	srv := New(Config{
		DisableAuth: true,
		Bots:        BotsConfig{Paths: []string{dir}},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bots", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Bots []botregistry.EntryWithSchema `json:"bots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(resp.Bots) != 1 {
		t.Fatalf("got %d bots; body=%s", len(resp.Bots), rec.Body.String())
	}
	b := resp.Bots[0]
	if b.Name != "feature_dev" {
		t.Errorf("Name = %q", b.Name)
	}
	if b.Vars == nil || len(b.Vars.Fields) == 0 {
		t.Errorf("expected vars schema in list payload; got %+v", b)
	}
	// The enum constraint rides the schema surface as `"enum"` — the key
	// the studio's launch form reads to render a select instead of text.
	var modeField *botregistry.VarField
	for _, f := range b.Vars.Fields {
		if f.Name == "mode" {
			modeField = f
		}
	}
	if modeField == nil {
		t.Fatalf("mode var missing from schema payload; body=%s", rec.Body.String())
	}
	if len(modeField.Enum) != 2 || modeField.Enum[0] != "autonomous" || modeField.Enum[1] != "interview" {
		t.Errorf("mode.Enum = %v, want [autonomous interview]", modeField.Enum)
	}
	if !strings.Contains(rec.Body.String(), `"enum"`) {
		t.Errorf("wire payload missing \"enum\" key; body=%s", rec.Body.String())
	}
}

func TestBotsListRoute_DisplayName(t *testing.T) {
	botregistry.ClearSchemaCache()
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "whats-next")
	writeBotFile(t, filepath.Join(bundleDir, "manifest.yaml"), `name: whats-next
display_name: Nexie
description: Orchestrator bot.
launch:
  primary: [workspace_dir, loop_cap]
  hidden: [internal_var]
`)
	writeBotFile(t, filepath.Join(bundleDir, "main.bot"), testBotSrc)

	srv := New(Config{
		DisableAuth: true,
		Bots:        BotsConfig{Paths: []string{dir}},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bots", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Bots []botregistry.EntryWithSchema `json:"bots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(resp.Bots) != 1 {
		t.Fatalf("got %d bots; body=%s", len(resp.Bots), rec.Body.String())
	}
	if resp.Bots[0].DisplayName != "Nexie" {
		t.Errorf("DisplayName = %q, want Nexie (the /bots payload must expose the manifest persona)", resp.Bots[0].DisplayName)
	}
	// The manifest launch: block flows onto the bot entry so the studio
	// launch form can order primary vars and prune hidden ones.
	launch := resp.Bots[0].Launch
	if launch == nil {
		t.Fatalf("expected launch hints in /bots payload; body=%s", rec.Body.String())
	}
	if len(launch.Primary) != 2 || launch.Primary[0] != "workspace_dir" || launch.Primary[1] != "loop_cap" {
		t.Errorf("launch.primary = %v, want [workspace_dir loop_cap] in manifest order", launch.Primary)
	}
	if len(launch.Hidden) != 1 || launch.Hidden[0] != "internal_var" {
		t.Errorf("launch.hidden = %v, want [internal_var]", launch.Hidden)
	}
	if !strings.Contains(rec.Body.String(), `"launch"`) {
		t.Errorf("wire payload missing \"launch\" key; body=%s", rec.Body.String())
	}
}

func TestBotsGetRoute(t *testing.T) {
	botregistry.ClearSchemaCache()
	dir := t.TempDir()
	writeBotFile(t, filepath.Join(dir, "feature_dev.bot"), testBotSrc)

	srv := New(Config{
		DisableAuth: true,
		Bots:        BotsConfig{Paths: []string{dir}},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bots/feature_dev", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var b botregistry.EntryWithSchema
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if b.Name != "feature_dev" {
		t.Errorf("Name = %q", b.Name)
	}
	if b.Vars == nil || len(b.Vars.Fields) != 3 {
		t.Fatalf("expected 3 vars; got %+v", b.Vars)
	}
}

func TestBotsGetRoute_NotFound(t *testing.T) {
	botregistry.ClearSchemaCache()
	dir := t.TempDir()
	srv := New(Config{
		DisableAuth: true,
		Bots:        BotsConfig{Paths: []string{dir}},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bots/ghost", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
}

const botPutStub = "agent a:\n  model: \"test\"\n"

// botPutFixture builds a workspace with an editable feature_dev bundle
// plus a whats-next bundle carrying the catalog template, so PUT can be
// observed to both persist the manifest and regenerate the catalog.
func botPutFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeBotFile(t, filepath.Join(dir, "bots", "feature_dev", "manifest.yaml"),
		"name: feature_dev\ndisplay_name: Featurly\ndescription: Ships a feature.\n")
	writeBotFile(t, filepath.Join(dir, "bots", "feature_dev", "main.bot"), botPutStub)
	writeBotFile(t, filepath.Join(dir, "bots", "whats-next", "manifest.yaml"),
		"name: whats-next\ndisplay_name: Nexie\ndescription: Orchestrator.\n")
	writeBotFile(t, filepath.Join(dir, "bots", "whats-next", "main.bot"), botPutStub)
	writeBotFile(t, filepath.Join(dir, "bots", "whats-next", "iterion-bot-catalog-static.md"),
		"---\nname: iterion-bot-catalog\n---\nPREAMBLE\n\n<!-- ITERION:CATALOG:GENERATED:BEGIN -->\n<!-- ITERION:CATALOG:GENERATED:END -->\n")
	return dir
}

func newBotServer(t *testing.T, workdir string) *Server {
	t.Helper()
	botregistry.ClearSchemaCache()
	srv := New(Config{
		DisableAuth: true,
		WorkDir:     workdir,
		Bots:        BotsConfig{Paths: botregistry.DefaultPaths(workdir)},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	return srv
}

func doPut(t *testing.T, srv *Server, path, body, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)
	return rec
}

func TestBotsPutRoute_UpdatesManifestAndRegenerates(t *testing.T) {
	dir := botPutFixture(t)
	srv := newBotServer(t, dir)

	rec := doPut(t, srv, "/api/v1/bots/feature_dev",
		`{"display_name":"Featly","when_to_use":"use for features","enabled":false}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var b botregistry.EntryWithSchema
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if b.DisplayName != "Featly" || b.WhenToUse != "use for features" || b.Enabled {
		t.Errorf("response not updated: %+v", b)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "bots", "feature_dev", "manifest.yaml"))
	if !strings.Contains(string(raw), "display_name: Featly") || !strings.Contains(string(raw), "enabled: false") {
		t.Errorf("manifest not persisted:\n%s", raw)
	}
	cat, err := os.ReadFile(filepath.Join(dir, "bots", "whats-next", "skills", "iterion-bot-catalog.md"))
	if err != nil {
		t.Fatalf("catalog not regenerated: %v", err)
	}
	if !strings.Contains(string(cat), "## The team") {
		t.Errorf("catalog missing generated block:\n%s", cat)
	}
	if strings.Contains(string(cat), "### `feature_dev`") {
		t.Errorf("disabled bot should be excluded from the regenerated catalog:\n%s", cat)
	}
}

func TestBotsPutRoute_PreservesUnsetFields(t *testing.T) {
	dir := botPutFixture(t)
	srv := newBotServer(t, dir)

	rec := doPut(t, srv, "/api/v1/bots/feature_dev", `{"when_to_use":"X"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var b botregistry.EntryWithSchema
	json.Unmarshal(rec.Body.Bytes(), &b)
	if b.DisplayName != "Featurly" {
		t.Errorf("display_name must be preserved when omitted, got %q", b.DisplayName)
	}
	if b.WhenToUse != "X" {
		t.Errorf("when_to_use = %q", b.WhenToUse)
	}
}

func TestBotsPutRoute_RejectsLooseBot(t *testing.T) {
	dir := t.TempDir()
	writeBotFile(t, filepath.Join(dir, "bots", "loose.bot"), `## ---
## name: loosey
## ---
`+botPutStub)
	srv := newBotServer(t, dir)
	rec := doPut(t, srv, "/api/v1/bots/loosey", `{"display_name":"x"}`, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBotsPutRoute_NotFound(t *testing.T) {
	dir := botPutFixture(t)
	srv := newBotServer(t, dir)
	rec := doPut(t, srv, "/api/v1/bots/ghost", `{"display_name":"x"}`, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBotsPutRoute_RejectsCrossOrigin(t *testing.T) {
	dir := botPutFixture(t)
	srv := newBotServer(t, dir)
	rec := doPut(t, srv, "/api/v1/bots/feature_dev", `{"display_name":"x"}`, "http://evil.example")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

func TestBotOverlayRoute_TogglesWithoutTouchingManifest(t *testing.T) {
	dir := botPutFixture(t)
	srv := newBotServer(t, dir)

	// Disable via the overlay.
	rec := doPut(t, srv, "/api/v1/bots/feature_dev/overlay", `{"enabled":false}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var b botregistry.EntryWithSchema
	json.Unmarshal(rec.Body.Bytes(), &b)
	if b.Enabled {
		t.Error("overlay disable should resolve Enabled=false")
	}
	// The manifest stays pristine (no enabled key written).
	raw, _ := os.ReadFile(filepath.Join(dir, "bots", "feature_dev", "manifest.yaml"))
	if strings.Contains(string(raw), "enabled:") {
		t.Errorf("overlay must not touch the manifest:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(dir, ".iterion", "bot-overrides.yaml")); err != nil {
		t.Errorf("overlay file not written: %v", err)
	}

	// Clearing the override restores the manifest default (enabled).
	rec = doPut(t, srv, "/api/v1/bots/feature_dev/overlay", `{"enabled":null}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d; body=%s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &b)
	if !b.Enabled {
		t.Error("clearing the overlay should restore Enabled=true")
	}
}

// The studio wires cfg.Bots.Paths ONLY for an explicit --bots-path; the
// default catalog must re-derive from the LIVE WorkDir so /api/v1/bots
// (and pipeline-ticket bot resolution via findBot) follows a project
// switch instead of staying pinned to the boot workspace.
func TestBotsListFollowsProjectSwitch(t *testing.T) {
	botregistry.ClearSchemaCache()
	// Keep swapWorkDir's per-project store out of the real ~/.iterion.
	t.Setenv("ITERION_HOME", t.TempDir())

	dirA := t.TempDir()
	writeBotFile(t, filepath.Join(dirA, "bots", "alpha.bot"),
		strings.ReplaceAll(testBotSrc, "feature_dev", "alpha"))
	dirB := t.TempDir()
	writeBotFile(t, filepath.Join(dirB, "bots", "beta.bot"),
		strings.ReplaceAll(testBotSrc, "feature_dev", "beta"))

	srv := New(Config{
		DisableAuth: true,
		WorkDir:     dirA,
		// No Bots.Paths: the catalog derives from the current WorkDir.
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	listNames := func() []string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/bots", nil)
		rec := httptest.NewRecorder()
		srv.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Bots []botregistry.EntryWithSchema `json:"bots"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
		}
		names := make([]string, 0, len(resp.Bots))
		for _, b := range resp.Bots {
			names = append(names, b.Name)
		}
		return names
	}

	if got := listNames(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("boot workspace: bots = %v, want [alpha]", got)
	}

	if err := srv.swapWorkDir(context.Background(), dirB); err != nil {
		t.Fatalf("swapWorkDir: %v", err)
	}

	if got := listNames(); len(got) != 1 || got[0] != "beta" {
		t.Fatalf("after project switch: bots = %v, want [beta]", got)
	}
}

// An explicit --bots-path stays pinned across project switches — it is
// a deliberate operator override, not a workspace convention.
func TestBotsListExplicitPathsPinnedAcrossSwitch(t *testing.T) {
	botregistry.ClearSchemaCache()
	t.Setenv("ITERION_HOME", t.TempDir())

	pinned := t.TempDir()
	writeBotFile(t, filepath.Join(pinned, "alpha.bot"),
		strings.ReplaceAll(testBotSrc, "feature_dev", "alpha"))
	dirB := t.TempDir()
	writeBotFile(t, filepath.Join(dirB, "bots", "beta.bot"),
		strings.ReplaceAll(testBotSrc, "feature_dev", "beta"))

	srv := New(Config{
		DisableAuth: true,
		WorkDir:     t.TempDir(),
		Bots:        BotsConfig{Paths: []string{pinned}},
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	if err := srv.swapWorkDir(context.Background(), dirB); err != nil {
		t.Fatalf("swapWorkDir: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bots", nil)
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Bots []botregistry.EntryWithSchema `json:"bots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(resp.Bots) != 1 || resp.Bots[0].Name != "alpha" {
		t.Fatalf("explicit --bots-path must stay pinned: got %+v", resp.Bots)
	}
}

// A platform-override entry carries an EMPTY Path. PUT /api/v1/bots/{name}
// must refuse it (409) instead of joining "" and scaffolding a stray
// ./manifest.yaml in the server's CWD while silently dropping the edit.
func TestHandleBotsPut_PlatformOverrideRefused(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	ctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	if _, err := s.botSources.Create(ctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "shadow-bot",
		Files: map[string]string{
			botsource.MainBotFile: testBotMain,
			"manifest.yaml":       "name: shadow-bot\ndisplay_name: Shadow\n",
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.invalidatePlatformBots()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	strayManifest := filepath.Join(cwd, "manifest.yaml")
	if _, err := os.Stat(strayManifest); err == nil {
		t.Fatalf("precondition: %s already exists", strayManifest)
	}

	r := httptest.NewRequest("PUT", "/api/v1/bots/shadow-bot", strings.NewReader(`{"display_name":"Hijack"}`))
	r.SetPathValue("name", "shadow-bot")
	w := httptest.NewRecorder()
	s.handleBotsPut(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("PUT on a platform-override bot = %d (%s), want 409", w.Code, w.Body.String())
	}
	if _, err := os.Stat(strayManifest); err == nil {
		_ = os.Remove(strayManifest)
		t.Fatal("PUT scaffolded a stray manifest.yaml in the server CWD")
	}
}

// The workspace overlay is never consulted for platform-override entries, so
// toggling one through it must refuse instead of 200-ing a silent no-op.
func TestHandleBotOverlay_PlatformOverrideRefused(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	s.cfg.WorkDir = t.TempDir()
	ctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	if _, err := s.botSources.Create(ctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "shadow-bot",
		Files: map[string]string{
			botsource.MainBotFile: testBotMain,
			"manifest.yaml":       "name: shadow-bot\n",
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.invalidatePlatformBots()

	r := httptest.NewRequest("PUT", "/api/v1/bots/shadow-bot/overlay", strings.NewReader(`{"enabled":false}`))
	r.SetPathValue("name", "shadow-bot")
	w := httptest.NewRecorder()
	s.handleBotOverlay(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("overlay on a platform-override bot = %d (%s), want 409", w.Code, w.Body.String())
	}
}

// A loose single-file catalog bot forks as {main.bot: <its source>} — the
// fork must not walk the file's PARENT directory (which sweeps in every
// sibling bot and then 404s on the missing top-level main.bot key).
func TestCatalogBundleFiles_LooseSingleFileBot(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "solo.bot"), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sibling bundle whose files must NOT leak into the fork.
	sib := filepath.Join(dir, "sibling")
	if err := os.MkdirAll(sib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sib, "main.bot"), []byte(testBotMain), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.Bots.Paths = []string{dir}

	files, id, err := s.catalogBundleFiles("solo")
	if err != nil {
		t.Fatalf("catalogBundleFiles: %v", err)
	}
	if id != "solo" || len(files) != 1 || files[botsource.MainBotFile] != testBotMain {
		t.Fatalf("loose fork = (id=%q, %d files) — want just main.bot with the loose source", id, len(files))
	}
}

// platformBotManifest sits on every launch's retry-policy resolution: it
// must serve from the resolver cache, never pay a per-call store read.
// Proven by swapping in a store that fails every read AFTER the cache is
// warm — the manifest must still come back.
func TestPlatformBotManifest_ServedFromCache(t *testing.T) {
	s, _, _ := newBotSourceTestServer(t)
	ctx := store.WithTenant(context.Background(), botsource.PlatformTenantID)
	if _, err := s.botSources.Create(ctx, botsource.BotSource{
		TenantID: botsource.PlatformTenantID,
		Slug:     "cached-bot",
		Files: map[string]string{
			botsource.MainBotFile: testBotMain,
			"manifest.yaml":       "name: cached-bot\ndisplay_name: Cached\n",
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.invalidatePlatformBots()
	if m := s.platformBotManifest("cached-bot"); m == nil || m.DisplayName != "Cached" {
		t.Fatalf("warm read = %+v, want the override manifest", m)
	}

	s.botSources = failingBotSourceStore{Store: s.botSources}
	if m := s.platformBotManifest("cached-bot"); m == nil || m.DisplayName != "Cached" {
		t.Fatalf("cached read = %+v — a per-call store read leaked through", m)
	}
}

// failingBotSourceStore fails every read.
type failingBotSourceStore struct{ botsource.Store }

func (failingBotSourceStore) GetBySlug(context.Context, string, string) (botsource.BotSource, error) {
	return botsource.BotSource{}, context.DeadlineExceeded
}

func (failingBotSourceStore) ListByTenant(context.Context, string) ([]botsource.BotSource, error) {
	return nil, context.DeadlineExceeded
}
