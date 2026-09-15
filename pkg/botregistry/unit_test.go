package botregistry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const unitMainSrc = "import \"lib/vars.bot\"\n\nworkflow w:\n  entry: a\n  a -> done\n"

// TestLoadSchema_FollowsAFragmentEdit: the launch form's vars come from
// the whole unit, and the cache stats every file of it — an edit to a
// fragment does not touch the main — while a hit reads nothing: an edit
// of the same size at a restored mtime is invisible, as it always was
// for a single file, which is what makes a hit cost a stat per file and
// not a parse of the catalog.
func TestLoadSchema_FollowsAFragmentEdit(t *testing.T) {
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
	// The fragment edited — a longer text, a later mtime: the miss reads.
	writeFile(t, fragment, "vars:\n  width: int = 1\n  depth: int = 2\n\nagent a:\n  model: \"test\"\n")
	vars, _, err = LoadSchema(e)
	if err != nil || vars == nil || len(vars.Fields) != 2 {
		t.Fatalf("after the fragment edit: %+v err=%v", vars, err)
	}
	names := map[string]bool{}
	for _, f := range vars.Fields {
		names[f.Name] = true
	}
	if !names["width"] || !names["depth"] {
		t.Fatalf("after the fragment edit: fields %v", names)
	}
	// A hit reads nothing: the same bytes count at a restored mtime is
	// served from the cache — even garbage — as a single file always was.
	info, err := os.Stat(fragment)
	if err != nil {
		t.Fatal(err)
	}
	garbage := strings.Repeat("?", int(info.Size()))
	writeFile(t, fragment, garbage)
	if err := os.Chtimes(fragment, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	vars, _, err = LoadSchema(e)
	if err != nil || vars == nil || len(vars.Fields) != 2 {
		t.Fatalf("a hit read the files (vars=%+v err=%v): the catalog is parsed on every listing", vars, err)
	}
	// A later mtime at the same size is an edit: the garbage is read.
	later := info.ModTime().Add(10 * time.Second)
	if err := os.Chtimes(fragment, later, later); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSchema(e); err == nil {
		t.Fatal("a same-size edit with a later mtime was served from the cache")
	}
	// A different size at the same mtime is an edit too: the new text is read
	// — never the error the cache holds.
	writeFile(t, fragment, "vars:\n  width: int = 1\n  depth: int = 2\n  height: int = 3\n\nagent a:\n  model: \"test\"\n")
	if err := os.Chtimes(fragment, later, later); err != nil {
		t.Fatal(err)
	}
	vars, _, err = LoadSchema(e)
	if err != nil || vars == nil || len(vars.Fields) != 3 {
		t.Fatalf("a different-size edit at the same mtime was served from the cache: %+v err=%v", vars, err)
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
