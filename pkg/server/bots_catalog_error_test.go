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

	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// unreadableCatalogOwner vendors a catalog owner under <workdir>/bots/nexie
// whose hand-authored template is a self-referencing symlink: ELOOP, which
// no privilege reads, so the regeneration fails whoever runs the test. It
// returns the bundle root every regeneration error names.
func unreadableCatalogOwner(t *testing.T, workdir string) string {
	t.Helper()
	dir := filepath.Join(workdir, "bots", "nexie")
	writeBotFile(t, filepath.Join(dir, "manifest.yaml"), "name: nexie\ndisplay_name: Nexie\ndescription: Orchestrator.\n")
	writeBotFile(t, filepath.Join(dir, "main.bot"), "agent a:\n  model: \"test\"\n")
	tmpl := filepath.Join(dir, "iterion-bot-catalog-static.md")
	if err := os.Symlink(tmpl, tmpl); err != nil {
		t.Fatalf("symlink unavailable: %v", err)
	}
	botregistry.ClearSchemaCache()
	return dir
}

// postBotzUpload packs srcDir into a `.botz` and posts it to the upload
// route the studio's "Import .botz file" affordance calls.
func postBotzUpload(t *testing.T, srv *Server, srcDir string) *httptest.ResponseRecorder {
	t.Helper()
	botz := filepath.Join(t.TempDir(), "packed.botz")
	if _, err := bundle.PackDir(srcDir, botz); err != nil {
		t.Fatalf("pack: %v", err)
	}
	data, err := os.ReadFile(botz)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "packed.botz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/bots/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, r)
	return rec
}

// TestBotInstallRoute_ReportsTheCatalogRegenFailure: the bundle landed, so
// the request succeeds — but the catalog Nexie routes on was not refreshed,
// and the studio has no other way to learn it than this response.
func TestBotInstallRoute_ReportsTheCatalogRegenFailure(t *testing.T) {
	workdir := t.TempDir()
	owner := unreadableCatalogOwner(t, workdir)
	source := t.TempDir()
	writeBotFile(t, filepath.Join(source, "manifest.yaml"), "name: newbot\nversion: 0.1.0\ndescription: fresh bot\n")
	writeBotFile(t, filepath.Join(source, "main.bot"), "workflow w:\n")
	srv := newBotServer(t, workdir)

	rec := doJSON(t, srv, http.MethodPost, "/api/v1/bots/install", `{"url":"`+source+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d; %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "newbot", "main.bot")); err != nil {
		t.Fatalf("the install itself must still succeed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	msg, _ := got["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the install response hides the catalog regeneration failure: %s", rec.Body.String())
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
	}
}

// TestBotUploadRoute_ReportsTheCatalogRegenFailure: the .botz upload path
// reuses Install, and its response is the studio's only word on the
// refresh too.
func TestBotUploadRoute_ReportsTheCatalogRegenFailure(t *testing.T) {
	workdir := t.TempDir()
	owner := unreadableCatalogOwner(t, workdir)
	src := t.TempDir()
	writeFixtureBundle(t, src, "packed")
	srv := newBotServer(t, workdir)

	rec := postBotzUpload(t, srv, src)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d; %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "packed", "main.bot")); err != nil {
		t.Fatalf("the upload itself must still succeed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	msg, _ := got["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the upload response hides the catalog regeneration failure: %s", rec.Body.String())
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
	}
}

// TestMarketplaceInstallRoute_ReportsTheCatalogRegenFailure covers the
// marketplace install response, which carries the install result in its
// own `install` member.
func TestMarketplaceInstallRoute_ReportsTheCatalogRegenFailure(t *testing.T) {
	repo := t.TempDir()
	writeFixtureBundle(t, repo, "mybot")
	workdir := t.TempDir()
	owner := unreadableCatalogOwner(t, workdir)
	srv := newMarketplaceServer(t, workdir)
	if rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/submit", `{"repo_url":"`+repo+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("submit = %d; %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/v1/marketplace/bots/mybot/install", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d; %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Install map[string]any `json:"install"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	msg, _ := got.Install["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the marketplace install response hides the catalog regeneration failure: %s", rec.Body.String())
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
	}
}

