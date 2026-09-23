package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// The syntax floor (C252) is read off the document — its profile, imports
// and contract — never off a main.bot on disk: a profile-2 document in a
// bundle without an engine floor draws C252 as its .bot would, and a
// profile-1 document beside a stale profile-2 main.bot draws none.
func TestValidateReadsTheSyntaxFloorOffTheDocument(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "manifest.yaml", "schema_version: 1\nname: probe\n")
	doc := writeFixture(t, dir, "main.bot.yaml", authorHello)
	res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if !hasDiagCode(res, "C252") {
		t.Fatalf("a profile-2 document in a bundle without an engine floor drew no C252 — the floor was read off the disk, where no main.bot is:\n%s", out)
	}

	writeFixture(t, dir, "main.bot", "dsl: 2\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n")
	doc = writeFixture(t, dir, "main.bot.yaml", strings.Replace(authorHello, "dsl: 2", "dsl: 1", 1))
	res, out, _ = validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if hasDiagCode(res, "C252") {
		t.Fatalf("C252 on a profile-1 document: the floor was read off the stale profile-2 main.bot beside it:\n%s", out)
	}
}

// A document the converter reads may describe a program that has no written
// .bot form: a profile-1 document whose value has no v1 form (a backtick and
// a quote together) makes the writer go strict, and a catalog long enough
// puts the directive out of the lexer's window. validate says so as an error
// (E054), the cause named — an OK followed by a refusal to write the .bot
// would be a lie — and the same document with a short catalog is OK.
func TestValidateRefusesADocumentWithNoWrittenBotForm(t *testing.T) {
	document := func(lines int) string {
		desc := strings.Repeat("    a line of the description\n", lines)
		return "dsl: 1\ncatalog:\n  name: probe\n  description: |\n" + desc +
			"nodes:\n  - tool: t\n    command: \"printf `x` \\\"y\\\"\"\n    output: out\nschemas:\n  out:\n    ok: bool\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n"
	}
	dir := t.TempDir()
	long := writeFixture(t, dir, "long.bot.yaml", document(40))
	res, out, err := validateDocumentJSON(t, long, cli.ValidateOptions{})
	if err == nil || res.Valid {
		t.Fatalf("a document whose program has no written .bot form validated OK:\n%s", out)
	}
	found := false
	for _, d := range res.Diagnostics {
		if d.Code != "E054" {
			continue
		}
		found = true
		if d.Severity != "error" || d.File != long || !strings.Contains(d.Message, "directive") || d.Hint == "" {
			t.Fatalf("E054 without its severity, file, cause or hint: %+v", d)
		}
	}
	if !found {
		t.Fatalf("no E054 among the findings:\n%s", out)
	}
	short := writeFixture(t, dir, "short.bot.yaml", document(1))
	res, out, err = validateDocumentJSON(t, short, cli.ValidateOptions{})
	if err != nil || !res.Valid {
		t.Fatalf("the same document with a short catalog is refused: %v\n%s", err, out)
	}
}

// The document's catalog is the bundle's frontmatter whatever else the
// document says: an unrelated error does not make the frontmatter checks
// disappear — the author would fix the error and meet a warning that was
// there all along.
func TestValidateReadsADocumentsCatalogWhateverItsErrors(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "manifest.yaml", "schema_version: 1\nname: probe\ncapabilities: [network]\n")
	doc := writeFixture(t, dir, "main.bot.yaml", "dsl: 2\ncatalog:\n  name: probe\n  capabilities: [shell]\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\n    max_turns: 3\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n")
	res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if !hasDiagCode(res, "E012") {
		t.Fatalf("the unknown property drew no E012:\n%s", out)
	}
	if !hasDiagCode(res, "C221") {
		t.Fatalf("the frontmatter check vanished with an unrelated error (no C221):\n%s", out)
	}
}

// A document under lib/ holds no workflow: the remedy names the .bot it
// stands for, which an import can name — the parser refuses an import of a
// document (E045), so a hint naming the document would prescribe a text the
// tool rejects.
func TestValidateNamesTheBotAFragmentDocumentStandsFor(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := writeFixture(t, filepath.Join(dir, "lib"), "y.bot.yaml", "dsl: 2\nprompts:\n  p: hi\n")
	res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if err == nil || res.Valid {
		t.Fatalf("a document without a workflow validated OK:\n%s", out)
	}
	hinted := false
	for _, d := range res.Diagnostics {
		if d.Hint == "" {
			continue
		}
		hinted = true
		if strings.Contains(d.Hint, ".bot.yaml") || !strings.Contains(d.Hint, "import \"lib/y.bot\"") {
			t.Fatalf("the remedy names the document, which an import cannot: %q", d.Hint)
		}
	}
	if !hinted {
		t.Fatalf("no remedy given:\n%s", out)
	}
}

// A directory whose only source is a document is a bundle for no launch:
// `validate <dir>` refuses it as the launchers do, and names the document
// beside the missing main so the author validates it by its own name.
func TestValidateOfADirectoryNamesTheDocumentBesideTheMissingMain(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "manifest.yaml", "schema_version: 1\nname: probe\n")
	writeFixture(t, dir, "main.bot.yaml", authorHello)
	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunValidate(dir, p)
	if err == nil {
		t.Fatalf("a directory whose only source is a document opened as a bundle:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), "main.bot.yaml") || !strings.Contains(err.Error(), "no main.bot") {
		t.Fatalf("the refusal names neither the missing main nor the document beside it: %v", err)
	}
}

// A manifest-looking file beside a document that is not an iterion manifest
// is said (C223), as it is beside a main.bot: the check looks beside the .bot
// the document stands for.
func TestValidateWarnsOfAForeignManifestBesideADocument(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "manifest.yaml", "name: probe\nfoo: [bar\n")
	doc := writeFixture(t, dir, "main.bot.yaml", authorHello)
	res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if err != nil || !res.Valid {
		t.Fatalf("a foreign manifest beside the document refused it: %v\n%s", err, out)
	}
	if !hasDiagCode(res, "C223") {
		t.Fatalf("the foreign manifest beside the document was not said (no C223):\n%s", out)
	}
}
