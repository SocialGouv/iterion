package unit

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const pinnedModel = "anthropic/claude-opus-4-8"

// A fragment loaded alone — `iterion validate lib/nodes.bot`, the editor
// opening it — is read from the bot's root, under its lib/ name: a sibling
// it imports by bare name resolves as it does through the main, and the
// unit says where it is validated instead of refusing a legal import.
func TestAFragmentLoadedAloneIsReadFromTheBotsRoot(t *testing.T) {
	dir := t.TempDir()
	for rel, src := range fixture {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u := LoadDir(filepath.Join(dir, "lib", "nodes.bot"))
	if u.HasErrors() {
		t.Fatalf("a fragment importing its sibling by bare name is refused when loaded alone: %v", u.Diagnostics)
	}
	var rels []string
	for _, f := range u.Files {
		rels = append(rels, f.Rel)
	}
	if !reflect.DeepEqual(rels, []string{"lib/nodes.bot", "lib/schemas.bot"}) || u.Main != "lib/nodes.bot" {
		t.Fatalf("files %v, main %q", rels, u.Main)
	}
	if u.Root != dir {
		t.Fatalf("root %q, want the bot's directory %q", u.Root, dir)
	}
	if len(u.Merged.Workflows) != 0 || len(u.Merged.Agents) != 1 || len(u.Merged.Schemas) != 1 {
		t.Fatalf("merged: %d workflows, %d agents, %d schemas", len(u.Merged.Workflows), len(u.Merged.Agents), len(u.Merged.Schemas))
	}
	// The document of that fragment, saved: the staged text is keyed from
	// the fragment's directory, as LoadDirStaged documents, and lands on it.
	staged := LoadDirWithMain(filepath.Join(dir, "lib", "nodes.bot"), "", []byte("import \"schemas.bot\"\n\nagent c:\n  model: \""+pinnedModel+"\"\n  description: \"c\"\n  output: s\n"))
	if staged.HasErrors() || len(staged.Merged.Agents) != 1 || staged.Merged.Agents[0].Name != "c" {
		t.Fatalf("the staged text of a fragment loaded alone was not taken as its main: %v %+v", staged.Diagnostics, staged.Merged.Agents)
	}
}

// The file the caller names may be a symlink — a bot linked into a
// workspace or a catalog read as it always has, by following it; only the
// files an import reaches are held to the no-symlink rule.
func TestTheMainMayBeASymlinkTheFragmentsMayNot(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(filepath.Join(real, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "main.bot"), []byte("agent a:\n  model: \""+pinnedModel+"\"\n  description: \"d\"\n\nworkflow w:\n  entry: a\n  a -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.bot")
	if err := os.Symlink(filepath.Join(real, "main.bot"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if u := LoadDir(link); u.HasErrors() || len(u.Files) != 1 || len(u.Merged.Workflows) != 1 {
		t.Fatalf("a symlinked .bot no longer loads: %v", u.Diagnostics)
	}
	// A unit whose main is a symlink reads its fragments beside the LINK,
	// as named; a fragment that is itself a symlink stays refused.
	unitDir := filepath.Join(dir, "unit")
	if err := os.MkdirAll(filepath.Join(unitDir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "lib", "f.bot"), []byte("schema s:\n  ok: bool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "unit-main.bot"), []byte("import \"lib/f.bot\"\n\nagent a:\n  model: \""+pinnedModel+"\"\n  description: \"d\"\n  output: s\n\nworkflow w:\n  entry: a\n  a -> done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "unit-main.bot"), filepath.Join(unitDir, "main.bot")); err != nil {
		t.Fatal(err)
	}
	if u := LoadDir(filepath.Join(unitDir, "main.bot")); u.HasErrors() || len(u.Files) != 2 {
		t.Fatalf("a symlinked main with fragments beside the link: %v (%d files)", u.Diagnostics, len(u.Files))
	}
	if err := os.Symlink(filepath.Join(dir, "unit", "lib", "f.bot"), filepath.Join(real, "lib", "f.bot")); err != nil {
		t.Fatal(err)
	}
	u := LoadDir(filepath.Join(real, "unit-main.bot"))
	if !u.HasErrors() || !strings.Contains(u.Diagnostics[0].Message, "symlink") {
		t.Fatalf("a symlinked fragment was read: %v", u.Diagnostics)
	}
}

// A files map whose main sits below the root — a bundle nested in a
// collection — keeps its fragments under ITS lib/.
func TestLoadMapReadsTheFragmentsBesideANestedMain(t *testing.T) {
	files := map[string]string{
		"bots/x/main.bot":  "import \"lib/a.bot\"\n\nworkflow w:\n  entry: a\n  a -> done\n",
		"bots/x/lib/a.bot": "agent a:\n  model: \"" + pinnedModel + "\"\n  description: \"d\"\n",
	}
	u := LoadMap(files, "bots/x/main.bot")
	if u.HasErrors() || len(u.Files) != 2 || u.Files[1].Rel != "bots/x/lib/a.bot" {
		t.Fatalf("nested main: %v (%d files)", u.Diagnostics, len(u.Files))
	}
	// A path to another bot's lib/ stays outside.
	files["bots/x/main.bot"] = "import \"../y/lib/a.bot\"\n"
	if u := LoadMap(files, "bots/x/main.bot"); !u.HasErrors() {
		t.Fatalf("an import leaving the bot's lib/ was accepted")
	}
	for _, mainRel := range []string{"main.bot", "lib/nodes.bot", "bots/x/main.bot", "bots/x/lib/n.bot"} {
		want := map[string]string{"main.bot": "lib/", "lib/nodes.bot": "lib/", "bots/x/main.bot": "bots/x/lib/", "bots/x/lib/n.bot": "bots/x/lib/"}[mainRel]
		if got := fragmentPrefixOf(mainRel); got != want {
			t.Errorf("fragmentPrefixOf(%q) = %q, want %q", mainRel, got, want)
		}
	}
}

// The canonical source of a subbot and its authored form are inverses: a
// save by provenance puts back what the fragment said, whatever the
// fragment's depth, and an absolute source travels untouched.
func TestSubbotSourceCanonicalAndAuthoredAreInverses(t *testing.T) {
	for _, c := range []struct{ owner, written, canonical string }{
		{"main.bot", "kids/k.bot", "kids/k.bot"},
		{"lib/nodes.bot", "kids/k.bot", "lib/kids/k.bot"},
		{"lib/nodes.bot", "../other/main.bot", "other/main.bot"},
		{"lib/deep/n.bot", "k.bot", "lib/deep/k.bot"},
		{"lib/nodes.bot", "/abs/k.bot", "/abs/k.bot"},
		{"lib/nodes.bot", "", ""},
	} {
		if got := CanonicalSubbotSource(c.owner, c.written); got != c.canonical {
			t.Errorf("Canonical(%q, %q) = %q, want %q", c.owner, c.written, got, c.canonical)
		}
		if got := AuthoredSubbotSource(c.owner, c.canonical); got != c.written {
			t.Errorf("Authored(%q, %q) = %q, want %q", c.owner, c.canonical, got, c.written)
		}
	}
}
