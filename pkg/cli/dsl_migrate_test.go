package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

const migrateFixtureBot = "## the bot\nprompt p:\n  First.\n\n  Second.\n\nagent a:\n  system: p\n  description: \"a\\tb\"\n  memory:\n    enabled: true\n\nworkflow w:\n  entry: a\n  a -> done\n"

const migrateFixtureManifest = "schema_version: 1\nname: probe # the bot's id\ndescription: A probe.\nrequires:\n  iterion: \">= 1.0.0\"\n"

func writeMigrateBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(migrateFixtureBot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(migrateFixtureManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The command migrates the bundle's file and raises the manifest's engine
// floor to the given version through the shared writer, keeping the
// manifest's comment; the report names the prompt whose rendering changes.
func TestMigrateDSLWritesTheFileAndRaisesTheManifestFloor(t *testing.T) {
	dir := writeMigrateBundle(t)
	var out bytes.Buffer
	res, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, Floor: "v3.200.0", ShowPrompts: true, Printer: &Printer{W: &out, Format: OutputHuman}})
	if err != nil {
		t.Fatalf("MigrateDSL: %v\n%s", err, out.String())
	}
	if len(res.Files) != 1 || !res.Files[0].Written || len(res.Files[0].Prompts) != 1 {
		t.Fatalf("files: %+v", res.Files)
	}
	bot, _ := os.ReadFile(filepath.Join(dir, "main.bot"))
	if !strings.HasPrefix(string(bot), "## the bot\ndsl: 2\n\nprompt p:") || !strings.Contains(string(bot), `description: "a\\tb"`) {
		t.Fatalf("main.bot:\n%s", bot)
	}
	if len(res.Manifests) != 1 || !res.Manifests[0].Written || res.Manifests[0].To != ">= 3.200.0" || res.Manifests[0].From != ">= 1.0.0" {
		t.Fatalf("manifests: %+v", res.Manifests)
	}
	man, _ := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	// The writer keeps the comment; the quote style of the value is its own.
	if !strings.Contains(string(man), "# the bot's id") || !strings.Contains(string(man), ">= 3.200.0") {
		t.Fatalf("manifest.yaml:\n%s", man)
	}
	if m, err := bundle.LoadManifest(filepath.Join(dir, "manifest.yaml")); err != nil || m.Requires == nil || m.Requires.Iterion != ">= 3.200.0" {
		t.Fatalf("rewritten manifest does not load: %v %+v", err, m)
	}
	if !strings.Contains(out.String(), "prompt p (line 2): 1 blank line(s)") {
		t.Fatalf("report:\n%s", out.String())
	}
}

// Nothing is written under --dry-run or --check; --check fails when a
// file would change and passes once it has.
func TestMigrateDSLDryRunAndCheckWriteNothing(t *testing.T) {
	dir := writeMigrateBundle(t)
	for _, opts := range []MigrateDSLOptions{{DryRun: true}, {Check: true}} {
		opts.Paths = []string{dir}
		opts.Floor = "3.200.0"
		res, err := MigrateDSL(opts)
		if opts.Check {
			if !errors.Is(err, ErrWouldChange) {
				t.Fatalf("check: err = %v", err)
			}
		} else if err != nil {
			t.Fatalf("dry-run: %v", err)
		}
		if res.Files[0].Written || !res.Files[0].Changed || res.Manifests[0].Written || res.Manifests[0].To != ">= 3.200.0" {
			t.Fatalf("wrote under a no-write mode: %+v %+v", res.Files, res.Manifests)
		}
		bot, _ := os.ReadFile(filepath.Join(dir, "main.bot"))
		man, _ := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
		if string(bot) != migrateFixtureBot || string(man) != migrateFixtureManifest {
			t.Fatalf("files changed on disk")
		}
	}
	if _, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, Floor: "3.200.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, Floor: "3.200.0", Check: true}); err != nil {
		t.Fatalf("check after migration: %v", err)
	}
	// A loose file, no manifest: --check fails on the file alone.
	loose := t.TempDir()
	if err := os.WriteFile(filepath.Join(loose, "x.bot"), []byte("agent a:\n  description: \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateDSL(MigrateDSLOptions{Paths: []string{loose}, Check: true}); !errors.Is(err, ErrWouldChange) {
		t.Fatalf("check on a loose file: err = %v", err)
	}
}

// A refused file fails the run, and NOTHING is written — not the sound
// files planned before it, not a manifest: a tree is migrated whole or not
// at all.
func TestMigrateDSLWritesNothingWhenAFileIsRefused(t *testing.T) {
	dir := writeMigrateBundle(t)
	bad := filepath.Join(dir, "other.bot")
	if err := os.WriteFile(bad, []byte("agent a:\n  memory:\n    enabled: true\n    project_root: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := MigrateDSL(MigrateDSLOptions{Paths: []string{dir}, Floor: "3.200.0"})
	if err == nil || !strings.Contains(err.Error(), "1 file(s) refused") || !strings.Contains(err.Error(), "nothing was written") || !strings.Contains(err.Error(), "project_root") {
		t.Fatalf("err = %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Written || !res.Files[0].Changed {
		t.Fatalf("files: %+v", res.Files)
	}
	if len(res.Manifests) != 0 {
		t.Fatalf("a manifest was touched: %+v", res.Manifests)
	}
	bot, _ := os.ReadFile(filepath.Join(dir, "main.bot"))
	man, _ := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if string(bot) != migrateFixtureBot || string(man) != migrateFixtureManifest {
		t.Fatalf("something was written despite the refusal")
	}
}
