package botregistry

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const unitMainSrc = "import \"lib/vars.bot\"\n\nworkflow w:\n  entry: a\n  a -> done\n"

// TestLoadSchema_FollowsAFragmentEditAtSameSizeAndMtime: the launch form's
// vars come from the whole unit, and the cache follows the unit's DIGEST —
// an edit to a fragment does not touch the main, and an edit of the same
// size at a restored mtime touches nothing a stat can see.
func TestLoadSchema_FollowsAFragmentEditAtSameSizeAndMtime(t *testing.T) {
	ClearSchemaCache()
	dir := t.TempDir()
	mainBot := filepath.Join(dir, "main.bot")
	fragment := filepath.Join(dir, "lib", "vars.bot")
	writeFile(t, mainBot, unitMainSrc)
	writeFile(t, fragment, "vars:\n  depth: int = 1\n\nagent a:\n  model: \"test\"\n")
	e := Entry{Path: mainBot, Name: "unit"}

	vars, _, err := LoadSchema(e)
	if err != nil || vars == nil || len(vars.Fields) != 1 || vars.Fields[0].Name != "depth" {
		t.Fatalf("vars from the fragment: %+v err=%v", vars, err)
	}
	info, err := os.Stat(fragment)
	if err != nil {
		t.Fatal(err)
	}
	// Same length, same mtime: only the bytes differ.
	writeFile(t, fragment, "vars:\n  width: int = 1\n\nagent a:\n  model: \"test\"\n")
	if err := os.Chtimes(fragment, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	vars, _, err = LoadSchema(e)
	if err != nil || vars == nil || len(vars.Fields) != 1 || vars.Fields[0].Name != "width" {
		t.Fatalf("after the same-size edit at a restored mtime: %+v err=%v", vars, err)
	}
}

// TestList_SkipsAFragmentsDirectoryBesideAMain: a bot in several files is
// one bot; its lib/ holds parts of it, never bots of their own.
func TestList_SkipsAFragmentsDirectoryBesideAMain(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "demo", "main.bot"), unitMainSrc)
	writeFile(t, filepath.Join(dir, "demo", "lib", "vars.bot"), "vars:\n  depth: int = 1\n\nagent a:\n  model: \"test\"\n")
	// A lib/ with no workflow file beside it is an ordinary directory.
	writeFile(t, filepath.Join(dir, "lib", "loose.bot"), "agent z:\n  model: \"test\"\n")
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		rel, _ := filepath.Rel(dir, e.Path)
		paths = append(paths, filepath.ToSlash(rel))
	}
	sort.Strings(paths)
	if len(paths) != 2 || paths[0] != "demo/main.bot" || paths[1] != "lib/loose.bot" {
		t.Fatalf("discovered %v, want the main and the loose file only", paths)
	}
}
