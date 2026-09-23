package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
)

// authorHello is the smallest author document that describes a program.
const authorHello = "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

func validateDocumentJSON(t *testing.T, path string, opts cli.ValidateOptions) (cli.ValidateResult, string, error) {
	t.Helper()
	p, buf := newTestPrinter(cli.OutputJSON)
	err := cli.RunValidateWith(path, p, opts)
	var res cli.ValidateResult
	if jerr := json.Unmarshal(buf.Bytes(), &res); jerr != nil {
		t.Fatalf("validate printed no result JSON (%v): err=%v\n%s", jerr, err, buf.String())
	}
	return res, buf.String(), err
}

func hasDiagCode(res cli.ValidateResult, code string) bool {
	for _, d := range res.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}

// An author document is validated as the program it describes: OK, named as
// an author source with the .bot it stands for — which validate never writes.
func TestValidateReadsAnAuthorDocumentAsItsBot(t *testing.T) {
	dir := t.TempDir()
	doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
	res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if err != nil || !res.Valid {
		t.Fatalf("validate refused a valid document: %v\n%s", err, out)
	}
	if res.SourceKind != "author" || res.BotPath != filepath.Join(dir, "hello.bot") {
		t.Fatalf("source_kind %q bot_path %q, want author / %s", res.SourceKind, res.BotPath, filepath.Join(dir, "hello.bot"))
	}
	if res.File != doc || res.WorkflowName != "hello" || res.NodeCount == 0 || res.EdgeCount != 1 {
		t.Fatalf("file %q workflow %q nodes %d edges %d, want the document, hello, the program's nodes and its one edge", res.File, res.WorkflowName, res.NodeCount, res.EdgeCount)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.bot")); err == nil {
		t.Fatal("validate wrote the .bot the document stands for")
	}
}

// A finding of the compiled program is positioned on the document — the YAML
// line the author wrote — under the document's own path, not the .bot's.
func TestValidatePositionsADocumentsFindingsOnTheDocument(t *testing.T) {
	dir := t.TempDir()
	bad := strings.Replace(authorHello, "entry: hello", "entry: nope", 1)
	doc := writeFixture(t, dir, "hello.bot.yaml", bad)
	res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if err == nil || res.Valid {
		t.Fatalf("an entry that names no node validated OK:\n%s", out)
	}
	// The compiler attributes what it can: the node the bad entry leaves
	// unreachable sits at its declaration on the document (C016); the entry
	// that names no node (C008) carries no position on a .bot either.
	// Whatever is positioned is positioned on the document — the YAML line
	// the author wrote — never on the .bot it stands for.
	nodeLine := strings.Count(bad[:strings.Index(bad, "- agent: hello")], "\n") + 1
	positioned := 0
	for _, d := range res.Diagnostics {
		if d.Severity != "error" || d.Line == 0 {
			continue
		}
		positioned++
		if d.File != doc {
			t.Fatalf("a finding under %q, want the document %q: %+v", d.File, doc, d)
		}
		if d.Code == "C016" && d.Line != nodeLine {
			t.Fatalf("the unreachable node is reported at line %d, want the document's line %d (`- agent: hello`): %+v\n%s", d.Line, nodeLine, d, out)
		}
	}
	if positioned == 0 {
		t.Fatalf("no error positioned on the document:\n%s", out)
	}
}

// A profile-1 document whose catalog is long and whose value needs a strict
// escape has a written .bot form all the same — the writer spells the value
// as a raw string, so no directive has to sit in the lexer's window — and
// validate says OK for the long catalog as for the short.
func TestValidateAcceptsALongCatalogBesideAStrictValue(t *testing.T) {
	document := func(lines int) string {
		desc := strings.Repeat("    a line of the description\n", lines)
		return "dsl: 1\ncatalog:\n  name: probe\n  description: |\n" + desc +
			"nodes:\n  - tool: t\n    command: \"printf a\\nb\"\n    output: out\nschemas:\n  out:\n    ok: bool\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n"
	}
	dir := t.TempDir()
	for name, lines := range map[string]int{"short.bot.yaml": 1, "long.bot.yaml": 40} {
		doc := writeFixture(t, dir, name, document(lines))
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("%s: refused: %v\n%s", name, err, out)
		}
		if hasDiagCode(res, "E054") {
			t.Fatalf("%s: E054 on a document whose program the writer spells without a directive:\n%s", name, out)
		}
	}
}

