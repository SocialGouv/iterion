package unit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// fileOf is the unit's entry for one relative path.
func fileOf(u *Unit, rel string) (File, bool) {
	for _, f := range u.Files {
		if f.Rel == rel {
			return f, true
		}
	}
	return File{}, false
}

// A unit whose main is handed over parsed (an author document's AST) is
// held to the loader's rules as a text main is: its fragments are read
// beside mainPath under lib/, a fragment loaded alone below a lib/ is read
// from the bot's root so its sibling imports resolve, a main with a
// workflow stays where it is, the main's positions and diagnostics carry
// mainName, and the digest covers the source the caller gave.
func TestLoadDirWithMainASTHoldsTheLoadersRules(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, src string) {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lib/helper.bot", "schema h:\n  ok: bool\n")
	write("lib/shared.bot", "import \"helper.bot\"\n\nschema s:\n  ok: bool\n")

	parse := func(name, src string) *parser.ParseResult {
		pr := parser.Parse(name, src)
		if len(pr.Diagnostics) > 0 {
			t.Fatalf("%s: %v", name, pr.Diagnostics)
		}
		return pr
	}

	t.Run("a main beside lib reads its fragments there", func(t *testing.T) {
		pr := parse("main.bot.yaml", "import \"lib/shared.bot\"\n\nagent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> done\n")
		u := LoadDirWithMainAST(filepath.Join(root, "main.bot"), "main.bot.yaml", pr.File, []byte("the yaml"))
		if u.HasErrors() {
			t.Fatalf("diagnostics: %v", u.Diagnostics)
		}
		if len(u.Files) != 3 || u.Main != "main.bot" || u.Files[0].Name != "main.bot.yaml" {
			t.Fatalf("files %d, main %q, main's name %q", len(u.Files), u.Main, u.Files[0].Name)
		}
		if string(u.Files[0].Source) != "the yaml" {
			t.Errorf("the main's source is %q, want the document's text (the digest covers it)", u.Files[0].Source)
		}
		if len(u.Merged.Schemas) != 2 {
			t.Errorf("merged %d schemas, want the two fragments'", len(u.Merged.Schemas))
		}
	})

	t.Run("a fragment alone below lib is read from the bot's root", func(t *testing.T) {
		pr := parse("shared.bot.yaml", "import \"helper.bot\"\n\nschema s:\n  ok: bool\n")
		u := LoadDirWithMainAST(filepath.Join(lib, "shared.bot"), "shared.bot.yaml", pr.File, []byte("y"))
		if u.HasErrors() {
			t.Fatalf("the sibling import is refused: %v", u.Diagnostics)
		}
		if u.Root != root || u.Main != "lib/shared.bot" || len(u.Files) != 2 {
			t.Fatalf("root %q, main %q, %d files — want the bot's root, lib/shared.bot, 2 files", u.Root, u.Main, len(u.Files))
		}
	})

	t.Run("a main with a workflow below lib stays where it is", func(t *testing.T) {
		pr := parse("odd.bot.yaml", "agent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n  a -> done\n")
		u := LoadDirWithMainAST(filepath.Join(lib, "odd.bot"), "odd.bot.yaml", pr.File, []byte("y"))
		if u.Root != lib || u.Main != "odd.bot" {
			t.Fatalf("root %q, main %q — want lib itself and odd.bot", u.Root, u.Main)
		}
	})

	t.Run("each file's parse diagnostics are its own, and the handed-over main has none", func(t *testing.T) {
		write("lib/bad.bot", "agent \n  model\n")
		pr := parse("main.bot.yaml", "import \"lib/bad.bot\"\n\nworkflow w:\n  entry: done\n")
		u := LoadDirWithMainAST(filepath.Join(root, "main.bot"), "main.bot.yaml", pr.File, []byte("the yaml"))
		// The caller parsed the main — from YAML, which the parser never
		// read — so there is no parse of its own text to report, whatever
		// the fragment it imports does.
		if len(u.Files[0].ParseDiagnostics) != 0 {
			t.Errorf("the handed-over main carries %v", u.Files[0].ParseDiagnostics)
		}
		bad, ok := fileOf(u, "lib/bad.bot")
		if !ok {
			t.Fatalf("the fragment was not loaded: %v", u.Files)
		}
		if len(bad.ParseDiagnostics) == 0 {
			t.Error("the fragment that does not parse carries no parse diagnostics")
		}
		for _, d := range bad.ParseDiagnostics {
			if d.File != bad.Name {
				t.Errorf("the fragment's diagnostic is positioned at %q, want %q", d.File, bad.Name)
			}
		}
	})

	t.Run("a fragment's error names the main by mainName", func(t *testing.T) {
		pr := parse("main.bot.yaml", "import \"lib/missing.bot\"\n\nworkflow w:\n  entry: a\n")
		u := LoadDirWithMainAST(filepath.Join(root, "main.bot"), "main.bot.yaml", pr.File, []byte("y"))
		if !u.HasErrors() {
			t.Fatal("a missing fragment is not refused")
		}
		if d := u.Diagnostics[0]; d.File != "main.bot.yaml" || !strings.Contains(d.Message, "missing.bot") {
			t.Errorf("the diagnostic is at %q: %s — want the document's name", d.File, d.Message)
		}
	})
}
