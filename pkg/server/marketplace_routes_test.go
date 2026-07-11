package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/marketplace"
)

// writeFixtureBundle scaffolds a minimal bundle (main.bot + manifest +
// optional README + presets) that botinstall.Inspect can validate. It
// mirrors pkg/botinstall/install_test.go's writeBundle but adds the
// fields the marketplace surfaces.
func writeFixtureBundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "presets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	man := "name: " + name + "\nversion: 0.1.0\ndescription: a sample bot\ndisplay_name: " + name + "-display\nauthor: jo\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\nhello"), 0o644); err != nil {
		t.Fatal(err)
	}
	preset := "---\nname: focus\ndisplay_name: SRE focus\ndescription: bias toward reliability\nskills: [obs]\n---\nrun cool\n"
	if err := os.WriteFile(filepath.Join(dir, "presets", "focus.md"), []byte(preset), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writePluginFixtureDir scaffolds a minimal plugin source (plugin.yaml
// with a skills contribution + the skill file) that plugin.Inspect —
// and so marketplace.InspectSource — accepts.
func writePluginFixtureDir(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	man := "name: " + name + "\nversion: 0.2.0\ndescription: a test plugin\nauthor: jo\ncontributes:\n  skills:\n    - skills/foo.md\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: foo\ndescription: test skill\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "skills", "foo.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newMarketplaceServer(t *testing.T, workdir string) *Server {
	t.Helper()
	store, err := marketplace.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		DisableAuth: true,
		WorkDir:     workdir,
		Marketplace: store,
	}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	return srv
}

func doJSON(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, r)
	return rec
}

func TestMarketplace_SubmitListGetInstall(t *testing.T) {
	repo := t.TempDir()
	writeFixtureBundle(t, repo, "mybot")
	workdir := t.TempDir()
	srv := newMarketplaceServer(t, workdir)

	// Submit the local fixture as a marketplace entry.
	body := `{"repo_url":"` + repo + `","tags":["review", "demo", "review", ""]}`
	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var stored marketplace.Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode submit: %v; body=%s", err, rec.Body.String())
	}
	if stored.Slug != "mybot" || stored.Name != "mybot" {
		t.Errorf("submit entry mismatch: %+v", stored)
	}
	if stored.DisplayName != "mybot-display" {
		t.Errorf("display_name = %q", stored.DisplayName)
	}
	if stored.Author != "jo" || stored.Version != "0.1.0" {
		t.Errorf("metadata not surfaced: %+v", stored)
	}
	// "" + duplicate "review" should have been folded out by normalizeTags.
	if len(stored.Tags) != 2 || stored.Tags[0] != "review" || stored.Tags[1] != "demo" {
		t.Errorf("tags = %v", stored.Tags)
	}
	if len(stored.Presets) != 1 || stored.Presets[0].Name != "focus" {
		t.Errorf("presets = %+v", stored.Presets)
	}
	if !strings.Contains(stored.README, "mybot") {
		t.Errorf("README missing content: %q", stored.README)
	}
	// Workspace must be untouched by submit.
	entries, _ := os.ReadDir(filepath.Join(workdir, ".botz"))
	if len(entries) != 0 {
		t.Errorf("submit must not install: %v", entries)
	}

	// List sees the new entry.
	rec = doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Bots []marketplace.Entry `json:"bots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Bots) != 1 || list.Bots[0].Slug != "mybot" {
		t.Fatalf("list mismatch: %+v", list.Bots)
	}

	// Get by slug.
	rec = doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots/mybot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d; body=%s", rec.Code, rec.Body.String())
	}

	// Install bumps the counter and copies the bundle into <workdir>/.botz.
	rec = doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/bots/mybot/install", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("install status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp marketplaceInstallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode install: %v", err)
	}
	if resp.Install == nil || resp.Install.Name != "mybot" {
		t.Errorf("install result: %+v", resp.Install)
	}
	if resp.Entry == nil || resp.Entry.Installs != 1 {
		t.Errorf("install counter = %+v", resp.Entry)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "mybot", "main.bot")); err != nil {
		t.Errorf("bundle not installed: %v", err)
	}
}

func TestMarketplace_InstallThenUninstall(t *testing.T) {
	repo := t.TempDir()
	writeFixtureBundle(t, repo, "mybot")
	workdir := t.TempDir()
	srv := newMarketplaceServer(t, workdir)

	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("submit = %d; %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/bots/mybot/install", ""); rec.Code != http.StatusOK {
		t.Fatalf("install = %d; %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "mybot", "main.bot")); err != nil {
		t.Fatalf("not installed: %v", err)
	}
	// Update path: force re-install over the existing one must succeed.
	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/bots/mybot/install?force=true", ""); rec.Code != http.StatusOK {
		t.Fatalf("force re-install = %d; %s", rec.Code, rec.Body.String())
	}
	// Uninstall removes the bundle and returns the entry.
	rec := doJSON(t, srv, http.MethodDelete, "/api/v1/marketplace/bots/mybot/install", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall = %d; %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "mybot")); !os.IsNotExist(err) {
		t.Errorf("bundle still present after uninstall: %v", err)
	}
}

func TestBots_UploadBotz(t *testing.T) {
	// Pack a fixture into a .botz, then POST it to /api/v1/bots/upload.
	src := t.TempDir()
	writeFixtureBundle(t, src, "uploaded")
	botz := filepath.Join(t.TempDir(), "uploaded.botz")
	if _, err := bundle.PackDir(src, botz); err != nil {
		t.Fatalf("pack: %v", err)
	}
	data, err := os.ReadFile(botz)
	if err != nil {
		t.Fatal(err)
	}

	workdir := t.TempDir()
	srv := newMarketplaceServer(t, workdir)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "uploaded.botz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/api/v1/bots/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d; %s", rec.Code, rec.Body.String())
	}
	var res botinstall.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if res.Name != "uploaded" {
		t.Errorf("name = %q", res.Name)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "uploaded", "main.bot")); err != nil {
		t.Errorf("uploaded bundle not installed: %v", err)
	}
}

func TestMarketplace_DisabledWhenStoreNil(t *testing.T) {
	srv := New(Config{DisableAuth: true}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux

	rec := doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("list disabled status = %d, want 404", rec.Code)
	}
	rec = doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots/x", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("get disabled status = %d, want 404", rec.Code)
	}
	rec = doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("submit disabled status = %d, want 404", rec.Code)
	}
}

func TestMarketplace_SubmitRequiresRepoURL(t *testing.T) {
	srv := newMarketplaceServer(t, t.TempDir())
	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMarketplace_SubmitMalformedBundleRejected(t *testing.T) {
	// Empty directory — botinstall.Inspect will fail to find a bundle.
	srv := newMarketplaceServer(t, t.TempDir())
	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+t.TempDir()+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMarketplace_InstallMissingSlugReturns404(t *testing.T) {
	srv := newMarketplaceServer(t, t.TempDir())
	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/bots/ghost/install", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMarketplace_GetMissingSlugReturns404(t *testing.T) {
	srv := newMarketplaceServer(t, t.TempDir())
	rec := doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots/ghost", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestMarketplace_ServerInfoFlag(t *testing.T) {
	srv := newMarketplaceServer(t, t.TempDir())
	r := httptest.NewRequest(http.MethodGet, "/api/server/info", nil)
	rec := httptest.NewRecorder()
	srv.handleServerInfo(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var info serverInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if !info.MarketplaceEnabled {
		t.Errorf("marketplace_enabled = false; want true")
	}
}

func TestMarketplace_ServerInfoFlagFalseWhenNil(t *testing.T) {
	srv := New(Config{DisableAuth: true}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	r := httptest.NewRequest(http.MethodGet, "/api/server/info", nil)
	rec := httptest.NewRecorder()
	srv.handleServerInfo(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var info serverInfoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.MarketplaceEnabled {
		t.Errorf("marketplace_enabled = true; want false")
	}
}

func TestMarketplace_SubmitPluginKind(t *testing.T) {
	repo := t.TempDir()
	writePluginFixtureDir(t, repo, "my-plugin")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# plugin doc"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newMarketplaceServer(t, t.TempDir())

	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`","icon":"🔌"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit = %d; %s", rec.Code, rec.Body.String())
	}
	var stored marketplace.Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if stored.Kind != marketplace.KindPlugin {
		t.Errorf("kind = %q, want %q", stored.Kind, marketplace.KindPlugin)
	}
	if stored.Slug != "my-plugin" || stored.Name != "my-plugin" {
		t.Errorf("identity mismatch: %+v", stored)
	}
	if len(stored.Categories) != 1 || stored.Categories[0] != "skill" {
		t.Errorf("categories = %v, want [skill]", stored.Categories)
	}
	if stored.Icon != "🔌" {
		t.Errorf("icon = %q", stored.Icon)
	}
	if !strings.Contains(stored.README, "plugin doc") {
		t.Errorf("README = %q", stored.README)
	}
}

