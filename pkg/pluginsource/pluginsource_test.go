package pluginsource

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
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
		cmd := exec.Command("git", gitlib.NoAutoMaintenance(args...)...)
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

// A cold pod takes several launches at once, and every one of them fetches the
// same source. Each must be handed a COMPLETE tree: publishing the directory
// before the checkout lands (what `git init` in place does) hands the losers a
// bare .git, and the plugin loader then reports the tree as having no
// plugin.yaml — a launch-blocking 502 whose message names the wrong cause.
func TestFetcher_ConcurrentFetchesAllSeeCompleteTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	origin := gitOrigin(t, "v1.0.0")

	f := &Fetcher{CacheDir: t.TempDir()}
	s := validSource()
	s.GitURL, s.Ref = origin, "v1.0.0"

	const n = 12
	paths := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			paths[i], errs[i] = f.Fetch(context.Background(), s)
		}()
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("fetch %d: %v", i, errs[i])
		}
		body, err := os.ReadFile(filepath.Join(paths[i], "skills", "deploy-target.md"))
		if err != nil || string(body) != "playbook\n" {
			t.Fatalf("fetch %d was handed an incomplete checkout at %q: %v %q", i, paths[i], err, body)
		}
	}

	// No staging or retired leftovers: the cache holds exactly the checkout.
	entries, err := os.ReadDir(f.CacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache dir should hold only the checkout, got %v", names)
	}
}

// The in-process gate cannot help across processes, and the cache dir is
// shared (/tmp/iterion-plugin-sources): separate Fetchers over one dir are the
// case where only the staging+rename publish protects the reader. Building in
// place instead makes the racers collide inside the same .git.
func TestFetcher_SeparateFetchersSharingACacheDirNeverSeeAPartialTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	origin := gitOrigin(t, "v1.0.0")
	cache := t.TempDir()
	s := validSource()
	s.GitURL, s.Ref = origin, "v1.0.0"

	const n = 8
	paths := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := &Fetcher{CacheDir: cache} // a distinct fetcher: no shared gate
			<-start
			paths[i], errs[i] = f.Fetch(context.Background(), s)
		}()
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("fetch %d: %v", i, errs[i])
		}
		body, err := os.ReadFile(filepath.Join(paths[i], "skills", "deploy-target.md"))
		if err != nil || string(body) != "playbook\n" {
			t.Fatalf("fetch %d was handed an incomplete checkout at %q: %v %q", i, paths[i], err, body)
		}
	}
}

// A waiter must be able to give up. Each attempt is bounded by FetchTimeout, so
// a queue of waiters on a hung remote burns it once per waiter — and the launch
// that wanted the plugin may be long gone.
func TestFetcher_WaiterHonoursCancellation(t *testing.T) {
	f := &Fetcher{CacheDir: t.TempDir()}
	release, err := f.lockKey(context.Background(), "some-key")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Waited for rather than called inline: the regression is a block, not a
	// wrong return, and a test that hangs says much less than one that fails.
	waited := make(chan error, 1)
	go func() {
		release, err := f.lockKey(ctx, "some-key")
		if release != nil {
			release()
		}
		waited <- err
	}()
	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("a cancelled waiter took the gate instead of giving up")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled waiter queued behind the in-flight fetch instead of giving up")
	}
}

