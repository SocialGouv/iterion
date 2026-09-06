package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
)

// The 2026-08-26 incident: a team registered a plugin source whose plugin.yaml
// carried an unquoted `: ` in a value. The POST accepted it without looking,
// and the parse error surfaced only at the next launches — every one of the
// team's webhook launches, for 2h22. Registration now materialises the source
// exactly as a launch would (clone, parse the manifest, read the
// contributions) and refuses one it cannot, with the parser's own error in
// the body, while the operator is right there to read it.

const brokenPluginManifest = "name: deploy-onyxia\nversion: 0.1.0\ndescription: deploy: to onyxia\ncontributes:\n  skills:\n    - skills/deploy.md\n"
const fixedPluginManifest = "name: deploy-onyxia\nversion: 0.1.1\ndescription: \"deploy: to onyxia\"\ncontributes:\n  skills:\n    - skills/deploy.md\n"

func gitRunIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", gitlib.NoAutoMaintenance(args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// bareOriginWithManifest builds a one-skill plugin repo tagged v0.1.0 with the
// broken manifest and v0.1.1 with the fixed one, then clones it bare under
// root/<name> so git's http-backend can serve it.
func bareOriginWithManifest(t *testing.T, root, name string) {
	t.Helper()
	work := t.TempDir()
	gitRunIn(t, work, "init", "--quiet", "-b", "main")
	if err := os.MkdirAll(filepath.Join(work, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "skills", "deploy.md"), []byte("# deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "plugin.yaml"), []byte(brokenPluginManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, work, "add", "-A")
	gitRunIn(t, work, "commit", "-m", "broken manifest")
	gitRunIn(t, work, "tag", "v0.1.0")
	if err := os.WriteFile(filepath.Join(work, "plugin.yaml"), []byte(fixedPluginManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunIn(t, work, "add", "-A")
	gitRunIn(t, work, "commit", "-m", "quote the description")
	gitRunIn(t, work, "tag", "v0.1.1")
	gitRunIn(t, root, "clone", "--quiet", "--bare", work, filepath.Join(root, name))
}

// serveGitOverHTTP serves the bare repositories under root with git's own
// http-backend through the standard CGI adapter, so a source can be
// registered under the http(s):// URL the validator requires — a local path is
// refused by design (a tenant must not point a source at the server's disk).
func serveGitOverHTTP(t *testing.T, root string) *httptest.Server {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	execPath, err := exec.Command(gitBin, "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git http-backend not available: %v", err)
	}
	srv := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(srv.Close)
	return srv
}

func TestPluginSource_RegistrationVerifiesTheManifest(t *testing.T) {
	root := t.TempDir()
	git := serveGitOverHTTP(t, root)
	bareOriginWithManifest(t, root, "deploy.git")

	s := newOrgTestServer(t)
	s.pluginSources = pluginsource.NewMemoryStore()
	s.pluginSourceFetcher = &pluginsource.Fetcher{CacheDir: t.TempDir()}
	seedTeam(t, s, "t1", "acme")
	admin := seedTeamMember(t, s, context.Background(), "ad", identity.RoleAdmin)
	ctx := auth.WithIdentity(context.Background(), admin)

	do := func(method, path, body string, h http.HandlerFunc, sourceID string) *httptest.ResponseRecorder {
		t.Helper()
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		}
		r = r.WithContext(ctx)
		r.SetPathValue("id", "t1")
		if sourceID != "" {
			r.SetPathValue("source_id", sourceID)
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w
	}
	repoURL := git.URL + "/deploy.git"

	// An unparseable manifest is refused at the door, with the parser's
	// error in the body, and nothing is persisted.
	w := do("POST", "/api/teams/t1/plugin-sources",
		`{"name":"deploy-onyxia","git_url":"`+repoURL+`","ref":"v0.1.0","enabled":true}`,
		s.handleCreatePluginSource, "")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST of a source whose plugin.yaml does not parse = %d (%s), want 422", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "parse") || !strings.Contains(body, "yaml") {
		t.Fatalf("the refusal must carry the YAML error so the operator can fix the file; got %s", body)
	}
	if list, _ := s.pluginSources.ListByTenant(context.Background(), "t1"); len(list) != 0 {
		t.Fatalf("a refused source must not be persisted; found %d", len(list))
	}

	// The same repo at the fixed tag registers, and reads healthy.
	w = do("POST", "/api/teams/t1/plugin-sources",
		`{"name":"deploy-onyxia","git_url":"`+repoURL+`","ref":"v0.1.1","enabled":true}`,
		s.handleCreatePluginSource, "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST of a valid source = %d (%s), want 200", w.Code, w.Body.String())
	}
	var created struct {
		ID       string `json:"id"`
		Ref      string `json:"ref"`
		Degraded bool   `json:"degraded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create response: %v %s", err, w.Body.String())
	}
	if created.Degraded {
		t.Fatalf("a source that just verified must not read degraded: %s", w.Body.String())
	}

	// Re-pinning it onto the broken tag is refused too, and the record keeps
	// the ref that works.
	w = do("PATCH", "/api/teams/t1/plugin-sources/"+created.ID, `{"ref":"v0.1.0"}`, s.handleUpdatePluginSource, created.ID)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH onto a broken ref = %d (%s), want 422", w.Code, w.Body.String())
	}
	if got, err := s.pluginSources.Get(context.Background(), created.ID); err != nil || got.Ref != "v0.1.1" {
		t.Fatalf("a refused PATCH must leave the record as it was: %+v (%v)", got, err)
	}

	// A launch that had to skip the source leaves it flagged; the flag is on
	// the API for the studio to show, and a PATCH that re-verifies clears it.
	if err := s.pluginSources.MarkDegraded(context.Background(), "t1", created.ID, "pluginsource: fetch: remote hung up"); err != nil {
		t.Fatal(err)
	}
	w = do("GET", "/api/teams/t1/plugin-sources", "", s.handleListPluginSources, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"degraded":true`) ||
		!strings.Contains(w.Body.String(), "remote hung up") {
		t.Fatalf("the list must expose the degraded flag and its reason: %d %s", w.Code, w.Body.String())
	}
	w = do("PATCH", "/api/teams/t1/plugin-sources/"+created.ID, `{"ref":"v0.1.1"}`, s.handleUpdatePluginSource, created.ID)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"degraded":true`) {
		t.Fatalf("a PATCH that re-verified the source must clear the flag: %d %s", w.Code, w.Body.String())
	}
	if got, _ := s.pluginSources.Get(context.Background(), created.ID); got.Degraded() {
		t.Fatalf("the cleared flag must be persisted, not just rendered: %+v", got)
	}
}
