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
// the curated set. Those must be ADDED to the known set, never replace it —
// narrowing the picker to the models already in use is the one list from which
// no new choice can be made.
func TestGetModels_ExtraSpecsAddToTheKnownSet(t *testing.T) {
	srv, _ := newTestServer(t)

	_, base := getModels(t, srv, "")
	_, cat := getModels(t, srv, "?spec=somevendor/some-model-9")

	if len(cat.Models) != len(base.Models)+1 {
		t.Fatalf("got %d models, want the %d known ones plus the requested one: %v",
			len(cat.Models), len(base.Models), cat.SortedSpecs())
	}
	if _, ok := cat.Find("somevendor/some-model-9"); !ok {
		t.Error("the requested spec is missing")
	}
	if _, ok := cat.Find("anthropic/claude-opus-5"); !ok {
		t.Error("asking about one model must not hide the rest")
	}
}

// A bot with twenty nodes on one model must not produce twenty rows, a spec
// already in the known set must not be duplicated, and a caller writing one
// comma-separated param must not get a 400.
func TestGetModels_DedupesAndSplitsSpecs(t *testing.T) {
	srv, _ := newTestServer(t)

	_, base := getModels(t, srv, "")
	_, cat := getModels(t, srv,
		"?spec=somevendor/one,anthropic/claude-opus-5&spec=somevendor/one")
	// +1: "somevendor/one" once, and claude-opus-5 is already known.
	if len(cat.Models) != len(base.Models)+1 {
		t.Fatalf("got %d models, want %d: %v", len(cat.Models), len(base.Models)+1, cat.SortedSpecs())
	}
}

// One malformed hint must not blank out the picker. LaunchView asks about
// every LLM node's DSL default in a single call, and a bot in this very repo
// pins a bare `model: "claude-opus-5"` — under a fail-whole contract that one
// bot made the registry unusable for every OTHER model the host can reach.
// The bad spec is skipped and REPORTED; the catalog still answers.
func TestGetModels_MalformedSpecDegradesInsteadOfBlankingTheCatalog(t *testing.T) {
	srv, _ := newTestServer(t)

	_, base := getModels(t, srv, "")
	rec, cat := getModels(t, srv, "?spec=no-provider-prefix&spec=somevendor/one")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The valid hint still lands; the malformed one costs only itself.
	if len(cat.Models) != len(base.Models)+1 {
		t.Fatalf("got %d models, want %d: %v", len(cat.Models), len(base.Models)+1, cat.SortedSpecs())
	}
	if len(cat.InvalidSpecs) != 1 || cat.InvalidSpecs[0].Spec != "no-provider-prefix" {
		t.Fatalf("invalid_specs = %+v, want exactly the malformed hint", cat.InvalidSpecs)
	}
	if cat.InvalidSpecs[0].Reason == "" {
		t.Fatal("a skipped spec must say why, or the caller cannot fix it")
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
