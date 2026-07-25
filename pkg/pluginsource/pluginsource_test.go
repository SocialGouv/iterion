package pluginsource

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validSource() PluginSource {
	return PluginSource{
		TenantID: "team-1", Name: "deploy-k8s-msociaux",
		GitURL: "https://example.test/org/plugin.git", Ref: "v1.0.0", Enabled: true,
	}
}

// seed inserts a source without going through Validate, which deliberately
// rejects local filesystem paths (a tenant must not be able to point a source
// at the server's disk). Tests need a local git origin, so they seed directly.
func seed(t *testing.T, m *MemoryStore, s PluginSource) PluginSource {
	t.Helper()
	if s.ID == "" {
		s.ID = s.TenantID + "-" + s.Name
	}
	s.CreatedAt = time.Now().UTC()
	s.UpdatedAt = s.CreatedAt
	m.byID[s.ID] = s
	return s
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PluginSource)
		want string
	}{
		{"ok", func(*PluginSource) {}, ""},
		{"no tenant", func(s *PluginSource) { s.TenantID = "" }, "tenant"},
		{"no name", func(s *PluginSource) { s.Name = "" }, "name is required"},
		{"bad name", func(s *PluginSource) { s.Name = "Bad/Name" }, "lowercase"},
		{"no url", func(s *PluginSource) { s.GitURL = "" }, "git_url is required"},
		{"bad url", func(s *PluginSource) { s.GitURL = "file:///etc" }, "must be http"},
		{"no ref", func(s *PluginSource) { s.Ref = "" }, "ref is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := validSource()
			c.mut(&s)
			err := s.Validate()
			if c.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error containing %q", err, c.want)
			}
		})
	}
}

// A pinned ref is what makes the cache immutable (and the launch network-free
// after the first fetch); a moving branch is the drift risk we warn about.
func TestPinnedRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"v1.0.0": true, "1.2.3": true,
		"3f786850e387550fdab836ed7e6dc881de23001b": true,
		"main": false, "develop": false, "feature-x": false,
	} {
		s := validSource()
		s.Ref = ref
		if got := s.PinnedRef(); got != want {
			t.Errorf("ref %q: PinnedRef=%v, want %v", ref, got, want)
		}
	}
}

func TestMemoryStore_CRUDAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	s := validSource()
	if err := st.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListByTenant(ctx, "team-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v (%d)", err, len(list))
	}
	got := list[0]
	if got.ID == "" || got.CreatedAt.IsZero() {
		t.Errorf("id/timestamps not stamped: %+v", got)
	}
	// Another team must not see it.
	if other, _ := st.ListByTenant(ctx, "team-2"); len(other) != 0 {
		t.Errorf("tenant isolation broken: %+v", other)
	}
	// Disabled sources are excluded from the launch-time list.
	got.Enabled = false
	if err := st.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if en, _ := st.ListEnabledByTenant(ctx, "team-1"); len(en) != 0 {
		t.Errorf("disabled source still listed as enabled: %+v", en)
	}
	if err := st.Delete(ctx, got.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, got.ID); err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestMemoryStore_NameConflictPerTenant(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	if err := st.Create(ctx, validSource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(ctx, validSource()); err != ErrNameConflict {
		t.Errorf("same name in same team: got %v, want ErrNameConflict", err)
	}
	// The same name in a DIFFERENT team is fine — that is the point of
	// org-scoping: two orgs may each bring their own "deploy-target".
	other := validSource()
	other.TenantID = "team-2"
	if err := st.Create(ctx, other); err != nil {
		t.Errorf("same name across teams must be allowed, got %v", err)
	}
}

func TestMemoryStore_ListRequiresTenant(t *testing.T) {
	if _, err := NewMemoryStore().ListByTenant(context.Background(), ""); err != ErrTenantMissing {
		t.Errorf("got %v, want ErrTenantMissing", err)
	}
}

// End-to-end against a real local git repo: the fetcher must materialise the
// plugin tree, and a pinned ref must be served from cache without touching the
// remote again (proved by deleting the origin between fetches).
func TestFetcher_FetchesAndCachesPinnedRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	origin := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(origin, "init", "--quiet", "-b", "main")
	if err := os.MkdirAll(filepath.Join(origin, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "skills", "deploy-target.md"), []byte("playbook\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(origin, "add", "-A")
	run(origin, "commit", "-m", "init")
	run(origin, "tag", "v1.0.0")

	f := &Fetcher{CacheDir: t.TempDir()}
	s := validSource()
	s.GitURL = origin
	s.Ref = "v1.0.0"

	path, err := f.Fetch(context.Background(), s)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(path, "skills", "deploy-target.md"))
	if err != nil || string(body) != "playbook\n" {
		t.Fatalf("plugin tree not materialised: %v %q", err, body)
	}

	// Destroy the origin: a pinned ref must now be served from cache.
	if err := os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	again, err := f.Fetch(context.Background(), s)
	if err != nil {
		t.Fatalf("pinned ref should hit the cache, got %v", err)
	}
	if again != path {
		t.Errorf("cache path changed: %q vs %q", again, path)
	}
}

func TestFetcher_MissingRefFailsLoudly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	f := &Fetcher{CacheDir: t.TempDir()}
	s := validSource()
	s.GitURL = t.TempDir() // a real dir, but not a git repo
	s.Ref = "v9.9.9"
	if _, err := f.Fetch(context.Background(), s); err == nil {
		t.Fatal("an unreachable source must fail explicitly, never silently contribute nothing")
	}
}

func TestRedact(t *testing.T) {
	if got := redact("token=s3cret failed", "s3cret"); strings.Contains(got, "s3cret") {
		t.Errorf("credential leaked into output: %q", got)
	}
}

// The whole point, end to end: a team's enabled source, hosted in a git repo,
// yields the skill files a run will mirror — with no dependency on any
// filesystem state that a pod restart could lose.
func TestResolver_ResolvesTeamSourcesFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	origin := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "--quiet", "-b", "main")
	if err := os.MkdirAll(filepath.Join(origin, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A bare skills/ repo (no plugin.yaml) — the shape LoadDir synthesizes.
	if err := os.WriteFile(filepath.Join(origin, "skills", "deploy-target.md"), []byte("# playbook\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "init")
	run("tag", "v1.0.0")

	ctx := context.Background()
	st := NewMemoryStore()
	s := validSource()
	s.GitURL, s.Ref = origin, "v1.0.0"
	seed(t, st, s)
	// A source of ANOTHER team must not leak in.
	other := validSource()
	other.TenantID, other.GitURL = "team-2", origin
	seed(t, st, other)

	r := &Resolver{Store: st, Fetcher: &Fetcher{CacheDir: t.TempDir()}}
	files, err := r.Resolve(ctx, "team-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1: %+v", len(files), files)
	}
	if files[0].Kind != "skills" || files[0].Name != "deploy-target.md" || string(files[0].Content) != "# playbook\n" {
		t.Errorf("unexpected file: %+v", files[0])
	}
}

// An enabled source that cannot be fetched must ERROR, not quietly contribute
// nothing — a run missing its platform playbook still "succeeds" while doing
// the wrong thing.
func TestResolver_UnfetchableSourceIsAnError(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	s := validSource()
	s.GitURL, s.Ref = t.TempDir(), "v1.0.0" // not a git repo
	seed(t, st, s)
	r := &Resolver{Store: st, Fetcher: &Fetcher{CacheDir: t.TempDir()}}
	if _, err := r.Resolve(ctx, "team-1"); err == nil {
		t.Fatal("expected an explicit error for an unfetchable enabled source")
	}
}
