package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
