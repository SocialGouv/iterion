package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrateDSLStopsAtTheRepositoryRoot reproduces the #1367 attribution:
// `dsl migrate --check bots/` on a repository with no bundle anywhere
// handed bots/smoke/board_smoke.bot to a directory of the operator's ABOVE
// the repository — taken for a bundle because it held a skills/ — and
// would have raised THAT manifest's floor. The upward bundle search stops
// at the repository root: the file is loose, no manifest is attributed,
// and the operator's manifest is left byte-identical. (Hermetic: the whole
// home/repo tree is a temp dir; the real $HOME is never touched.)
func TestMigrateDSLStopsAtTheRepositoryRoot(t *testing.T) {
	tmp := t.TempDir()
	// A directory of the operator's above the repository, marked as a
	// bundle — main.bot, skills/ and a manifest with a floor of its own.
	opDir := filepath.Join(tmp, "home")
	opManifest := "schema_version: 1\nname: operator\ndescription: Not the repository's.\nrequires:\n  iterion: \">= 1.0.0\"\n"
	if err := os.MkdirAll(filepath.Join(opDir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opDir, "main.bot"), []byte("workflow op:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(opDir, "manifest.yaml"), []byte(opManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// The repository, with a loose workflow under its bots/ and no bundle.
	repo := filepath.Join(opDir, "lab", "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(repo, "bots", "smoke", "board_smoke.bot")
	if err := os.MkdirAll(filepath.Dir(loose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte("## a loose workflow\nagent a:\n  description: \"x\"\n\nworkflow w:\n  entry: a\n  a -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := MigrateDSL(MigrateDSLOptions{Paths: []string{filepath.Join(repo, "bots")}, Floor: "3.200.0", Printer: &Printer{W: &out, Format: OutputHuman}})
	if err != nil {
		t.Fatalf("MigrateDSL: %v\n%s", err, out.String())
	}
	if len(res.Files) != 1 || !strings.Contains(res.Files[0].Loose, "no bundle to carry requires.iterion") {
		t.Fatalf("files: %+v — the loose file was attributed above its repository", res.Files)
	}
	if len(res.Manifests) != 0 {
		t.Fatalf("a manifest outside the repository was attributed: %+v", res.Manifests)
	}
	if man, _ := os.ReadFile(filepath.Join(opDir, "manifest.yaml")); string(man) != opManifest {
		t.Fatalf("the operator's manifest was touched:\n%s", man)
	}

	// A bundle above the file inside the SAME repository is still its
	// bundle: only the crossing is refused.
	writeBundle := func(dir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(migrateFixtureBot), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(migrateFixtureManifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBundle(filepath.Join(repo, "bots"))
	// The first run migrated the loose file; hand the second an unmigrated one.
	other := filepath.Join(repo, "bots", "smoke", "other.bot")
	if err := os.WriteFile(other, []byte("## a loose workflow\nagent b:\n  description: \"y\"\n\nworkflow w:\n  entry: b\n  b -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = MigrateDSL(MigrateDSLOptions{Paths: []string{other}, Floor: "3.200.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Manifests) != 1 || res.Manifests[0].Path != filepath.Join(repo, "bots", "manifest.yaml") || !res.Manifests[0].Written {
		t.Fatalf("the bundle above the file inside the repository was not floored: %+v", res.Manifests)
	}
}
