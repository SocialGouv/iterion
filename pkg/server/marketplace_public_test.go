package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/marketplace"
)

func TestIsPublicMarketplaceRead(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/api/v1/marketplace/bots", true},
		{http.MethodGet, "/api/v1/marketplace/config", true},
		{http.MethodGet, "/api/v1/marketplace/bots/mybot", true},
		{http.MethodGet, "/api/v1/marketplace/bots/mybot/download", true},
		// Mutating / privileged surfaces stay private.
		{http.MethodPost, "/api/v1/marketplace/submit", false},
		{http.MethodPost, "/api/v1/marketplace/bots/mybot/install", false},
		{http.MethodDelete, "/api/v1/marketplace/bots/mybot/install", false},
		{http.MethodGet, "/api/v1/marketplace/bots/mybot/install", false}, // belt-and-suspenders
		{http.MethodGet, "/api/v1/marketplace/moderation", false},
		// Unrelated API paths are never opened here.
		{http.MethodGet, "/api/v1/bots", false},
		{http.MethodGet, "/api/runs", false},
	}
	for _, c := range cases {
		if got := isPublicMarketplaceRead(c.method, c.path); got != c.want {
			t.Errorf("isPublicMarketplaceRead(%q, %q) = %v; want %v", c.method, c.path, got, c.want)
		}
	}
}

// newAuthedCloudMarketplaceServer builds an auth-enabled cloud server (no
// DisableAuth) with the marketplace store wired, plus the auth middleware
// chain so the public-path bypass is actually exercised.
func newAuthedCloudMarketplaceServer(t *testing.T) (*Server, http.Handler, marketplace.Store) {
	t.Helper()
	store, err := marketplace.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		Mode:        "cloud",
		WorkDir:     t.TempDir(),
		Marketplace: store,
	}, iterlog.New(iterlog.LevelError, nil))
	return srv, srv.authMiddleware(srv.mux), store
}

// TestMarketplace_PublicAccessGating asserts the anti-injection boundary:
// anonymous callers can browse + download (public reads) but cannot submit
// or install (mutating, auth-required) in cloud mode.
func TestMarketplace_PublicAccessGating(t *testing.T) {
	repo := t.TempDir()
	writeFixtureBundle(t, repo, "mybot")
	srv, h, store := newAuthedCloudMarketplaceServer(t)

	// Seed an approved public (builtin-style) entry so an anonymous viewer
	// can see it — a *pending* submission would be invisible by design.
	if err := store.Upsert(context.Background(), marketplace.Entry{
		Slug:    "mybot",
		Name:    "mybot",
		RepoURL: repo, // local bundle dir, as a builtin seed records it
		Version: "0.1.0",
		Source:  marketplace.SourceBuiltin,
		Status:  marketplace.StatusApproved,
		Scope:   marketplace.ScopePublic,
	}); err != nil {
		t.Fatal(err)
	}

	do := func(method, path string, body []byte) *httptest.ResponseRecorder {
		var r *http.Request
		if body == nil {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, bytes.NewReader(body))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	// Anonymous browse → 200, lists the public entry.
	rec := do(http.MethodGet, "/api/v1/marketplace/bots", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("anon browse = %d; %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Bots []marketplace.Entry `json:"bots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Bots) != 1 || list.Bots[0].Slug != "mybot" {
		t.Fatalf("anon browse mismatch: %+v", list.Bots)
	}

	// Anonymous detail → 200.
	if rec := do(http.MethodGet, "/api/v1/marketplace/bots/mybot", nil); rec.Code != http.StatusOK {
		t.Fatalf("anon detail = %d; %s", rec.Code, rec.Body.String())
	}

	// Anonymous .botz download → 200 + a re-openable bundle named "mybot".
	rec = do(http.MethodGet, "/api/v1/marketplace/bots/mybot/download", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("anon download = %d; %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("download content-type = %q", ct)
	}
	dest := t.TempDir()
	if _, err := bundle.ExtractArchive(bytes.NewReader(rec.Body.Bytes()), dest); err != nil {
		t.Fatalf("extract downloaded .botz: %v", err)
	}
	b, err := bundle.OpenDir(dest)
	if err != nil {
		t.Fatalf("open downloaded bundle: %v", err)
	}
	if b.Manifest == nil || b.Manifest.Name != "mybot" {
		t.Errorf("downloaded bundle manifest = %+v", b.Manifest)
	}

	// Anonymous submit → 401 (needs SSO/login).
	if rec := do(http.MethodPost, "/api/v1/marketplace/submit", []byte(`{"repo_url":"`+repo+`"}`)); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon submit = %d; want 401", rec.Code)
	}
	// Anonymous install → 401.
	if rec := do(http.MethodPost, "/api/v1/marketplace/bots/mybot/install", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon install = %d; want 401", rec.Code)
	}
	// Anonymous uninstall → 401.
	if rec := do(http.MethodDelete, "/api/v1/marketplace/bots/mybot/install", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon uninstall = %d; want 401", rec.Code)
	}
	// A pending submission must NOT leak to an anonymous viewer.
	if err := store.Upsert(context.Background(), marketplace.Entry{
		Slug: "secret", Name: "secret", RepoURL: repo, Version: "0.1.0",
		Source: marketplace.SourceGit, Status: marketplace.StatusPending, Scope: marketplace.ScopePublic,
	}); err != nil {
		t.Fatal(err)
	}
	if rec := do(http.MethodGet, "/api/v1/marketplace/bots/secret/download", nil); rec.Code != http.StatusNotFound {
		t.Errorf("anon download of pending entry = %d; want 404", rec.Code)
	}
	_ = srv
}