// A document that stands for a bundle's main is validated in that bundle —
// its prompts/*.md in scope, its manifest read — whether or not the main.bot
// is written.
func TestValidateReadsAnAuthorDocumentInItsBundle(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "manifest.yaml", "schema_version: 1\nname: probe\n")
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "prompts"), "ask.md", "Say hello.\n")
	doc := writeFixture(t, dir, "main.bot.yaml", "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n")
	res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if err != nil || !res.Valid {
		t.Fatalf("the bundle's prompt was not in scope for the document: %v\n%s", err, out)
	}
	if res.BundleName != "probe" {
		t.Fatalf("bundle_name %q, want probe (the manifest beside the document was not read)", res.BundleName)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.bot")); err == nil {
		t.Fatal("validate wrote main.bot")
	}
}

// `iterion validate x.bot.yaml` run from the document's own directory: an
// include in a prompt resolves beside the document — validate hands the
// converter the path in full.
func TestValidateResolvesAnIncludeBesideARelativeDocumentPath(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "rules.md", "RULES")
	writeFixture(t, dir, "x.bot.yaml", "dsl: 2\nprompts:\n  p: |\n    {{include \"rules.md\"}}\nnodes:\n  - agent: a\n    model: m\n    system: p\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n")
	t.Chdir(dir)
	res, out, err := validateDocumentJSON(t, "x.bot.yaml", cli.ValidateOptions{})
	if strings.Contains(out, "C055") {
		t.Fatalf("the include beside a relative document path was refused:\n%s", out)
	}
	if err != nil || !res.Valid {
		t.Fatalf("validate refused a document whose include sits beside it: %v\n%s", err, out)
	}
}

// --exec runs the dry run on the program a document describes, as on a .bot.
func TestValidateDryRunsAnAuthorDocument(t *testing.T) {
	dir := t.TempDir()
	doc := writeFixture(t, dir, "t.bot.yaml", "dsl: 2\nnodes:\n  - tool: t\n    command: \"true\"\n    output: out\nschemas:\n  out:\n    ok: bool\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n")
	res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{Exec: true})
	if err != nil || !res.Valid {
		t.Fatalf("validate --exec refused the document: %v\n%s", err, out)
	}
	if res.Exec == nil {
		t.Fatalf("no dry-run report for the document:\n%s", out)
	}
}

// A finding on a document carries no mechanical .bot edit: the edits `fix`
// plans (C137, the quotes around a reference) are positions in .bot text,
// and a document's text is YAML — a folded scalar in a prompt can spell a
// `tool t:` block the planner would find, and edit the prompt's text.
func TestValidateCarriesNoBotEditOnADocument(t *testing.T) {
	dir := t.TempDir()
	doc := writeFixture(t, dir, "q.bot.yaml", "dsl: 2\nvars:\n  x: {type: string, default: v}\nprompts:\n  p: >\n    tool t:\n      command: \"printf '{{vars.x}}'\"\nnodes:\n  - tool: t\n    command: \"printf '{{vars.x}}'\"\n    output: out\nschemas:\n  out:\n    ok: bool\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n")
	res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if !hasDiagCode(res, "C137") {
		t.Fatalf("the quoted reference drew no C137:\n%s", out)
	}
	for _, d := range res.Diagnostics {
		if d.Edit != nil {
			t.Fatalf("a .bot edit rides a document's finding: %+v\n%s", d, out)
		}
	}
}

// A document's `catalog:` is the frontmatter of the .bot it stands for: the
// bundle's lint reads it there — off the written .bot, not off the YAML —
// and a capabilities list that differs from the manifest's is said (C221).
func TestValidateReadsADocumentsCatalogAsItsBundlesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "manifest.yaml", "schema_version: 1\nname: probe\ncapabilities: [network]\n")
	doc := writeFixture(t, dir, "main.bot.yaml", "dsl: 2\ncatalog:\n  name: probe\n  capabilities: [shell]\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n")
	res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
	if !hasDiagCode(res, "C221") {
		t.Fatalf("the document's catalog was not read as the bundle's frontmatter (no C221):\n%s", out)
	}
}
