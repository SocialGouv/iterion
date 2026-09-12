package migrate

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The migration removes exactly the directive comments profile 2 refuses
// (E042: the top-level ones, wherever they sit), and nothing else: a
// comment inside a block is never a directive and stays, a directive after
// code on a top-level line goes without its line, and a trailing comment
// that is not the directive stays where it was. Every result parses in
// profile 2, and none of these used to: two of them crashed the migrator on
// overlapping edits, one deleted a line of code.
func TestMigrationRemovesOnlyTheDirectivesProfileTwoRefuses(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			"a block comment spelt like the directive stays",
			"agent a:\n  ## strict-escape: on\n  description: \"x\"\n\nworkflow w:\n  entry: a\n  a -> done\n",
			"dsl: 2\n\nagent a:\n  ## strict-escape: on\n  description: \"x\"\n\nworkflow w:\n  entry: a\n  a -> done\n",
		},
		{
			"a trailing directive on a literal's line stays, the literal is re-spelled",
			"tool t:\n  command: \"a\\nb\"   ## strict-escape: on\n\nworkflow w:\n  entry: t\n  t -> done\n",
			"dsl: 2\n\ntool t:\n  command: \"a\\\\nb\"   ## strict-escape: on\n\nworkflow w:\n  entry: t\n  t -> done\n",
		},
		{
			"a trailing directive on the header's line goes, the header stays on its line",
			"dsl: 1   ## strict-escape: on\ntool t:\n  command: \"x\"\n\nworkflow w:\n  entry: t\n  t -> done\n",
			"dsl: 2\ntool t:\n  command: \"x\"\n\nworkflow w:\n  entry: t\n  t -> done\n",
		},
		{
			"a trailing comment on the header's line that is not the directive stays",
			"dsl: 1  ## pinned until rollout\ntool t:\n  command: \"x\"\n\nworkflow w:\n  entry: t\n  t -> done\n",
			"dsl: 2  ## pinned until rollout\ntool t:\n  command: \"x\"\n\nworkflow w:\n  entry: t\n  t -> done\n",
		},
		{
			// The parser reads a column-1 comment right under a block as the
			// block's: not a top-level comment, so not E042's, so kept.
			"a directive-looking comment attached to the block above stays",
			"## note\ntool t:\n  command: \"x\"\n## strict-escape: on\n\nworkflow w:\n  entry: t\n  t -> done\n",
			"## note\ndsl: 2\n\ntool t:\n  command: \"x\"\n## strict-escape: on\n\nworkflow w:\n  entry: t\n  t -> done\n",
		},
		{
			// Even past a blank line: the block closes at the next
			// declaration, and the comment belongs to what is open.
			"a directive-looking comment between declarations stays too",
			"## note\ntool t:\n  command: \"x\"\n\n## strict-escape: on\n\nworkflow w:\n  entry: t\n  t -> done\n",
			"## note\ndsl: 2\n\ntool t:\n  command: \"x\"\n\n## strict-escape: on\n\nworkflow w:\n  entry: t\n  t -> done\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mustMigrate(t, c.src)
			if string(res.Migrated) != c.want {
				t.Fatalf("migrated:\n%s\nwant:\n%s", res.Migrated, c.want)
			}
			after := parser.Parse("x.bot", string(res.Migrated))
			if len(after.Diagnostics) != 0 || after.File.EffectiveProfile() != 2 {
				t.Fatalf("the migrated file does not read as profile 2: %v", after.Diagnostics)
			}
		})
	}
}

// Every spelling the lexer accepts for the directive is removed.
func TestMigrationRemovesEverySpellingOfTheDirective(t *testing.T) {
	for _, spelling := range []string{"## strict-escape: on", "## strict-escape:on", "# strict-escape = on"} {
		res := mustMigrate(t, spelling+"\ntool t:\n  command: \"a\\nb\"\n\nworkflow w:\n  entry: t\n  t -> done\n")
		if strings.Contains(string(res.Migrated), "strict-escape") || !strings.HasPrefix(string(res.Migrated), "dsl: 2\n\ntool t:") {
			t.Fatalf("%q was not removed:\n%s", spelling, res.Migrated)
		}
		if parser.Parse("x.bot", string(res.Migrated)).File.Tools[0].Command != "a\nb" {
			t.Fatalf("%q: the strict value changed", spelling)
		}
	}
}

// A comments-only file, with or without a final newline, takes the header
// on a line of its own and reads as profile 2; an empty file becomes the
// header alone.
func TestMigrationInsertsTheHeaderAtTheEndOfACommentsOnlyFile(t *testing.T) {
	for src, want := range map[string]string{
		"## note":   "## note\ndsl: 2\n",
		"## note\n": "## note\ndsl: 2\n",
		"":          "dsl: 2\n",
	} {
		res := mustMigrate(t, src)
		if string(res.Migrated) != want {
			t.Fatalf("%q migrated to %q, want %q", src, res.Migrated, want)
		}
	}
}

// An overlap between planned edits is reported, never sliced or paniced.
func TestOverlappingEditsAreReported(t *testing.T) {
	_, err := applyEdits("0123456789", []edit{{0, 5, "a"}, {3, 7, "b"}})
	if err == nil || !strings.Contains(err.Error(), "overlapping edits") {
		t.Fatalf("err = %v", err)
	}
	out, err := applyEdits("0123456789", []edit{{0, 5, "a"}, {5, 7, "b"}})
	if err != nil || out != "ab789" {
		t.Fatalf("adjacent edits: %q %v", out, err)
	}
}

// The `project_root:` refusal names the property's own line.
func TestProjectRootRefusalNamesThePropertyLine(t *testing.T) {
	src := "agent a:\n  description: \"x\"\n  memory:\n    enabled: true\n    project_root: true\n"
	_, err := Bytes("x.bot", []byte(src), Options{})
	if err == nil || !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "x.bot:5") {
		t.Fatalf("err = %v", err)
	}
}
