package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSearxngSearch_Formats verifies the SearXNG JSON API is queried and
// its results are rendered as the shared "N. title / url / snippet" block.
func TestSearxngSearch_Formats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("format"); got != "json" {
			t.Errorf("format = %q, want json", got)
		}
		if got := r.URL.Query().Get("q"); got != "iterion dsl" {
			t.Errorf("q = %q, want %q", got, "iterion dsl")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"title":"First","url":"https://a.example/1","content":"snippet one"},
			{"title":"Second","url":"https://b.example/2","content":"snippet two"}
		]}`))
	}))
	defer srv.Close()

	out, err := searxngSearch("iterion dsl", 5, srv.URL)
	if err != nil {
		t.Fatalf("searxngSearch: %v", err)
	}
	for _, want := range []string{"1. First", "https://a.example/1", "snippet one", "2. Second", "https://b.example/2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
}

// TestSearxngSearch_TruncatesToNumResults ensures more results than
// requested are clamped to num_results.
func TestSearxngSearch_TruncatesToNumResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[
			{"title":"A","url":"https://x/1","content":"c1"},
			{"title":"B","url":"https://x/2","content":"c2"},
			{"title":"C","url":"https://x/3","content":"c3"}
		]}`))
	}))
	defer srv.Close()

	out, err := searxngSearch("q", 2, srv.URL)
	if err != nil {
		t.Fatalf("searxngSearch: %v", err)
	}
	if strings.Contains(out, "3. C") || strings.Contains(out, "https://x/3") {
		t.Errorf("expected only 2 results, got:\n%s", out)
	}
}

// TestSearxngSearch_HTTPError surfaces a non-2xx (e.g. JSON format not
// enabled on the instance) as an explicit error, not an empty result.
func TestSearxngSearch_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	if _, err := searxngSearch("q", 5, srv.URL); err == nil {
		t.Fatal("expected error on HTTP 403, got nil")
	}
}

// TestExecuteWebSearch_SearxngPrecedence verifies SEARXNG_URL wins over
// BRAVE_API_KEY: with both set, the SearXNG instance is queried and Brave
// is never contacted (its override endpoint would fail the test if hit).
func TestExecuteWebSearch_SearxngPrecedence(t *testing.T) {
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"FromSearxng","url":"https://s/1","content":"c"}]}`))
	}))
	defer searx.Close()

	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Brave endpoint hit despite SEARXNG_URL being set")
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"FromBrave","url":"https://b/1","description":"d"}]}}`))
	}))
	defer brave.Close()

	t.Setenv("SEARXNG_URL", searx.URL)
	t.Setenv("BRAVE_API_KEY", "dummy")
	t.Setenv("CLAW_WEB_SEARCH_BRAVE_URL", brave.URL)

	out, err := ExecuteWebSearch(map[string]any{"query": "q"})
	if err != nil {
		t.Fatalf("ExecuteWebSearch: %v", err)
	}
	if !strings.Contains(out, "FromSearxng") {
		t.Errorf("expected SearXNG result, got:\n%s", out)
	}
	if strings.Contains(out, "FromBrave") {
		t.Errorf("Brave result leaked through:\n%s", out)
	}
}

// TestSearxngBaseURL_EndpointAlias confirms SEARXNG_ENDPOINT is honoured
// as an alias when SEARXNG_URL is unset (the name Firecrawl uses).
func TestSearxngBaseURL_EndpointAlias(t *testing.T) {
	t.Setenv("SEARXNG_URL", "")
	t.Setenv("SEARXNG_ENDPOINT", "http://searx.internal:8080")
	if got := searxngBaseURL(); got != "http://searx.internal:8080" {
		t.Errorf("searxngBaseURL() = %q, want the SEARXNG_ENDPOINT alias", got)
	}
}
