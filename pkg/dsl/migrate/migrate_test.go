package migrate

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func mustMigrate(t *testing.T, src string) *Result {
	t.Helper()
	res, err := Bytes("x.bot", []byte(src), Options{})
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	return res
}

// The header goes in after the head comments, a literal holding a
// backslash is re-spelled from its profile-1 value — `\"` included, which
// profile 1 kept as two characters — and every other byte stays: the
// comments inside blocks, the blank lines, the order.
func TestMigrationTouchesOnlyWhatChangesMeaning(t *testing.T) {
	src := "## ---\n## name: probe\n## ---\n## a head comment\n\ntool t:\n  ## a comment inside the block\n  command: \"printf 'a\\nb' \\\"q\\\"\"\n  description: `raw \\n stays`\n\n\nworkflow w:\n  entry: t\n  t -> done\n"
	res := mustMigrate(t, src)
	want := "## ---\n## name: probe\n## ---\n## a head comment\n\ndsl: 2\n\ntool t:\n  ## a comment inside the block\n  command: \"printf 'a\\\\nb' \\\\\\\"q\\\\\\\"\"\n  description: `raw \\n stays`\n\n\nworkflow w:\n  entry: t\n  t -> done\n"
	if string(res.Migrated) != want {
		t.Fatalf("migrated:\n%s\nwant:\n%s", res.Migrated, want)
	}
	if !res.Changed || len(res.Changes) != 2 || res.Changes[0].Kind != "header" || res.Changes[1].Kind != "literal" || res.Changes[1].Line != 8 {
		t.Fatalf("changes: %+v", res.Changes)
	}
	// The value is the same in both readings.
	before := parser.Parse("x.bot", src).File.Tools[0].Command
	after := parser.Parse("x.bot", string(res.Migrated)).File.Tools[0].Command
	if before != after || before != `printf 'a\nb' \"q\"` {
		t.Fatalf("command before %q, after %q", before, after)
	}
}

// The directive comes out (profile 2 refuses it), an explicit `dsl: 1` is
// replaced, a file already at profile 2 is left alone, and migrating twice
// is migrating once.
func TestMigrationHandlesTheHeaderAndTheDirective(t *testing.T) {
	strict := "## strict-escape: on\n## note\ntool t:\n  command: \"a\\nb\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	res := mustMigrate(t, strict)
	if strings.Contains(string(res.Migrated), "strict-escape") || !strings.HasPrefix(string(res.Migrated), "## note\ndsl: 2\n\ntool t:") {
		t.Fatalf("directive not replaced by the header:\n%s", res.Migrated)
	}
	// The strict literal already read `\n` as a newline: its spelling is unchanged.
	if strings.Contains(string(res.Migrated), `\\n`) {
		t.Fatalf("a strict literal was re-spelled:\n%s", res.Migrated)
	}
	if parser.Parse("x.bot", string(res.Migrated)).File.Tools[0].Command != "a\nb" {
		t.Fatalf("value changed")
	}

	explicit := "dsl: 1\ntool t:\n  command: \"x\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	res = mustMigrate(t, explicit)
	if !strings.HasPrefix(string(res.Migrated), "dsl: 2\ntool t:") {
		t.Fatalf("explicit dsl: 1 not replaced:\n%s", res.Migrated)
	}

	again, err := Bytes("x.bot", res.Migrated, Options{})
	if err != nil || again.Changed || len(again.Changes) != 0 || !bytes.Equal(again.Migrated, res.Migrated) {
		t.Fatalf("not idempotent: %v %+v", err, again)
	}
}

