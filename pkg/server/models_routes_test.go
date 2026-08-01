package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SocialGouv/iterion/pkg/modelcatalog"
)

func getModels(t *testing.T, srv *Server, query string) (*httptest.ResponseRecorder, modelcatalog.Catalog) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models"+query, nil))
	var cat modelcatalog.Catalog
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil {
			t.Fatalf("response is not a Catalog: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, cat
}

func TestGetModels_ReturnsTheKnownSet(t *testing.T) {
	srv, _ := newTestServer(t)

	rec, cat := getModels(t, srv, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(cat.Models) == 0 {
		t.Fatal("catalog is empty")
	}
	if _, ok := cat.Find("anthropic/claude-opus-5"); !ok {
		t.Errorf("expected anthropic/claude-opus-5 in the default set, got %v", cat.SortedSpecs())
	}
	// Every row must carry the fields a picker needs to render a decision.
	for _, m := range cat.Models {
		if m.Spec == "" || m.Source == "" {
			t.Errorf("row is missing identity/source: %+v", m)
		}
		if !m.Usable && m.UnusableReason == "" {
			t.Errorf("%s is unusable without saying why", m.Spec)
		}
	}
}

// LaunchView asks about the specs its nodes actually pin, which may sit outside
// the curated set — the endpoint has to answer for those too.
func TestGetModels_HonoursExplicitSpecs(t *testing.T) {
	srv, _ := newTestServer(t)

	_, cat := getModels(t, srv, "?spec=openai/gpt-5.5&spec=anthropic/claude-haiku-4-5")
	if got := cat.SortedSpecs(); len(got) != 2 {
		t.Fatalf("specs = %v, want exactly the 2 requested", got)
	}
	if _, ok := cat.Find("openai/gpt-5.5"); !ok {
		t.Error("openai/gpt-5.5 missing")
	}
}

// A bot with twenty nodes on one model must not produce twenty rows, and a
// caller writing one comma-separated param must not get a 400.
func TestGetModels_DedupesAndSplitsSpecs(t *testing.T) {
	srv, _ := newTestServer(t)

	_, cat := getModels(t, srv, "?spec=openai/gpt-5.5,anthropic/claude-opus-5&spec=openai/gpt-5.5")
	if got := cat.SortedSpecs(); len(got) != 2 {
		t.Fatalf("specs = %v, want 2 deduped entries", got)
	}
}

func TestGetModels_MalformedSpecIsA400(t *testing.T) {
	srv, _ := newTestServer(t)

	rec, _ := getModels(t, srv, "?spec=no-provider-prefix")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// The endpoint reports credential SOURCE names so an operator can fix a gap —
// it must never echo a credential value. Detection only ever produces variable
// names, and this test pins that contract at the HTTP boundary.
func TestGetModels_NeverLeaksCredentialValues(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-supersecret-test-value")
	srv, _ := newTestServer(t)

	rec, _ := getModels(t, srv, "?refresh=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); contains(body, "sk-ant-supersecret-test-value") {
		t.Fatalf("response leaked a credential value:\n%s", body)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestDedupeSpecs(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{" a/b ", "a/b"}, []string{"a/b"}},
		{[]string{"a/b,c/d", "c/d"}, []string{"a/b", "c/d"}},
		{[]string{"", " ", ","}, []string{}},
	}
	for _, tc := range cases {
		got := dedupeSpecs(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("dedupeSpecs(%v) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("dedupeSpecs(%v) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}