func TestMarketplace_SubmitIconRoundTripAndCap(t *testing.T) {
	repo := t.TempDir()
	writeFixtureBundle(t, repo, "mybot")
	srv := newMarketplaceServer(t, t.TempDir())

	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`","icon":" 🤖 "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit = %d; %s", rec.Code, rec.Body.String())
	}
	var stored marketplace.Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Icon != "🤖" {
		t.Errorf("icon = %q, want trimmed emoji", stored.Icon)
	}
	// Get echoes it back.
	rec = doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots/mybot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	stored = marketplace.Entry{}
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Icon != "🤖" {
		t.Errorf("icon after get = %q", stored.Icon)
	}
	// Over the 32-byte cap → 400.
	long := strings.Repeat("x", marketplaceIconMaxBytes+1)
	rec = doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`","icon":"`+long+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized icon = %d, want 400; %s", rec.Code, rec.Body.String())
	}
}

func TestMarketplace_SlugKindConflict409(t *testing.T) {
	botRepo := t.TempDir()
	writeFixtureBundle(t, botRepo, "mybot")
	pluginRepo := t.TempDir()
	writePluginFixtureDir(t, pluginRepo, "mybot") // same slug, other kind
	srv := newMarketplaceServer(t, t.TempDir())

	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+botRepo+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("bot submit = %d; %s", rec.Code, rec.Body.String())
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+pluginRepo+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("plugin submit over bot slug = %d, want 409; %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already exists as a bot entry") {
		t.Errorf("conflict message = %s", rec.Body.String())
	}
	// Re-submitting the same kind stays fine (registry refresh).
	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+botRepo+`"}`); rec.Code != http.StatusOK {
		t.Errorf("bot re-submit = %d; %s", rec.Code, rec.Body.String())
	}
}

