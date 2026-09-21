package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every CLI command builds its URL by concatenating operator arguments into a
// path. Go's ServeMux CLEANS dot segments and answers 307, which net/http
// follows preserving method AND body — so an argument carrying `..` silently
// retargets the request to a route the operator never named, and the CLI
// exits 0.
//
// The guard sits in doRequest because that is the ONE function every request
// traverses: ~100 mutating call sites build their own paths, and a per-site
// escape is a guard the next command added will not have.
func TestDoRequestRefusesADotSegment(t *testing.T) {
	for _, path := range []string{
		"/api/admin/orgs/../users/u-victim",
		"/api/teams/t1/members/../../t-other/members/u-9",
		"/api/admin/users/..",
		"/api/admin/users/.",
		"/api/admin/users/%2e%2e/orgs",
		"/api/admin/users/%2E%2E/orgs",
		"/api/admin/users/%2e",
		"../api/admin/orgs",
	} {
		err := checkNoDotSegment(path)
		if err == nil {
			t.Fatalf("checkNoDotSegment(%q) = nil, want a refusal", path)
		}
		if !errors.Is(err, errDotSegment) {
			t.Fatalf("checkNoDotSegment(%q) = %v, want errDotSegment in the chain", path, err)
		}
	}
}

// A blocking guard's false positives cost as much as its holes. A segment
// CONTAINING a dot is ordinary — a bot slug, an email, a version, a
// fingerprint — and must keep working.
func TestDoRequestAllowsAnOrdinaryDot(t *testing.T) {
	for _, path := range []string{
		"/api/admin/users/01a0b4e2-8a2d-783b-8d5c-77e899deb1fe",
		"/api/bots/feature-dev/main.bot",
		"/api/admin/usage-readings/sha256.abcdef",
		"/api/teams/t1/members/someone%40example.org",
		"/api/orgs/o1/members/us-MiXeD-Id",
		"/api/runs/...arguably-odd-but-not-a-dot-segment",
		"/api/x/a..b",
		"/api/x/.hidden",
		"/api/x/trailing.",
		"/",
		"/api/admin/users",
	} {
		if err := checkNoDotSegment(path); err != nil {
			t.Fatalf("checkNoDotSegment(%q) = %v, want nil — an ordinary dot is not a dot segment", path, err)
		}
	}
}

// And the guard is reached from the real request path, not merely present:
// a refused request must never leave the process.
func TestDoRequestRefusalNeverReachesTheServer(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &RemoteClient{cfg: RemoteConfig{BaseURL: srv.URL, Token: "tok"}, http: srv.Client()}
	status, _, err := c.API(context.Background(), http.MethodDelete, "/api/admin/orgs/../users/u-victim", nil)
	if err == nil {
		t.Fatalf("API() = %d, nil — want a refusal", status)
	}
	if !strings.Contains(err.Error(), "dot segment") {
		t.Fatalf("API() error = %v, want it to name the dot segment", err)
	}
	if hits != 0 {
		t.Fatalf("the server was reached %d time(s) — the guard is not on the request path", hits)
	}

	// The control: an ordinary path still goes out.
	if _, _, err := c.API(context.Background(), http.MethodGet, "/api/admin/users/u-1", nil); err != nil {
		t.Fatalf("ordinary request refused: %v", err)
	}
	if hits != 1 {
		t.Fatalf("ordinary request reached the server %d time(s), want 1", hits)
	}
}