// TestMarketplaceUninstallRoute_ReportsTheCatalogRegenFailure: the bundle
// is gone, so the removal succeeded — but the catalog still advertises it
// until a refresh lands, and the operator must be able to tell that from a
// clean uninstall.
func TestMarketplaceUninstallRoute_ReportsTheCatalogRegenFailure(t *testing.T) {
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
	// Break the catalog only now: the install must stay a clean success so
	// the uninstall is the one gesture under test.
	owner := unreadableCatalogOwner(t, workdir)

	rec := doJSON(t, srv, http.MethodDelete, "/api/v1/marketplace/bots/mybot/install", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall = %d; %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "mybot")); !os.IsNotExist(err) {
		t.Fatalf("the removal itself must still succeed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if name, _ := got["name"].(string); name != "mybot" {
		t.Errorf("the uninstall response no longer carries the entry it removed: %s", rec.Body.String())
	}
	msg, _ := got["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the uninstall response hides the catalog regeneration failure: %s", rec.Body.String())
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
	}
}

// TestBotCreateRoute_ReportsTheCatalogRegenFailure: the studio builder's
// "New bot" and the assistant's bot.create add a bundle to the routing table
// exactly like an install. The scaffold lands whatever the refresh does, and
// this response is the studio's only word on it — the banner otherwise says
// the new bot is visible to an orchestrator that cannot route to it.
func TestBotCreateRoute_ReportsTheCatalogRegenFailure(t *testing.T) {
	workdir := t.TempDir()
	owner := unreadableCatalogOwner(t, workdir)
	srv := newBotServer(t, workdir)

	rec := doJSON(t, srv, http.MethodPost, "/api/v1/bots", `{"slug":"freshbot","instructions":"Do the thing."}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d; %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workdir, "bots", "freshbot", "main.bot")); err != nil {
		t.Fatalf("the scaffold itself must still succeed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if name, _ := got["name"].(string); name != "freshbot" {
		t.Errorf("the create response no longer carries the entry it created: %s", rec.Body.String())
	}
	msg, _ := got["catalog_error"].(string)
	if msg == "" {
		t.Fatalf("the create response hides the catalog regeneration failure: %s", rec.Body.String())
	}
	if !strings.Contains(msg, owner) {
		t.Errorf("catalog_error %q does not name the catalog owner it could not refresh (%s)", msg, owner)
	}
}

// TestBotCreateRoute_SilentWhenTheCatalogRefreshSucceeds: the created bot's
// row really is in the refreshed catalog, and the response says nothing.
func TestBotCreateRoute_SilentWhenTheCatalogRefreshSucceeds(t *testing.T) {
	workdir := t.TempDir()
	writeBotFile(t, filepath.Join(workdir, "bots", "nexie", "manifest.yaml"), "name: nexie\ndisplay_name: Nexie\ndescription: Orchestrator.\n")
	writeBotFile(t, filepath.Join(workdir, "bots", "nexie", "main.bot"), "agent a:\n  model: \"test\"\n")
	writeBotFile(t, filepath.Join(workdir, "bots", "nexie", "iterion-bot-catalog-static.md"),
		"---\nname: iterion-bot-catalog\n---\nPREAMBLE\n\n<!-- ITERION:CATALOG:GENERATED:BEGIN -->\n<!-- ITERION:CATALOG:GENERATED:END -->\n")
	botregistry.ClearSchemaCache()
	srv := newBotServer(t, workdir)

	rec := doJSON(t, srv, http.MethodPost, "/api/v1/bots", `{"slug":"freshbot","instructions":"Do the thing."}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d; %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "catalog_error") {
		t.Errorf("a successful refresh must add nothing to the response: %s", rec.Body.String())
	}
	// The premise: the refresh the route ran really wrote the new bot's row.
	generated, err := os.ReadFile(filepath.Join(workdir, "bots", "nexie", "skills", "iterion-bot-catalog.md"))
	if err != nil {
		t.Fatalf("the refresh wrote no catalog: %v", err)
	}
	if !strings.Contains(string(generated), "freshbot") {
		t.Errorf("the refreshed catalog does not advertise the created bot:\n%s", generated)
	}
}

