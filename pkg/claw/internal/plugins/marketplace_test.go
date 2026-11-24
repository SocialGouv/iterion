package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newCatalogServer(t *testing.T, cat Catalog) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/catalog.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cat)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sampleCatalog() Catalog {
	return Catalog{
		Version: 1,
		Plugins: []PluginEntry{
			{Name: "linter", Version: "1.0", Description: "Lints things", TarballURL: "https://x/l.tgz", SHA256: "aa"},
			{Name: "alpha", Version: "0.2", Description: "Alphabet helper", TarballURL: "https://x/a.tgz", SHA256: "bb"},
			{Name: "tester", Version: "2.1", Description: "Run tests", TarballURL: "https://x/t.tgz", SHA256: "cc"},
		},
	}
}

func TestMarketplace_FetchReturnsSortedCatalog(t *testing.T) {
	srv := newCatalogServer(t, sampleCatalog())
	m := New(srv.URL, WithHTTPClient(srv.Client()))
	cat, err := m.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := []string{"alpha", "linter", "tester"}
	if len(cat.Plugins) != len(want) {
		t.Fatalf("plugin count mismatch: got %d want %d", len(cat.Plugins), len(want))
	}
	for i, name := range want {
		if cat.Plugins[i].Name != name {
			t.Errorf("expected %q at index %d, got %q", name, i, cat.Plugins[i].Name)
		}
	}
}

func TestMarketplace_SearchFiltersCaseInsensitive(t *testing.T) {
	srv := newCatalogServer(t, sampleCatalog())
	m := New(srv.URL, WithHTTPClient(srv.Client()))

	hits, err := m.Search(context.Background(), "TEST")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "tester" {
		t.Errorf("expected only 'tester', got %+v", hits)
	}

	all, err := m.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 entries on empty query, got %d", len(all))
	}
}

func TestMarketplace_GetReturnsNotFound(t *testing.T) {
	srv := newCatalogServer(t, sampleCatalog())
	m := New(srv.URL, WithHTTPClient(srv.Client()))

	entry, ok, err := m.Get(context.Background(), "alpha")
	if err != nil || !ok || entry.Version != "0.2" {
		t.Errorf("expected alpha v0.2, got entry=%+v ok=%v err=%v", entry, ok, err)
	}

	_, ok, err = m.Get(context.Background(), "missing")
	if err != nil {
		t.Errorf("unexpected error on missing plugin: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false for missing plugin")
	}
}

func TestMarketplace_FetchPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream barfed"))
	}))
	defer srv.Close()
	m := New(srv.URL, WithHTTPClient(srv.Client()))
	if _, err := m.Fetch(context.Background()); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestMarketplace_FetchHonoursDirectJSONURL(t *testing.T) {
	cat := sampleCatalog()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom.json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(cat)
	}))
	defer srv.Close()
	m := New(srv.URL+"/custom.json", WithHTTPClient(srv.Client()))
	got, err := m.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got.Plugins) != 3 {
		t.Errorf("expected 3 plugins, got %d", len(got.Plugins))
	}
}

func TestNew_RejectsNothing(t *testing.T) {
	// New tolerates an empty baseURL — Fetch is what surfaces the error.
	m := New("")
	if _, err := m.Fetch(context.Background()); err == nil {
		t.Error("expected error fetching from empty baseURL")
	}
}

func TestMarketplace_FetchSummarisesSignedCoverage(t *testing.T) {
	cat := Catalog{
		Version: 1,
		Plugins: []PluginEntry{
			{Name: "linter", Version: "1.0", TarballURL: "https://x/l.tgz", SHA256: "aa"},
			{Name: "signed", Version: "1.0", TarballURL: "https://x/s.tgz", SHA256: "bb",
				SignatureURL: "https://x/s.tgz.sig"},
			{Name: "keyless", Version: "1.0", TarballURL: "https://x/k.tgz", SHA256: "cc",
				CertificateIdentity: "alice@example.com"},
		},
	}
	srv := newCatalogServer(t, cat)

	var buf bytes.Buffer
	m := New(srv.URL,
		WithHTTPClient(srv.Client()),
		WithMarketplaceLogger(&buf),
	)
	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "verification runs at install time") {
		t.Errorf("expected install-time verification summary, got %q", got)
	}
	if !strings.Contains(got, "2/3") {
		t.Errorf("expected '2/3' coverage ratio in summary, got %q", got)
	}
	for _, want := range []string{"signed", "keyless"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected signed plugin %q in summary, got %q", want, got)
		}
	}
	if strings.Contains(got, "linter") {
		t.Errorf("plugin without signature fields should not appear in summary, got %q", got)
	}
}

func TestMarketplace_FetchSilentWhenNoSignatureFields(t *testing.T) {
	srv := newCatalogServer(t, sampleCatalog())
	var buf bytes.Buffer
	m := New(srv.URL, WithHTTPClient(srv.Client()), WithMarketplaceLogger(&buf))
	if _, err := m.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no advisory output, got %q", buf.String())
	}
}
