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
// path. Go's ServeMux CLEANS the path and answers 307, which net/http follows
// preserving method AND body — so an argument that shifts the segments
// silently retargets the request to a route the operator never named, and the
// CLI exits 0.
//
// The guard sits in doRequest because that is the ONE function every request
// traverses: ~100 mutating call sites build their own paths, and a per-site
// escape is a guard the next command added will not have.
//
// The predicate is the SERVER's own cleaner, not a list of spellings. The
// first version of this guard enumerated `.` and `..` and got BOTH arms
// wrong: it skipped an empty segment by construction (which retargets
// exactly like `..`) and refused `.` (which retargets nothing).
func TestDoRequestRefusesAPathTheServerWouldRewrite(t *testing.T) {
	for _, path := range []string{
		// The `..` family the first version did catch.
		"/api/admin/orgs/../users/u-victim",
		"/api/teams/t1/members/../../t-other/members/u-9",
		"/api/admin/users/..",
		"/api/admin/users/%2e%2e/orgs",
		"/api/admin/users/%2E%2E/orgs",
		// doRequest prefixes the root slash before the guard runs, so this is
		// the shape a relative argument actually arrives in.
		"/../api/admin/orgs",
		// The EMPTY segment, which the enumeration skipped: measured against
		// the real server, `PUT /api/v1/bots//overlay` 307s to
		// `PUT /api/v1/bots/{name}` — a different route, and a mutating one.
		"/api/v1/bots//overlay",
		"/api/teams/t1//board-binding",
		"/api/admin/users//reset-password",
	} {
		err := checkPathIsServed(path)
		if err == nil {
			t.Fatalf("checkPathIsServed(%q) = nil, want a refusal", path)
		}
		if !errors.Is(err, errPathRetarget) {
			t.Fatalf("checkPathIsServed(%q) = %v, want errPathRetarget in the chain", path, err)
		}
	}
}

// A blocking guard's false positives cost as much as its holes. `.` is the
// identity under cleaning — it can never retarget anything, only `..` can —
// and refusing it broke a real invocation: `runs artifacts <id> --file
// './report.md'`, where RemoteRunsArtifacts already normalises pasted paths.
func TestDoRequestAllowsAPathTheServerServesAsWritten(t *testing.T) {
	for _, path := range []string{
		// The false positive the enumeration introduced.
		"/api/runs/r1/artifact-files/./report.md",
		"/api/runs/r1/artifact-files/a/./b",
		// Ordinary dots in a segment: a bot bundle, a digest, an email, a
		// dotfile, a version.
		"/api/admin/users/01a0b4e2-8a2d-783b-8d5c-77e899deb1fe",
		"/api/bots/feature-dev/main.bot",
		"/api/admin/usage-readings/sha256.abcdef",
		"/api/teams/t1/members/someone%40example.org",
		"/api/x/.hidden",
		"/api/x/a..b",
		"/api/x/trailing.",
		"/api/x/my.secret.v1",
		// A trailing slash is meaningful to the mux (subtree patterns), not
		// a rewrite — Clean drops it, so the guard must put it back.
		"/api/teams/t1/files/",
		"/",
		"/api/admin/users",
		// A query string is not a path and is not cleaned.
		"/api/admin/users?q=a.b",
		"/api/admin/audit?from=2026-01-01&to=2026-12-31",
	} {
		if err := checkPathIsServed(path); err != nil {
			t.Fatalf("checkPathIsServed(%q) = %v, want nil — the server serves this as written", path, err)
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
	if !strings.Contains(err.Error(), "would serve") {
		t.Fatalf("API() error = %v, want it to name what the server would serve instead", err)
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
