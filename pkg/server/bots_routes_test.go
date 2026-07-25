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
}

func TestBotsListRoute_DisplayName(t *testing.T) {
	botregistry.ClearSchemaCache()
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "whats-next")
	writeBotFile(t, filepath.Join(bundleDir, "manifest.yaml"), "name: whats-next\ndisplay_name: Nexie\ndescription: Orchestrator bot.\n")
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
	if b.Vars == nil || len(b.Vars.Fields) != 2 {
		t.Fatalf("expected 2 vars; got %+v", b.Vars)
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