// TestBotMetadataRoute_CarriesTheStaleCatalogOnTheAnswer: the manifest PUT
// and the overlay change WHICH bots the table routes on — the rendered table
// holds one row per enabled bot, so an enable/disable whose refresh failed
// leaves the previous set routed until a regeneration succeeds. The studio's
// only word on that is the entry these routes answer, so the cause travels
// on it (`catalog_error`), next to the standing server-log warning.
func TestBotMetadataRoute_CarriesTheStaleCatalogOnTheAnswer(t *testing.T) {
	workdir := botPutFixture(t)
	owner := unreadableCatalogOwner(t, workdir)
	var logs bytes.Buffer
	srv := New(Config{
		DisableAuth: true,
		WorkDir:     workdir,
		Bots:        BotsConfig{Paths: botregistry.DefaultPaths(workdir)},
	}, iterlog.New(iterlog.LevelWarn, &logs))
	srv.handler = srv.mux

	rec := doPut(t, srv, "/api/v1/bots/feature_dev", `{"display_name":"Featly"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d; %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), owner) {
		t.Errorf("the server log must still name the catalog it could not refresh (%s):\n%s", owner, logs.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; %s", err, rec.Body.String())
	}
	cause, _ := got["catalog_error"].(string)
	if cause == "" {
		t.Fatalf("the metadata answer hides the stale catalog the failed refresh left:\n%s", rec.Body.String())
	}
	if !strings.Contains(cause, owner) {
		t.Errorf("catalog_error %q does not name the owner it could not refresh (%s)", cause, owner)
	}
}

// TestBotOverlayRoute_CarriesTheStaleCatalogOnTheAnswer: the catalog
// manager's quick-toggle answers the same entry — a toggle whose refresh
// failed must say so, or the operator reads "enabled" while the table the
// dispatcher routes on still shows the previous set.
func TestBotOverlayRoute_CarriesTheStaleCatalogOnTheAnswer(t *testing.T) {
	workdir := botPutFixture(t)
	owner := unreadableCatalogOwner(t, workdir)
	srv := newBotServer(t, workdir)

	rec := doPut(t, srv, "/api/v1/bots/feature_dev/overlay", `{"enabled":false}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("overlay = %d; %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; %s", err, rec.Body.String())
	}
	cause, _ := got["catalog_error"].(string)
	if cause == "" {
		t.Fatalf("the overlay answer hides the stale catalog the failed refresh left:\n%s", rec.Body.String())
	}
	if !strings.Contains(cause, owner) {
		t.Errorf("catalog_error %q does not name the owner it could not refresh (%s)", cause, owner)
	}
}

// TestBotMetadataRoute_SilentWhenTheCatalogRefreshSucceeds: a healthy
// refresh adds nothing to the metadata answer — the field exists to report
// a failure, not to narrate a success.
func TestBotMetadataRoute_SilentWhenTheCatalogRefreshSucceeds(t *testing.T) {
	workdir := botPutFixture(t)
	srv := newBotServer(t, workdir)

	rec := doPut(t, srv, "/api/v1/bots/feature_dev", `{"display_name":"Featly"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d; %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "catalog_error") {
		t.Errorf("a successful refresh must add nothing to the metadata answer: %s", rec.Body.String())
	}
}

// TestBotInstallRoute_SilentWhenTheCatalogRefreshSucceeds: a healthy
// refresh adds nothing to the response — the field exists to report a
// failure, not to narrate a success.
func TestBotInstallRoute_SilentWhenTheCatalogRefreshSucceeds(t *testing.T) {
	workdir := t.TempDir()
	writeBotFile(t, filepath.Join(workdir, "bots", "nexie", "manifest.yaml"), "name: nexie\ndisplay_name: Nexie\ndescription: Orchestrator.\n")
	writeBotFile(t, filepath.Join(workdir, "bots", "nexie", "main.bot"), "agent a:\n  model: \"test\"\n")
	writeBotFile(t, filepath.Join(workdir, "bots", "nexie", "iterion-bot-catalog-static.md"),
		"---\nname: iterion-bot-catalog\n---\nPREAMBLE\n\n<!-- ITERION:CATALOG:GENERATED:BEGIN -->\n<!-- ITERION:CATALOG:GENERATED:END -->\n")
	botregistry.ClearSchemaCache()
	source := t.TempDir()
	writeBotFile(t, filepath.Join(source, "manifest.yaml"), "name: newbot\nversion: 0.1.0\ndescription: fresh bot\n")
	writeBotFile(t, filepath.Join(source, "main.bot"), "workflow w:\n")
	srv := newBotServer(t, workdir)

	rec := doJSON(t, srv, http.MethodPost, "/api/v1/bots/install", `{"url":"`+source+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("install = %d; %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "catalog_error") {
		t.Errorf("a successful refresh must add nothing to the response: %s", rec.Body.String())
	}
}
