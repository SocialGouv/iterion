package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// A token that can rewrite an org's roadmap is a broader privilege than one
// that can push a branch. These tests pin that the project-board grant is a
// SEPARATE opt-in profile and never leaks into the runtime baseline — the same
// shape as the security-read and repo-admin precedents.

func TestProjectsInstallationPermissionsIsItsOwnProfile(t *testing.T) {
	perms := ProjectsInstallationPermissions()
	if perms["organization_projects"] != "write" {
		t.Errorf("projects profile must carry organization_projects:write, got %v", perms)
	}
	if perms["metadata"] != "read" {
		t.Errorf("projects profile must carry the mandatory metadata baseline, got %v", perms)
	}
	if _, ok := RuntimeInstallationPermissions()["organization_projects"]; ok {
		t.Error("organization_projects must NOT be in the runtime baseline: the cached runtime token stays minimal")
	}
}

func TestBuildAppManifestProjectBoardIsOptIn(t *testing.T) {
	base := BuildAppManifest("it", "https://x", "https://x/cb")
	if _, ok := base.DefaultPermissions["organization_projects"]; ok {
		t.Error("default manifest must not request organization_projects")
	}

	opted := BuildAppManifest("it", "https://x", "https://x/cb", AppManifestOptions{AllowProjectBoard: true})
	if opted.DefaultPermissions["organization_projects"] != "write" {
		t.Errorf("AllowProjectBoard must request organization_projects:write, got %v", opted.DefaultPermissions)
	}
	// The baseline must survive alongside it.
	if opted.DefaultPermissions["contents"] != "write" {
		t.Error("the runtime baseline must remain when the project grant is added")
	}

	// A watch-only App replaces the whole set: no write grant may sneak in.
	watch := BuildAppManifest("it", "https://x", "https://x/cb", AppManifestOptions{SecurityReadOnly: true, AllowProjectBoard: true})
	if _, ok := watch.DefaultPermissions["organization_projects"]; ok {
		t.Errorf("a security-read-only App must carry no project grant, got %v", watch.DefaultPermissions)
	}
}

func TestMissingProjectPermissions(t *testing.T) {
	if got := MissingProjectPermissions(nil); got != nil {
		t.Errorf("an unknown grant set is not evidence of a gap, got %v", got)
	}
	granted := map[string]string{"contents": "write", "metadata": "read"}
	got := MissingProjectPermissions(granted)
	if len(got) != 1 || got[0] != "organization_projects" {
		t.Errorf("MissingProjectPermissions = %v, want [organization_projects]", got)
	}
	granted["organization_projects"] = "write"
	if got := MissingProjectPermissions(granted); got != nil {
		t.Errorf("nothing is missing once the grant is present, got %v", got)
	}
}

// TestAppClientIsABoardClient pins the parity half: a GitHub-App connection
// must reach the board too, or the capability silently works on PATs only.
func TestAppClientIsABoardClient(t *testing.T) {
	var admin forge.Admin = &AppClient{}
	if _, ok := forge.AsBoardClient(admin); !ok {
		t.Fatal("github AppClient must implement forge.BoardClient")
	}
}

// countingMintServer answers the mint endpoint and every GraphQL call, keeping
// a tally of each. It is the oracle for "how many round trips does one pass
// actually cost".
type countingMintServer struct {
	srv *httptest.Server
	// mu guards the tallies: httptest serves concurrent requests, so a test
	// that exercises the client from N goroutines would otherwise race in its
	// own oracle.
	mu         sync.Mutex
	mints      int
	mintPerms  []map[string]string
	graphQL    int
	expiresAt  func() time.Time
	mintedName func(int) string
}

func (c *countingMintServer) counts() (mints, graphQL int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mints, c.graphQL
}

// permsAt returns the permission set of the n-th mint (0-based).
func (c *countingMintServer) permsAt(n int) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= len(c.mintPerms) {
		return nil
	}
	return c.mintPerms[n]
}

func newCountingMintServer(t *testing.T) *countingMintServer {
	t.Helper()
	c := &countingMintServer{
		expiresAt:  func() time.Time { return time.Now().UTC().Add(time.Hour) },
		mintedName: func(n int) string { return fmt.Sprintf("ghs_board_%d", n) },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/app/installations/99/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Permissions map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		c.mu.Lock()
		c.mints++
		c.mintPerms = append(c.mintPerms, req.Permissions)
		name, exp := c.mintedName(c.mints), c.expiresAt()
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": name, "expires_at": exp.Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.graphQL++
		c.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"organization": map[string]any{"projectV2": map[string]any{
				"id": "PVT_p", "number": 203, "title": "Iterion", "url": "u",
				"fields": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
			}},
		}})
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

