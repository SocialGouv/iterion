package botregistry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A unit that does not load — a fragment renamed, an import broken — is a
// SchemaError the launch form shows, never a bot that declares no vars.
func TestLoadSchema_ReportsABrokenImport(t *testing.T) {
	ClearSchemaCache()
	dir := t.TempDir()
	mainBot := filepath.Join(dir, "main.bot")
	writeFile(t, mainBot, unitMainSrc)
	writeFile(t, filepath.Join(dir, "lib", "vars.bot"), "vars:\n  depth: int = 1\n\nagent a:\n  model: \"test\"\n")
	e := Entry{Path: mainBot, Name: "unit"}
	if vars, _, err := LoadSchema(e); err != nil || vars == nil || len(vars.Fields) != 1 {
		t.Fatalf("baseline: %+v err=%v", vars, err)
	}
	writeFile(t, filepath.Join(dir, "lib", "other.bot"), "vars:\n  depth: int = 1\n")
	if err := os.Rename(filepath.Join(dir, "lib", "vars.bot"), filepath.Join(dir, "lib", "gone.bot")); err != nil {
		t.Fatal(err)
	}
	vars, _, err := LoadSchema(e)
	if err == nil || !strings.Contains(err.Error(), "E046") || vars != nil {
		t.Fatalf("a broken import read as a bot without vars: vars=%+v err=%v", vars, err)
	}
}

// A lib/ beside a main holds that bot's fragments, never bots of their
// own — but a bundle nested below it is still a bot, and is listed.
func TestList_FindsABundleNestedUnderLib(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "scratch.bot"), "workflow w:\n  entry: done\n")
	writeFile(t, filepath.Join(dir, "lib", "frag.bot"), "agent z:\n  model: \"test\"\n")
	writeFile(t, filepath.Join(dir, "lib", "deep", "frag.bot"), "agent y:\n  model: \"test\"\n")
	writeFile(t, filepath.Join(dir, "lib", "mybot", "main.bot"), "workflow w:\n  entry: done\n")
	writeFile(t, filepath.Join(dir, "lib", "mybot", "manifest.yaml"), "name: mybot\ndisplay_name: My\nschema_version: 1\n")
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
	if strings.Join(paths, " ") != "lib/mybot scratch.bot" {
		t.Fatalf("listed %v, want the loose bot and the bundle nested under lib/, no fragment", paths)
	}
}