// A moving ref re-fetches, and the new tree must replace the old one wholesale
// rather than being assembled in place under a concurrent reader's feet.
func TestFetcher_MovingRefSwapsInTheNewTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	origin := gitOrigin(t, "")

	f := &Fetcher{CacheDir: t.TempDir()}
	s := validSource()
	s.GitURL, s.Ref = origin, "main"

	first, err := f.Fetch(context.Background(), s)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if err := os.WriteFile(filepath.Join(origin, "skills", "deploy-target.md"), []byte("revised\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, origin, "add", "-A")
	gitRun(t, origin, "commit", "-m", "revise")

	second, err := f.Fetch(context.Background(), s)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if second != first {
		t.Errorf("cache path changed across a moving-ref refresh: %q vs %q", second, first)
	}
	body, err := os.ReadFile(filepath.Join(second, "skills", "deploy-target.md"))
	if err != nil || string(body) != "revised\n" {
		t.Fatalf("moving ref did not pick up the new commit: %v %q", err, body)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", gitlib.NoAutoMaintenance(args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// gitOrigin builds a one-commit plugin repo, tagged when tag is non-empty.
func gitOrigin(t *testing.T, tag string) string {
	t.Helper()
	origin := t.TempDir()
	gitRun(t, origin, "init", "--quiet", "-b", "main")
	if err := os.MkdirAll(filepath.Join(origin, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "skills", "deploy-target.md"), []byte("playbook\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, origin, "add", "-A")
	gitRun(t, origin, "commit", "-m", "init")
	if tag != "" {
		gitRun(t, origin, "tag", tag)
	}
	return origin
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
		cmd := exec.Command("git", gitlib.NoAutoMaintenance(args...)...)
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
	files, skipped, err := r.Resolve(ctx, "team-1")
	if err != nil || len(skipped) != 0 {
		t.Fatalf("resolve: %v (skipped %d)", err, len(skipped))
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1: %+v", len(files), files)
	}
	if files[0].Kind != "skills" || files[0].Name != "deploy-target.md" || string(files[0].Content) != "# playbook\n" {
		t.Errorf("unexpected file: %+v", files[0])
	}
}

// brokenManifestOrigin builds a plugin repo whose plugin.yaml does not parse
// (an unquoted `: ` inside a value — the exact shape of the 2026-08-26
// incident), tagged v0.1.0, plus a v0.1.1 tag that fixes it.
func brokenManifestOrigin(t *testing.T) string {
	t.Helper()
	origin := t.TempDir()
	gitRun(t, origin, "init", "--quiet", "-b", "main")
	if err := os.MkdirAll(filepath.Join(origin, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "skills", "deploy.md"), []byte("# deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	broken := "name: deploy-onyxia\nversion: 0.1.0\ndescription: deploy: to onyxia\ncontributes:\n  skills:\n    - skills/deploy.md\n"
	if err := os.WriteFile(filepath.Join(origin, "plugin.yaml"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, origin, "add", "-A")
	gitRun(t, origin, "commit", "-m", "broken manifest")
	gitRun(t, origin, "tag", "v0.1.0")
	fixed := "name: deploy-onyxia\nversion: 0.1.1\ndescription: \"deploy: to onyxia\"\ncontributes:\n  skills:\n    - skills/deploy.md\n"
	if err := os.WriteFile(filepath.Join(origin, "plugin.yaml"), []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, origin, "add", "-A")
	gitRun(t, origin, "commit", "-m", "quote the description")
	gitRun(t, origin, "tag", "v0.1.1")
	return origin
}

// One team's broken plugin.yaml made EVERY launch of that team fail for 2h22
// (2026-08-26): the resolver failed the whole launch on the first source it
// could not load. A source that cannot be materialised is now SKIPPED for
// the launch — the run proceeds with the sources that work — and the skip is
// recorded on the source (degraded + reason) so nothing about it is silent.
// The flag clears on the next resolution that succeeds.
func TestResolver_BrokenManifestIsQuarantinedNotFatal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	st := NewMemoryStore()
	healthy := validSource()
	healthy.Name, healthy.GitURL, healthy.Ref = "healthy-skills", gitOrigin(t, "v1.0.0"), "v1.0.0"
	healthy = seed(t, st, healthy)
	broken := validSource()
	broken.Name, broken.GitURL, broken.Ref = "deploy-onyxia", brokenManifestOrigin(t), "v0.1.0"
	broken = seed(t, st, broken)

	r := &Resolver{Store: st, Fetcher: &Fetcher{CacheDir: t.TempDir()}}
	files, skipped, err := r.Resolve(ctx, "team-1")
	if err != nil {
		t.Fatalf("one broken source failed the whole resolution: %v", err)
	}
	if len(files) != 1 || files[0].Name != "deploy-target.md" {
		t.Fatalf("the healthy source's files must still ship: %+v", files)
	}
	if len(skipped) != 1 || skipped[0].Source.ID != broken.ID || skipped[0].Err == nil ||
		!strings.Contains(skipped[0].Err.Error(), "parse") {
		t.Fatalf("the broken source must be reported skipped with its parse error: %+v", skipped)
	}
	got, err := st.Get(ctx, broken.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Degraded() || !strings.Contains(got.DegradedReason, "parse") || got.DegradedAt == nil {
		t.Fatalf("the skipped source must read degraded with the reason on its record: %+v", got)
	}
	if h, _ := st.Get(ctx, healthy.ID); h.Degraded() {
		t.Fatalf("the healthy source must not be flagged: %+v", h)
	}

	// The operator fixes the manifest and re-pins (re-seeded rather than
	// Updated: Validate refuses the local origin, and the flag must survive
	// the re-pin so that clearing it is the resolver's doing): the next
	// launch resolves it and clears the flag by itself.
	got.Ref = "v0.1.1"
	seed(t, st, got)
	if again, _ := st.Get(ctx, broken.ID); !again.Degraded() {
		t.Fatal("fixture: the re-pin must not clear the flag by itself")
	}
	files, skipped, err = r.Resolve(ctx, "team-1")
	if err != nil || len(skipped) != 0 || len(files) != 2 {
		t.Fatalf("after the fix: files=%d skipped=%d err=%v, want 2/0/nil", len(files), len(skipped), err)
	}
	if got, _ := st.Get(ctx, broken.ID); got.Degraded() {
		t.Fatalf("a successful resolution must clear the flag: %+v", got)
	}
}

// An unfetchable source (unreachable remote, wrong ref) is the same class as
// an unparseable one: skipped and flagged, never fatal for the team's launches.
func TestResolver_UnfetchableSourceIsSkippedAndFlagged(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	s := validSource()
	s.GitURL, s.Ref = t.TempDir(), "v1.0.0" // not a git repo
	s = seed(t, st, s)
	r := &Resolver{Store: st, Fetcher: &Fetcher{CacheDir: t.TempDir()}}
	files, skipped, err := r.Resolve(ctx, "team-1")
	if err != nil {
		t.Fatalf("an unfetchable source must not fail the launch: %v", err)
	}
	if len(files) != 0 || len(skipped) != 1 {
		t.Fatalf("files=%d skipped=%d, want 0/1", len(files), len(skipped))
	}
	if got, _ := st.Get(ctx, s.ID); !got.Degraded() || !strings.Contains(got.DegradedReason, "fetch") {
		t.Fatalf("the source must read degraded with the fetch failure: %+v", got)
	}
}

// A source list that cannot be READ is not a broken source: nothing can tell a
// healthy team from one whose sources are all lost, so that still fails the
// launch with the cause.
func TestResolver_UnlistableStoreIsAnError(t *testing.T) {
	r := &Resolver{Store: unlistableStore{}, Fetcher: &Fetcher{CacheDir: t.TempDir()}}
	if _, _, err := r.Resolve(context.Background(), "team-1"); err == nil {
		t.Fatal("a store that cannot list the team's sources must fail the resolution explicitly")
	}
}

type unlistableStore struct{ Store }

func (unlistableStore) ListEnabledByTenant(context.Context, string) ([]PluginSource, error) {
	return nil, context.DeadlineExceeded
}

// Health is the engine's readout: an operator's Update (rename, re-pin,
// toggle) never rewrites it — only Mark/ClearDegraded do, and only for the
// owning tenant.
func TestMemoryStore_DegradedIsOwnedByTheResolver(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	s := validSource()
	if err := st.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	list, _ := st.ListByTenant(ctx, "team-1")
	s = list[0]

	if err := st.MarkDegraded(ctx, "team-2", s.ID, "not yours"); err != ErrNotFound {
		t.Fatalf("another tenant must not flag this source: %v", err)
	}
	if err := st.MarkDegraded(ctx, "team-1", s.ID, "fetch refused"); err != nil {
		t.Fatal(err)
	}
	s.Ref = "v1.0.1"
	if err := st.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, s.ID)
	if got.Ref != "v1.0.1" || got.DegradedReason != "fetch refused" || got.DegradedAt == nil {
		t.Fatalf("Update must apply the operator's fields and leave the health readout alone: %+v", got)
	}
	if err := st.ClearDegraded(ctx, "team-1", s.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(ctx, s.ID); got.Degraded() || got.DegradedAt != nil {
		t.Fatalf("ClearDegraded must clear both fields: %+v", got)
	}
	if err := st.ClearDegraded(ctx, "team-1", "nope"); err != ErrNotFound {
		t.Fatalf("clearing an unknown id = %v, want ErrNotFound", err)
	}
}