func TestMarketplace_ListKindAndSortParams(t *testing.T) {
	botRepo := t.TempDir()
	writeFixtureBundle(t, botRepo, "mybot")
	pluginRepo := t.TempDir()
	writePluginFixtureDir(t, pluginRepo, "my-plugin")
	srv := newMarketplaceServer(t, t.TempDir())

	for _, repo := range []string{botRepo, pluginRepo} {
		if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("submit %s = %d; %s", repo, rec.Code, rec.Body.String())
		}
	}
	listSlugs := func(query string) []string {
		t.Helper()
		rec := doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots"+query, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s = %d; %s", query, rec.Code, rec.Body.String())
		}
		var list struct {
			Bots []marketplace.Entry `json:"bots"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(list.Bots))
		for _, e := range list.Bots {
			out = append(out, e.Slug)
		}
		return out
	}
	if got := listSlugs("?kind=plugin"); len(got) != 1 || got[0] != "my-plugin" {
		t.Errorf("kind=plugin → %v", got)
	}
	if got := listSlugs("?kind=bot"); len(got) != 1 || got[0] != "mybot" {
		t.Errorf("kind=bot → %v", got)
	}
	if got := listSlugs("?sort=name"); len(got) != 2 || got[0] != "my-plugin" {
		t.Errorf("sort=name → %v", got)
	}
	if rec := doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots?kind=gizmo", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("kind=gizmo = %d, want 400; %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots?sort=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("sort=bogus = %d, want 400; %s", rec.Code, rec.Body.String())
	}
}

func TestMarketplace_PluginInstallUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	repo := t.TempDir()
	writePluginFixtureDir(t, repo, "my-plugin")
	workdir := t.TempDir()
	srv := newMarketplaceServer(t, workdir)

	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("submit = %d; %s", rec.Code, rec.Body.String())
	}
	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/bots/my-plugin/install", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d; %s", rec.Code, rec.Body.String())
	}
	var resp marketplaceInstallResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if resp.Install != nil {
		t.Errorf("bot install payload set for a plugin: %+v", resp.Install)
	}
	if !strings.Contains(rec.Body.String(), `"plugin"`) || strings.Contains(rec.Body.String(), `"install"`) {
		t.Errorf("response shape: %s", rec.Body.String())
	}
	if resp.Plugin == nil || resp.Plugin.Name != "my-plugin" {
		t.Errorf("plugin view = %+v", resp.Plugin)
	}
	if resp.Entry == nil || resp.Entry.Installs != 1 {
		t.Errorf("entry after install = %+v", resp.Entry)
	}
	// The plugin lands under ITERION_HOME/plugins/, never the workspace.
	if _, err := os.Stat(filepath.Join(home, "plugins", "my-plugin", "plugin.yaml")); err != nil {
		t.Errorf("plugin not installed under ITERION_HOME: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(workdir, ".botz")); len(entries) != 0 {
		t.Errorf("plugin install touched .botz/: %v", entries)
	}

	// Uninstall removes the installed plugin directory.
	rec = doJSON(t, srv, http.MethodDelete, "/api/v1/marketplace/bots/my-plugin/install", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall = %d; %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(home, "plugins", "my-plugin")); !os.IsNotExist(err) {
		t.Errorf("plugin dir still present after uninstall: %v", err)
	}
}

func TestMarketplace_PluginInstallNeedsNoWorkdir(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	repo := t.TempDir()
	writePluginFixtureDir(t, repo, "my-plugin")
	srv := newMarketplaceServer(t, "") // no workspace configured

	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("submit = %d; %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/bots/my-plugin/install", ""); rec.Code != http.StatusOK {
		t.Errorf("plugin install without workdir = %d, want 200; %s", rec.Code, rec.Body.String())
	}
}

func TestMarketplace_DownloadRejectsPluginKind(t *testing.T) {
	repo := t.TempDir()
	writePluginFixtureDir(t, repo, "my-plugin")
	srv := newMarketplaceServer(t, t.TempDir())

	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("submit = %d; %s", rec.Code, rec.Body.String())
	}
	rec := doJSON(t, srv, http.MethodGet, "/api/v1/marketplace/bots/my-plugin/download", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("download plugin = %d, want 400; %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "plugin entry") {
		t.Errorf("error message = %s", rec.Body.String())
	}
}

// Sanity check that the Inspect → Entry conversion preserves the
// preset Skills slice fully (defensive copy + slug derivation).
func TestMarketplace_ToEntryPresets(t *testing.T) {
	out := toEntryPresets([]botinstall.PresetMeta{
		{Name: "p1", Skills: []string{"a", "b"}},
	})
	if len(out) != 1 {
		t.Fatalf("len = %d", len(out))
	}
	if len(out[0].Skills) != 2 || out[0].Skills[0] != "a" {
		t.Errorf("skills = %v", out[0].Skills)
	}
}