// The original bytes are what is rewritten: a BOM and CRLF line endings
// stay, and the header takes the file's own line ending. The frontmatter
// is read on those bytes — a BOM makes it invisible today — so migrating
// does not make a catalogue identity appear.
func TestMigrationPreservesTheOriginalBytes(t *testing.T) {
	src := "\ufeff## ---\r\n## name: probe\r\n## triggers: [x]\r\n## ---\r\ntool t:\r\n  command: \"a\\tb\"\r\n\r\nworkflow w:\r\n  entry: t\r\n  t -> done\r\n"
	res := mustMigrate(t, src)
	out := string(res.Migrated)
	if !strings.HasPrefix(out, "\ufeff## ---\r\n") || strings.Contains(out, "\n\n") || !strings.Contains(out, "## ---\r\ndsl: 2\r\n\r\ntool t:\r\n") {
		t.Fatalf("bytes not preserved:\n%q", out)
	}
	if !strings.Contains(out, `command: "a\\tb"`) {
		t.Fatalf("literal not re-spelled:\n%q", out)
	}
	if bundle.ParseFrontmatter([]byte(src)) != nil || bundle.ParseFrontmatter(res.Migrated) != nil {
		t.Fatalf("a frontmatter behind a BOM became visible")
	}
	// Without the BOM the identity is visible on both sides, and equal.
	plain := strings.TrimPrefix(src, "\ufeff")
	res = mustMigrate(t, plain)
	fb, fa := bundle.ParseFrontmatter([]byte(plain)), bundle.ParseFrontmatter(res.Migrated)
	if fb == nil || fa == nil || fb.Name != fa.Name || len(fa.Triggers) != 1 {
		t.Fatalf("frontmatter: %+v / %+v", fb, fa)
	}
}

// A named prompt with a blank line changes rendering under profile 2: the
// text is the same, the paragraph reaches the model. Reported by name, and
// refused only on request.
func TestMigrationReportsThePromptsWhoseRenderingChanges(t *testing.T) {
	src := "prompt p:\n  First.\n\n  Second.\n\nprompt q:\n  Single.\n\nagent a:\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"
	res := mustMigrate(t, src)
	if len(res.Prompts) != 1 || res.Prompts[0].Name != "p" || res.Prompts[0].BlankLines != 1 || res.Prompts[0].Line != 1 {
		t.Fatalf("prompts: %+v", res.Prompts)
	}
	if _, err := Bytes("x.bot", []byte(src), Options{StrictPrompts: true}); err == nil || !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "p (line 1, 1 blank lines)") {
		t.Fatalf("strict: %v", err)
	}
}

// What has no profile-2 form is refused with the remedy, never guessed:
// `project_root:`, and a file that does not parse to begin with.
func TestMigrationRefusesWhatItCannotRewrite(t *testing.T) {
	src := "agent a:\n  memory:\n    enabled: true\n    project_root: true\n\nworkflow w:\n  entry: a\n  a -> done\n"
	_, err := Bytes("x.bot", []byte(src), Options{})
	if err == nil || !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "x.bot:2") || !strings.Contains(err.Error(), "keep the file in profile 1") {
		t.Fatalf("project_root: %v", err)
	}
	_, err = Bytes("x.bot", []byte("agent a:\n  bogus: 1\n"), Options{})
	if err == nil || !strings.Contains(err.Error(), "does not parse in its own profile") {
		t.Fatalf("broken file: %v", err)
	}
	_, err = Bytes("x.bot", []byte("agent a:\n  description: \"x\"\n"), Options{To: 3})
	if err == nil || !strings.Contains(err.Error(), "profile 2 only") {
		t.Fatalf("profile 3: %v", err)
	}
}

// The catalogue-identity oracle sees a frontmatter that changed, and
// neither one behind a BOM (invisible on both sides) nor two equal ones.
func TestCatalogueIdentityOracleSeesAChange(t *testing.T) {
	a := []byte("## ---\n## name: one\n## ---\nagent a:\n  description: \"x\"\n")
	b := []byte("## ---\n## name: two\n## ---\nagent a:\n  description: \"x\"\n")
	if sameCatalogueIdentity(a, b) {
		t.Fatalf("two frontmatter names read as the same identity")
	}
	if !sameCatalogueIdentity(a, a) || !sameCatalogueIdentity([]byte("\ufeff"+string(a)), []byte("\ufeff"+string(b))) {
		t.Fatalf("equal, or both invisible, identities read as different")
	}
}

// The oracle is not vacuous: a fragment with no workflow is compared as a
// document, and a rewrite that changed a value would be caught.
func TestTheOracleComparesDocumentsNotOnlyPrograms(t *testing.T) {
	fragment := "tool t:\n  command: \"a\\nb\"\n"
	res := mustMigrate(t, fragment)
	if !strings.Contains(string(res.Migrated), `"a\\nb"`) {
		t.Fatalf("fragment:\n%s", res.Migrated)
	}
	if why := sameDocument(parser.Parse("a", "tool t:\n  command: \"x\"\n").File, parser.Parse("b", "tool t:\n  command: \"y\"\n").File); why == "" {
		t.Fatalf("two different documents read as the same")
	}
}
