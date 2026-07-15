package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/marketplace"
)

func writeSeedBundle(t *testing.T, dir, name, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	man := "name: " + name + "\nversion: " + version + "\ndescription: seed bot\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePluginSource(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	man := "name: " + name + "\nversion: 0.1.0\ndescription: test plugin\ncontributes:\n  skills:\n    - skills/foo.md\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "foo.md"), []byte("---\nname: foo\ndescription: d\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMarketplaceCLI_SubmitInstallUninstall_KindAware(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ITERION_HOME", t.TempDir())
	storeDir := t.TempDir()
	workdir := t.TempDir()

	botRepo := t.TempDir()
	writeSeedBundle(t, botRepo, "mybot", "1.0.0")
	pluginRepo := t.TempDir()
	writePluginSource(t, pluginRepo, "my-plugin")

	// Submit detects the kind of each source.
	botEntry, err := MarketplaceSubmit(ctx, MarketplaceSubmitOptions{StoreDir: storeDir, Source: botRepo})
	if err != nil {
		t.Fatalf("submit bot: %v", err)
	}
	if marketplace.EffectiveKind(*botEntry) != marketplace.KindBot {
		t.Errorf("bot kind = %q", botEntry.Kind)
	}
	pluginEntry, err := MarketplaceSubmit(ctx, MarketplaceSubmitOptions{StoreDir: storeDir, Source: pluginRepo})
	if err != nil {
		t.Fatalf("submit plugin: %v", err)
	}
	if pluginEntry.Kind != marketplace.KindPlugin {
		t.Errorf("plugin kind = %q", pluginEntry.Kind)
	}

	// --kind filtering.
	if entries, err := MarketplaceList(ctx, MarketplaceListOptions{StoreDir: storeDir, Kind: "plugin"}); err != nil || len(entries) != 1 || entries[0].Slug != "my-plugin" {
		t.Errorf("list kind=plugin → %v, %v", entries, err)
	}
	if _, err := MarketplaceList(ctx, MarketplaceListOptions{StoreDir: storeDir, Kind: "gizmo"}); err == nil {
		t.Error("list kind=gizmo should error")
	}

	// Bot install/uninstall targets the workspace .botz/.
	res, err := MarketplaceInstall(ctx, MarketplaceInstallOptions{StoreDir: storeDir, Slug: "mybot", Workdir: workdir})
	if err != nil {
		t.Fatalf("install bot: %v", err)
	}
	if res.Kind != marketplace.KindBot || res.Bot == nil || res.Plugin != "" {
		t.Errorf("bot install result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "mybot", "main.bot")); err != nil {
		t.Fatalf("bot not installed: %v", err)
	}
	if _, err := MarketplaceUninstall(ctx, MarketplaceUninstallOptions{StoreDir: storeDir, Slug: "mybot", Workdir: workdir}); err != nil {
		t.Fatalf("uninstall bot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".botz", "mybot")); !os.IsNotExist(err) {
		t.Errorf("bot bundle still present: %v", err)
	}

	// Plugin install/uninstall targets ITERION_HOME/plugins/.
	res, err = MarketplaceInstall(ctx, MarketplaceInstallOptions{StoreDir: storeDir, Slug: "my-plugin"})
	if err != nil {
		t.Fatalf("install plugin: %v", err)
	}
	if res.Kind != marketplace.KindPlugin || res.Plugin != "my-plugin" || res.Bot != nil {
		t.Errorf("plugin install result = %+v", res)
	}
	pluginDir := filepath.Join(os.Getenv("ITERION_HOME"), "plugins", "my-plugin")
	if _, err := os.Stat(filepath.Join(pluginDir, "plugin.yaml")); err != nil {
		t.Fatalf("plugin not installed: %v", err)
	}
	if _, err := MarketplaceUninstall(ctx, MarketplaceUninstallOptions{StoreDir: storeDir, Slug: "my-plugin"}); err != nil {
		t.Fatalf("uninstall plugin: %v", err)
	}
	if _, err := os.Stat(pluginDir); !os.IsNotExist(err) {
		t.Errorf("plugin dir still present: %v", err)
	}
}

func TestSeedMarketplace_Idempotent_NoClobber(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	bots := filepath.Join(ws, "bots")
	writeSeedBundle(t, filepath.Join(bots, "alpha"), "alpha", "1.0.0")
	writeSeedBundle(t, filepath.Join(bots, "beta"), "beta", "1.0.0")

	store, err := marketplace.NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := SeedOptions{Paths: []string{"bots"}, Workdir: ws}

	// First seed indexes both bundles as builtin/approved/public.
	n, err := SeedMarketplace(ctx, store, opts)
	if err != nil || n != 2 {
		t.Fatalf("first seed: n=%d err=%v", n, err)
	}
	a, ok, _ := store.Get(ctx, "alpha")
	if !ok || a.Source != marketplace.SourceBuiltin || a.Status != marketplace.StatusApproved || a.Scope != marketplace.ScopePublic {
		t.Fatalf("alpha not seeded as builtin/approved/public: %+v", a)
	}

	// Reseed with no changes is a no-op.
	if n, _ := SeedMarketplace(ctx, store, opts); n != 0 {
		t.Fatalf("reseed wrote %d, want 0", n)
	}

	// A user entry (git source) with the same slug must never be clobbered.
	if err := store.Upsert(ctx, marketplace.Entry{
		Slug: "beta", Name: "beta", RepoURL: "https://example.com/beta.git",
		Source: marketplace.SourceGit, Version: "9.9.9", Installs: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := SeedMarketplace(ctx, store, opts); n != 0 {
		t.Fatalf("seed clobbered a user entry (wrote %d)", n)
	}
	b, _, _ := store.Get(ctx, "beta")
	if b.Source != marketplace.SourceGit || b.Version != "9.9.9" || b.Installs != 5 {
		t.Fatalf("user beta entry was overwritten: %+v", b)
	}

	// A version drift in a builtin bundle triggers exactly one update,
	// preserving the install counter.
	if err := store.IncrementInstalls(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	writeSeedBundle(t, filepath.Join(bots, "alpha"), "alpha", "2.0.0")
	if n, _ := SeedMarketplace(ctx, store, opts); n != 1 {
		t.Fatalf("version-drift reseed wrote %d, want 1", n)
	}
	a, _, _ = store.Get(ctx, "alpha")
	if a.Version != "2.0.0" {
		t.Errorf("alpha version not updated: %q", a.Version)
	}
	if a.Installs != 1 {
		t.Errorf("install counter not preserved across reseed: %d", a.Installs)
	}
}
