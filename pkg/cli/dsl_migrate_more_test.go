package cli

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// A --floor that cannot be ordered is refused before anything is planned:
// an operator who typed a floor asked for one.
func TestMigrateDSLRefusesAFloorItCannotOrder(t *testing.T) {
	dir := writeMigrateBundle(t)
	for _, flag := range []string{"latest", "banana", "3.2.0-rc1"} {
		_, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, Floor: flag})
		if err == nil || !strings.Contains(err.Error(), "--floor") || !strings.Contains(err.Error(), flag) {
			t.Fatalf("--floor %q: err = %v", flag, err)
		}
	}
	bot, _ := os.ReadFile(filepath.Join(dir, "main.bot"))
	if string(bot) != migrateFixtureBot {
		t.Fatalf("a file was written under a refused flag")
	}
}

// A build with no version of its own writes the release that reads the
// target profile — never nothing on a bundle it just moved to profile 2.
func TestMigrateDSLOnADevBuildWritesTheProfilesRelease(t *testing.T) {
	prev := appinfo.Version
	t.Cleanup(func() { appinfo.Version = prev })
	appinfo.Version = "dev"
	dir := writeMigrateBundle(t)
	res, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Manifests) != 1 || !res.Manifests[0].Written || res.Manifests[0].To != ">= 3.141.0" {
		t.Fatalf("manifests: %+v", res.Manifests)
	}
	if m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml")); err != nil || m.Requires == nil || m.Requires.Iterion != ">= 3.141.0" {
		t.Fatalf("manifest: %v %+v", err, m)
	}
}

// A bundle the run did not change keeps its floor: `--check` on a migrated
// tree stays green across engine releases, and a real run never locks a
// bundle out of engines that read it.
func TestMigrateDSLLeavesAnUnchangedBundlesFloorAlone(t *testing.T) {
	dir := writeMigrateBundle(t)
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("dsl: 2\n"+migrateFixtureBot), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []MigrateDSLOptions{{Check: true}, {}} {
		opts.Paths = []string{dir}
		opts.Floor = "3.200.0"
		res, err := MigrateDSL(opts)
		if err != nil {
			t.Fatalf("%+v: %v", opts, err)
		}
		if len(res.Manifests) != 0 || res.Files[0].Changed {
			t.Fatalf("an unchanged bundle was floored: %+v %+v", res.Files, res.Manifests)
		}
	}
	man, _ := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if string(man) != migrateFixtureManifest {
		t.Fatalf("manifest changed:\n%s", man)
	}
}

// A floor already above the requested one is kept: lowering a declared
// floor is never this command's to do.
func TestMigrateDSLKeepsAHigherFloor(t *testing.T) {
	dir := writeMigrateBundle(t)
	high := strings.Replace(migrateFixtureManifest, `">= 1.0.0"`, `">= 9.0.0"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(high), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, Floor: "3.200.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Manifests) != 1 || res.Manifests[0].Written || !strings.Contains(res.Manifests[0].Skipped, "already at or above") {
		t.Fatalf("manifests: %+v", res.Manifests)
	}
	man, _ := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if string(man) != high {
		t.Fatalf("a higher floor was lowered:\n%s", man)
	}
}

// A child migrated on its own raises the manifest of the bundle that OWNS
// it, not one beside it; a bundle known by its skills/ alone, with no
// manifest, is reported rather than floored in silence or refused.
func TestMigrateDSLFloorsTheOwningBundle(t *testing.T) {
	dir := writeMigrateBundle(t)
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("dsl: 2\n"+migrateFixtureBot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "kids"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "kids", "c.bot")
	if err := os.WriteFile(child, []byte("agent a:\n  description: \"a\\tb\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateDSL(MigrateDSLOptions{Paths: []string{child}, Floor: "3.200.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Manifests) != 1 || !res.Manifests[0].Written || res.Manifests[0].Path != filepath.Join(dir, "manifest.yaml") {
		t.Fatalf("the owning bundle's manifest was not raised: %+v", res.Manifests)
	}

	noManifest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(noManifest, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noManifest, "main.bot"), []byte(migrateFixtureBot), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = MigrateDSL(MigrateDSLOptions{Paths: []string{noManifest}, Floor: "3.200.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Manifests) != 1 || res.Manifests[0].Written || !strings.Contains(res.Manifests[0].Skipped, "no manifest") {
		t.Fatalf("a manifest-less bundle: %+v", res.Manifests)
	}
	if _, err := MigrateDSL(MigrateDSLOptions{Paths: []string{noManifest}, Floor: "3.200.0", Check: true}); err != nil {
		t.Fatalf("check after migrating a manifest-less bundle: %v", err)
	}
}

// The walk never descends into a hidden tree: `.claude/worktrees`, `.works`
// and `.repos` hold other checkouts of the same files.
func TestCollectBotFilesSkipsHiddenTrees(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{".claude/w/x.bot", ".works/other/c.bot", ".repos/r/d.bot", "a.bot", "lib/b.bot", "vendor/v.bot"} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("agent a:\n  description: \"x\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := collectBotFiles([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "a.bot"), filepath.Join(dir, "lib", "b.bot")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	// Named as a path, a hidden tree is migrated: the rule is about the walk.
	got, err = collectBotFiles([]string{filepath.Join(dir, ".works")})
	if err != nil || len(got) != 1 {
		t.Fatalf("named hidden dir: %v %v", got, err)
	}
	if _, err := collectBotFiles([]string{filepath.Join(dir, "nope")}); !errors.Is(err, os.ErrNotExist) && err == nil {
		t.Fatalf("a missing path is an error")
	}
}
