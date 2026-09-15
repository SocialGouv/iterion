package bots

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// orphanFragments reports, for each bot directory, the .bot files under its
// lib/ that the bot's unit does not reach: a fragment nothing imports is
// dead text that the catalog would otherwise ship and nobody would compile.
func orphanFragments(botDirs []string) []string {
	var orphans []string
	for _, dir := range botDirs {
		mainBot := filepath.Join(dir, "main.bot")
		if _, err := os.Stat(mainBot); err != nil {
			continue
		}
		reached := map[string]bool{}
		for _, f := range unit.LoadDir(mainBot).Files {
			reached[f.Rel] = true
		}
		lib := filepath.Join(dir, unit.FragmentDir)
		_ = filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".bot") {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			if !reached[filepath.ToSlash(rel)] {
				orphans = append(orphans, filepath.ToSlash(filepath.Join(filepath.Base(dir), rel)))
			}
			return nil
		})
	}
	sort.Strings(orphans)
	return orphans
}

// TestEveryFragmentIsImported: every lib/*.bot of a catalog bot or an
// example is reached by that bot's imports.
func TestEveryFragmentIsImported(t *testing.T) {
	teamDirs, _ := filepath.Glob("*/")
	demoDirs, _ := filepath.Glob("../examples/*/")
	if orphans := orphanFragments(append(teamDirs, demoDirs...)); len(orphans) > 0 {
		t.Fatalf("fragments nothing imports: %v", orphans)
	}
}

// TestOrphanFragmentsAreReported proves the check bites: a fragment beside
// the imported one, imported by nobody, is named.
func TestOrphanFragmentsAreReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")
	for rel, src := range map[string]string{
		"main.bot":       "import \"lib/nodes.bot\"\n\nworkflow w:\n  entry: a\n  a -> done\n",
		"lib/nodes.bot":  "agent a:\n  model: \"test\"\n",
		"lib/orphan.bot": "agent b:\n  model: \"test\"\n",
	} {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := orphanFragments([]string{dir}); len(got) != 1 || got[0] != "demo/lib/orphan.bot" {
		t.Fatalf("orphans %v, want demo/lib/orphan.bot alone", got)
	}
}
