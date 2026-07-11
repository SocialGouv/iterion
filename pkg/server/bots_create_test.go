package server

import (
	"bytes"
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

func newBotCreateServer(t *testing.T) (*Server, string) {
	t.Helper()
	workdir := t.TempDir()
	srv := New(Config{DisableAuth: true, WorkDir: workdir}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	return srv, workdir
}

func postBotCreate(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bots", bytes.NewBufferString(body))
	srv.mux.ServeHTTP(rec, req)
	return rec
}

func TestBotCreate_ScaffoldsAndDiscovers(t *testing.T) {
	srv, workdir := newBotCreateServer(t)
	rec := postBotCreate(t, srv, `{
		"slug": "my-digest",
		"display_name": "My Digest",
		"icon": "📰",
		"description": "Daily repo digest.",
		"instructions": "Summarize recent activity.",
		"vars": [{"name": "window", "type": "string", "default": "24 hours"}],
		"schedule_cron": "0 7 * * 1-5"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var entry botregistry.EntryWithSchema
	if err := json.Unmarshal(rec.Body.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Name != "my-digest" || entry.Icon != "📰" || !entry.Enabled {
		t.Fatalf("entry = %+v, want my-digest / 📰 / enabled", entry.Entry)
	}
	if len(entry.Invocations) != 1 || string(entry.Invocations[0].Kind) != "schedule" {
		t.Errorf("invocations = %+v, want one schedule", entry.Invocations)
	}
	if entry.Vars == nil {
		t.Error("vars schema missing from the created entry")
	}
	for _, f := range []string{"main.bot", "manifest.yaml", "README.md"} {
		if _, err := os.Stat(filepath.Join(workdir, "bots", "my-digest", f)); err != nil {
			t.Errorf("%s missing: %v", f, err)
		}
	}
}

func TestBotCreate_Rejections(t *testing.T) {
	srv, _ := newBotCreateServer(t)
	// Seed one bot for the collision case.
	if rec := postBotCreate(t, srv, `{"slug":"taken","instructions":"x"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}

	cases := map[string]struct {
		body string
		want int
	}{
		"duplicate":    {`{"slug":"taken","instructions":"x"}`, http.StatusConflict},
		"bad slug":     {`{"slug":"Bad Slug","instructions":"x"}`, http.StatusBadRequest},
		"no mission":   {`{"slug":"empty-bot"}`, http.StatusBadRequest},
		"invalid json": {`{`, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postBotCreate(t, srv, tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestBotCreate_CloudModeForbidden(t *testing.T) {
	srv := New(Config{DisableAuth: true, WorkDir: t.TempDir(), Mode: "cloud"}, iterlog.New(iterlog.LevelError, nil))
	srv.handler = srv.mux
	rec := postBotCreate(t, srv, `{"slug":"x-bot","instructions":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestBotTemplates_RouteWinsOverName pins the Go 1.22 mux behaviour the
// registration order relies on: the literal /templates pattern must not
// be captured by GET /api/v1/bots/{name}.
func TestBotTemplates_RouteWinsOverName(t *testing.T) {
	srv, _ := newBotCreateServer(t)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bots/templates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Templates []struct {
			ID   string          `json:"id"`
			Spec json.RawMessage `json:"spec"`
		} `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Templates) < 4 || out.Templates[0].ID != "blank" {
		t.Errorf("templates = %+v, want blank-first gallery", out.Templates)
	}
	if !strings.Contains(rec.Body.String(), "instructions") {
		t.Error("template specs should carry pre-filled instructions")
	}
}
