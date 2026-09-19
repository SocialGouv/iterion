package cloudpublisher

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pluginsource"
)

// twoSourceStore is the minimum Store the resolver needs to return two
// enabled sources for one tenant. The memory store cannot be used here (its
// Validate refuses a local git origin) — same reason the quarantine test
// rolls its own.
type twoSourceStore struct {
	pluginsource.Store
	srcs []pluginsource.PluginSource
}

func (t *twoSourceStore) ListEnabledByTenant(context.Context, string) ([]pluginsource.PluginSource, error) {
	return t.srcs, nil
}

// A store that never marks/clears degraded — success path here.
func (t *twoSourceStore) MarkDegraded(context.Context, string, string, string) error { return nil }
func (t *twoSourceStore) ClearDegraded(context.Context, string, string) error        { return nil }

// initSkillRepo makes a git repo at `origin` holding `plugin.yaml` and one
// skill file at skills/<name>.md with the given body. Returns the tag ref
// callers pin their source at.
func initSkillRepo(t *testing.T, origin, name, body string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Same trap as plugin_source_quarantine_test.go: `git commit` detaches a
	// maintenance run that writes into .git/objects AFTER the command
	// returns, into a temp dir cleanup is about to remove.
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
	if err := os.WriteFile(filepath.Join(origin, "skills", "deploy.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: " + name + "\nversion: 1.0.0\ndescription: dedup test\ncontributes:\n  skills:\n    - skills/deploy.md\n"
	if err := os.WriteFile(filepath.Join(origin, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "init")
	run("tag", "v0.1.0")
	return "v0.1.0"
}

// #1374 root cause: `resolveContributionsFor` step 0 (team-scoped git-hosted
// plugin sources) appended unconditionally. Two enabled team sources
// shipping `skills/deploy.md` both rode the payload — one destination on the
// runner, and the runner's mirror order silently decided which team's file
// landed. Fix: dedup step 0 with `replaceContribution` and log the
// shadowing.
//
// Mutation: swap `replaceContribution` back for an unconditional append and
// the len(payload.Plugin)==1 assertion goes red — two entries ride the
// message.
func TestResolveContributionsFor_DedupsMultipleTeamSourcesSharingAName(t *testing.T) {
	// Isolation: without this, plugin.Load() reads the operator's real
	// ~/.iterion/plugins and can inject a step-1 contribution named
	// "deploy.md" that would collide with what we're asserting on step 0.
	// The class of tests that need this is #1374's adjacent — this one
	// applies it deliberately for the assertion.
	t.Setenv("ITERION_HOME", t.TempDir())

	originA := t.TempDir()
	initSkillRepo(t, originA, "team-a-deploy", "FROM SOURCE A\n")
	originB := t.TempDir()
	initSkillRepo(t, originB, "team-a-deploy-v2", "FROM SOURCE B\n")

	store := &twoSourceStore{srcs: []pluginsource.PluginSource{
		{ID: "s-a", TenantID: "team-a", Name: "team-a-deploy", GitURL: originA, Ref: "v0.1.0", Enabled: true},
		{ID: "s-b", TenantID: "team-a", Name: "team-a-deploy-v2", GitURL: originB, Ref: "v0.1.0", Enabled: true},
	}}
	resolver := &pluginsource.Resolver{Store: store, Fetcher: &pluginsource.Fetcher{CacheDir: t.TempDir()}}

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &buf)
	got, err := resolveContributionsFor(context.Background(), nil, t.TempDir(), "team-a", "run-1", resolver, logger)
	if err != nil {
		t.Fatalf("resolveContributionsFor: %v", err)
	}
	if got == nil {
		t.Fatal("no contributions returned")
	}
	// One (kind, name) entry on the payload; the runner's mirror sees ONE
	// destination and cannot pick a winner by order.
	count := 0
	var winner []byte
	for _, f := range got.Plugin {
		if f.Kind == "skills" && f.Name == "deploy.md" {
			count++
			winner = f.Content
		}
	}
	if count != 1 {
		t.Fatalf("payload carries %d entries for (skills, deploy.md), want 1 — team sources are not deduped", count)
	}
	// The LATER source's content wins — that mirrors the stable write order
	// a redelivery would replay, and it is what the docstring commits to.
	if string(winner) != "FROM SOURCE B\n" {
		t.Errorf("winning content = %q, want the later source's body", winner)
	}
	// The substitution is not silent.
	if logs := buf.String(); !strings.Contains(logs, "contributed by more than one enabled team source") || !strings.Contains(logs, "deploy.md") {
		t.Errorf("shadowing not warned about; logs = %q", logs)
	}
}

// A step-0 team-hosted contribution and a step-1 locally installed plugin
// sharing (kind, name) should still resolve to ONE payload entry, with the
// local plugin winning — the pre-existing invariant #1372 established and
// this fix must preserve.
func TestResolveContributionsFor_LocalPluginStillShadowsTeamSource(t *testing.T) {
	// Local plugin: point ITERION_HOME at a scratch tree with the plugin
	// pre-installed and enabled by default_enabled: true.
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	pluginDir := filepath.Join(home, "plugins", "local-deploy")
	if err := os.MkdirAll(filepath.Join(pluginDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "skills", "deploy.md"), []byte("FROM LOCAL PLUGIN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "name: local-deploy\nversion: 1.0.0\nschema_version: 1\ndefault_enabled: true\ncontributes:\n  skills:\n    - skills/deploy.md\n"
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// One team source shipping the same (kind, name).
	origin := t.TempDir()
	initSkillRepo(t, origin, "team-deploy", "FROM TEAM SOURCE\n")
	store := &twoSourceStore{srcs: []pluginsource.PluginSource{
		{ID: "s", TenantID: "team-a", Name: "team-deploy", GitURL: origin, Ref: "v0.1.0", Enabled: true},
	}}
	resolver := &pluginsource.Resolver{Store: store, Fetcher: &pluginsource.Fetcher{CacheDir: t.TempDir()}}

	logger := iterlog.New(iterlog.LevelError, io.Discard)
	got, err := resolveContributionsFor(context.Background(), nil, t.TempDir(), "team-a", "run-1", resolver, logger)
	if err != nil {
		t.Fatalf("resolveContributionsFor: %v", err)
	}
	if got == nil {
		t.Fatal("no contributions returned")
	}
	count := 0
	var winner []byte
	for _, f := range got.Plugin {
		if f.Kind == "skills" && f.Name == "deploy.md" {
			count++
			winner = f.Content
		}
	}
	if count != 1 {
		t.Fatalf("payload carries %d entries for (skills, deploy.md), want 1", count)
	}
	if string(winner) != "FROM LOCAL PLUGIN\n" {
		t.Errorf("winning content = %q, want the LOCAL plugin's body (step 1 shadows step 0)", winner)
	}
}

// A team source shipping content byte-identical to the incumbent's is still
// deduped — the payload stays at one entry — and the shadow warning fires
// (the operator has two sources shipping the same file, which they should
// know to rename). Documents the wire behaviour without asserting a silent
// no-op.
func TestResolveContributionsFor_DedupsEvenWhenTeamSourceContentIsIdentical(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	originA := t.TempDir()
	initSkillRepo(t, originA, "team-a-deploy", "SAME BODY\n")
	originB := t.TempDir()
	initSkillRepo(t, originB, "team-a-deploy-v2", "SAME BODY\n")

	store := &twoSourceStore{srcs: []pluginsource.PluginSource{
		{ID: "s-a", TenantID: "team-a", Name: "team-a-deploy", GitURL: originA, Ref: "v0.1.0", Enabled: true},
		{ID: "s-b", TenantID: "team-a", Name: "team-a-deploy-v2", GitURL: originB, Ref: "v0.1.0", Enabled: true},
	}}
	resolver := &pluginsource.Resolver{Store: store, Fetcher: &pluginsource.Fetcher{CacheDir: t.TempDir()}}
	got, err := resolveContributionsFor(context.Background(), nil, t.TempDir(), "team-a", "run-1", resolver, nil)
	if err != nil {
		t.Fatalf("resolveContributionsFor: %v", err)
	}
	count := 0
	for _, f := range got.Plugin {
		if f.Kind == "skills" && f.Name == "deploy.md" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("payload carries %d entries for (skills, deploy.md), want 1 even on identical content", count)
	}
}

// F4 of the round-1 adversarial review: "later wins" step-0 dedup was
// depending on `Store.ListEnabledByTenant` iteration order — MongoStore
// sorts by created_at ASC (deterministic in prod) but MemoryStore iterates
// a map (random). A stub returning slice-order passes but does not commit
// to a rule.
//
// The fix (pluginsource.Resolver.Resolve): stable sort by (CreatedAt ASC,
// ID ASC) before dedup, so "later CreatedAt wins" is enforced by the
// resolver on any Store. This test feeds a stub whose ListEnabledByTenant
// returns the newer source FIRST (reversed) and asserts the older-then-
// newer sort holds → the newer source's bytes win.
//
// Mutation: remove the sort.SliceStable in pluginsource/resolve.go and this
// test goes red.
func TestResolveContributionsFor_TeamSourcesAreSortedByCreatedAtBeforeDedup(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())

	originOld := t.TempDir()
	initSkillRepo(t, originOld, "team-a-deploy", "OLDER SOURCE (should be shadowed)\n")
	originNew := t.TempDir()
	initSkillRepo(t, originNew, "team-a-deploy-v2", "NEWER SOURCE (must win)\n")

	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Return them in the REVERSED order (newer first) — a naive iteration
	// would pick the older as the "later" and get it wrong.
	store := &twoSourceStore{srcs: []pluginsource.PluginSource{
		{ID: "s-new", TenantID: "team-a", Name: "team-a-deploy-v2", GitURL: originNew, Ref: "v0.1.0", Enabled: true, CreatedAt: newTime},
		{ID: "s-old", TenantID: "team-a", Name: "team-a-deploy", GitURL: originOld, Ref: "v0.1.0", Enabled: true, CreatedAt: oldTime},
	}}
	resolver := &pluginsource.Resolver{Store: store, Fetcher: &pluginsource.Fetcher{CacheDir: t.TempDir()}}

	got, err := resolveContributionsFor(context.Background(), nil, t.TempDir(), "team-a", "run-1", resolver, nil)
	if err != nil {
		t.Fatalf("resolveContributionsFor: %v", err)
	}
	count := 0
	var winner []byte
	for _, f := range got.Plugin {
		if f.Kind == "skills" && f.Name == "deploy.md" {
			count++
			winner = f.Content
		}
	}
	if count != 1 {
		t.Fatalf("payload carries %d entries for (skills, deploy.md), want 1", count)
	}
	if string(winner) != "NEWER SOURCE (must win)\n" {
		t.Errorf("winning content = %q, want the source with the later CreatedAt", winner)
	}
}