// TestAppClientCachesTheBoardToken pins that a board pass mints ONE token.
//
// projectsREST minted a fresh organization_projects:write token for every
// BoardClient method, so a paged pass cost 1 mint for GetProject plus 1 per
// item page plus 1 per reflected card — each an extra HTTPS round trip with an
// RS256-signed App JWT, repeated on the binding's interval forever, against an
// endpoint GitHub rate-limits for abuse. It also widened the pass duration the
// lease TTL has to cover.
func TestAppClientCachesTheBoardToken(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	c := newCountingMintServer(t)
	app := &AppClient{HTTP: c.srv.Client(), WebBaseURL: c.srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}, InstallationID: 99}

	const calls = 5
	for i := 0; i < calls; i++ {
		if _, err := app.GetProject(context.Background(), forge.ProjectRef{Owner: "SocialGouv", Number: 203}); err != nil {
			t.Fatalf("GetProject %d: %v", i, err)
		}
	}
	mints, graphQL := c.counts()
	if graphQL != calls {
		t.Fatalf("graphQL calls = %d, want %d", graphQL, calls)
	}
	if mints != 1 {
		t.Errorf("mints = %d for %d board calls, want 1 — a pass must not pay a token round trip per call", mints, calls)
	}
	if got := c.permsAt(0)["organization_projects"]; got != "write" {
		t.Errorf("the board token was minted with %v, want organization_projects:write", c.permsAt(0))
	}
}

// A token near expiry is re-minted rather than served, so a long pass never
// carries a token that dies mid-way.
func TestAppClientReMintsAnExpiringBoardToken(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	c := newCountingMintServer(t)
	// Inside the leeway: every call must re-mint.
	c.expiresAt = func() time.Time { return time.Now().UTC().Add(2 * time.Minute) }
	app := &AppClient{HTTP: c.srv.Client(), WebBaseURL: c.srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}, InstallationID: 99}

	for i := 0; i < 3; i++ {
		if _, err := app.GetProject(context.Background(), forge.ProjectRef{Owner: "SocialGouv", Number: 203}); err != nil {
			t.Fatalf("GetProject %d: %v", i, err)
		}
	}
	if mints, _ := c.counts(); mints != 3 {
		t.Errorf("mints = %d, want 3 — a token about to expire must not be reused", mints)
	}
}

// The board cache is scoped to its permission set, so the broad
// organization_projects grant can never be served to an ordinary call — the
// whole reason the board path does not use the runtime token.
func TestAppClientBoardTokenNeverServesTheRuntimeCall(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	c := newCountingMintServer(t)
	c.srv.Config.Handler.(*http.ServeMux).HandleFunc("/api/v3/repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]any{})
	})
	app := &AppClient{HTTP: c.srv.Client(), WebBaseURL: c.srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}, InstallationID: 99}

	if _, err := app.GetProject(context.Background(), forge.ProjectRef{Owner: "SocialGouv", Number: 203}); err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if _, err := app.ListHooks(context.Background(), "o/r"); err != nil {
		t.Fatalf("ListHooks: %v", err)
	}
	if mints, _ := c.counts(); mints != 2 {
		t.Fatalf("mints = %d, want 2 — the board token must not serve a runtime call", mints)
	}
	if _, ok := c.permsAt(1)["organization_projects"]; ok {
		t.Errorf("the runtime token was minted with the board grant: %v", c.permsAt(1))
	}
}

// Concurrent board calls mint ONCE. A pass reflects cards one at a time today,
// but the client is shared per connection and the sync worker runs a tenant
// per goroutine — an unguarded cache would let N callers race into N mints,
// which is the very amplification the cache removes.
func TestAppClientBoardTokenMintsOnceUnderConcurrency(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	c := newCountingMintServer(t)
	app := &AppClient{HTTP: c.srv.Client(), WebBaseURL: c.srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}, InstallationID: 99}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := app.GetProject(context.Background(), forge.ProjectRef{Owner: "SocialGouv", Number: 203}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("GetProject: %v", err)
	}
	if mints, _ := c.counts(); mints != 1 {
		t.Errorf("mints = %d for %d concurrent board calls, want 1", mints, workers)
	}
}
