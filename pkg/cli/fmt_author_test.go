package cli

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// authorHelloDoc is the smallest author document that describes a program.
const authorHelloDoc = "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// `fmt --to bot x.bot.yaml` writes the .bot the document stands for, beside
// it, proven the same program; a second run is a no-op; a .bot already there
// and different is refused without --force and left as it is, --check says
// it would change, --force overwrites it.
func TestFmtToBotWritesTheBotADocumentStandsFor(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "d/hello.bot.yaml", authorHelloDoc)
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if err != nil {
		t.Fatalf("--to bot: %v", err)
	}
	bot := "d/hello.bot"
	got, err := os.ReadFile(bot)
	if err != nil {
		t.Fatalf("the .bot was not written: %v", err)
	}
	pr := parser.Parse(bot, string(got))
	if diagnosticErrors(pr.Diagnostics) != "" || unparse.Unparse(pr.File) != string(got) {
		t.Fatalf("the .bot written is not canonical .bot text:\n%s", got)
	}
	if want := unparse.Unparse(author.Parse(doc, []byte(authorHelloDoc)).File); string(got) != want {
		t.Fatalf("the .bot written is not the document's program:\n%s\nwant\n%s", got, want)
	}
	if len(res.Files) != 1 || res.Files[0].Path != bot || res.Files[0].From != doc || !res.Files[0].Written {
		t.Fatalf("report: %+v", res.Files)
	}

	res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if err != nil || len(res.Files) != 1 || res.Files[0].Changed {
		t.Fatalf("a second conversion is not a no-op: %v %+v", err, res.Files)
	}

	edited := "## edited by hand\n" + string(got)
	if err := os.WriteFile(bot, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "--force") {
		t.Fatalf("a different .bot beside the document was not refused: %v %v", err, res.Refused)
	}
	if now, _ := os.ReadFile(bot); string(now) != edited {
		t.Fatalf("the refused destination was rewritten:\n%s", now)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Check: true, Printer: jp}); !errors.Is(err, ErrFmtWouldChange) {
		t.Fatalf("--check on a differing destination: %v, want ErrFmtWouldChange", err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Force: true, Printer: jp}); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if now, _ := os.ReadFile(bot); string(now) != string(got) {
		t.Fatalf("--force did not write the document's .bot:\n%s", now)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Check: true, Printer: jp}); err != nil {
		t.Fatalf("--check on the .bot the document writes: %v", err)
	}
}

// A document whose program has no written .bot form (a profile-1 value with
// no v1 form under a catalog long enough to put the directive out of the
// lexer's window) is refused by --to bot, nothing written: the proof is the
// one validate reports as E054.
func TestFmtToBotRefusesADocumentWithNoWrittenForm(t *testing.T) {
	inTempWorkspace(t)
	desc := strings.Repeat("    a line of the description\n", 40)
	doc := writeBot(t, "e/long.bot.yaml", "dsl: 1\ncatalog:\n  name: probe\n  description: |\n"+desc+
		"nodes:\n  - tool: t\n    command: \"printf `x` \\\"y\\\"\"\n    output: out\nschemas:\n  out:\n    ok: bool\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n")
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "no written .bot form") {
		t.Fatalf("a document with no written form was not refused: %v %v", err, res.Refused)
	}
	if _, err := os.Stat("e/long.bot"); err == nil {
		t.Fatal("a .bot was written for a document that has no written form")
	}
}

// `fmt --to yaml x.bot` writes the author document of a .bot beside it — the
// same program — and says what the document does not carry: the frontmatter
// keys beyond `catalog:`'s four and the ordinary comments. A .bot that does
// not parse is refused, even with --force.
func TestFmtToYamlWritesTheDocumentOfABot(t *testing.T) {
	inTempWorkspace(t)
	botText := "## ---\n## name: probe\n## owner: jo\n## ---\n\nprompt ask:\n  Say hello.\n\n## the agent\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n"
	bot := writeBot(t, "y/hello.bot", botText)
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp})
	if err != nil {
		t.Fatalf("--to yaml: %v", err)
	}
	out, err := os.ReadFile("y/hello.bot.yaml")
	if err != nil {
		t.Fatalf("the document was not written: %v", err)
	}
	back := author.Parse("y/hello.bot.yaml", out)
	if back.HasErrors() {
		t.Fatalf("the document written does not read: %v\n%s", back.Diagnostics, out)
	}
	if why := ir.SameProgram(ir.Compile(back.File), ir.Compile(parser.Parse(bot, botText).File)); why != "" {
		t.Fatalf("the document written is not the .bot's program (%s):\n%s", why, out)
	}
	if !strings.Contains(string(out), "catalog:") || !strings.Contains(string(out), "name: probe") {
		t.Fatalf("the document carries no catalog:\n%s", out)
	}
	joined := strings.Join(res.Notices, "\n")
	if !strings.Contains(joined, "1 key(s) of the frontmatter are not carried") || !strings.Contains(joined, "owner") {
		t.Fatalf("the frontmatter key the document does not carry was not said: %q", res.Notices)
	}
	if !strings.Contains(joined, "1 comment line(s) are not represented") {
		t.Fatalf("the comment the document does not represent was not said: %q", res.Notices)
	}

	broken := writeBot(t, "y/broken.bot", "agent :\n  model\n")
	res, err = RunFmt(FmtOptions{Paths: []string{broken}, To: "yaml", Force: true, Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "does not parse") {
		t.Fatalf("a .bot that does not parse was converted: %v %v", err, res.Refused)
	}
	if _, err := os.Stat("y/broken.bot.yaml"); err == nil {
		t.Fatal("a document was written from a text the parser recovered on")
	}
}

// In a walk, fmt gives an author document its canonical YAML form as it
// gives a .bot its canonical text — proven the same program, idempotent,
// said by --check — and the .bot beside it is formatted all the same.
func TestFmtCanonicalisesADocumentInAWalk(t *testing.T) {
	inTempWorkspace(t)
	loose := "dsl: 2\nprompts: {ask: Say hello.}\nnodes:\n- agent: hello\n  model: m\n  system: ask\nworkflow: {name: hello, entry: hello, edges: [hello -> done]}\n"
	doc := writeBot(t, "w/hello.bot.yaml", loose)
	bot := writeBot(t, "w/other.bot", looseBot)
	jp, _ := jsonPrinter()
	if _, err := RunFmt(FmtOptions{Paths: []string{"w"}, Check: true, Printer: jp}); !errors.Is(err, ErrFmtWouldChange) {
		t.Fatalf("--check on a loose document: %v, want ErrFmtWouldChange", err)
	}
	res, err := RunFmt(FmtOptions{Paths: []string{"w"}, Printer: jp})
	if err != nil {
		t.Fatalf("fmt: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("both files were not formatted: %+v", res.Files)
	}
	got, _ := os.ReadFile(doc)
	want, err := author.Write(author.Parse(doc, []byte(loose)).File)
	if err != nil || string(got) != string(want) {
		t.Fatalf("the document is not in its canonical form (%v):\n%s\nwant\n%s", err, got, want)
	}
	if unparse.Unparse(author.Parse(doc, got).File) != unparse.Unparse(author.Parse(doc, []byte(loose)).File) {
		t.Fatal("the canonical document is another program")
	}
	if b, _ := os.ReadFile(bot); string(b) == looseBot {
		t.Fatal("the .bot beside the document was not formatted")
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{"w"}, Check: true, Printer: jp}); err != nil {
		t.Fatalf("--check after fmt: %v", err)
	}
}

// A document that carries YAML comments is refused in a walk and left as it
// is — the writer keeps none — and named among the refusals; --to bot still
// reads it, since the .bot it writes carries the program, not the document.
func TestFmtRefusesToRewriteACommentedDocument(t *testing.T) {
	inTempWorkspace(t)
	commented := "# keep me\n" + strings.Replace(authorHelloDoc, "  ask: Say hello.\n", "  ask: Say hello. # and me\n", 1)
	doc := writeBot(t, "k/hello.bot.yaml", commented)
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{"k"}, Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "2 YAML comment line(s)") {
		t.Fatalf("a commented document was not refused by name: %v %v", err, res.Refused)
	}
	if len(res.RefusedPaths) != 1 {
		t.Fatalf("the refusal is not listed for a baseline: %v", res.RefusedPaths)
	}
	if got, _ := os.ReadFile(doc); string(got) != commented {
		t.Fatalf("the commented document was rewritten:\n%s", got)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp}); err != nil {
		t.Fatalf("--to bot on a commented document: %v", err)
	}
	if _, err := os.Stat("k/hello.bot"); err != nil {
		t.Fatalf("the .bot of a commented document was not written: %v", err)
	}
}

// --to takes named files of the kind the direction converts, and no baseline.
func TestFmtToNeedsNamedFilesOfTheRightKind(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "n/hello.bot.yaml", authorHelloDoc)
	bot := writeBot(t, "n/other.bot", looseBot)
	jp, _ := jsonPrinter()
	if _, err := RunFmt(FmtOptions{Paths: []string{"n"}, To: "bot", Printer: jp}); !errors.Is(err, ErrFmtToNeedsFiles) {
		t.Fatalf("--to on a directory: %v", err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "bot", Printer: jp}); !errors.Is(err, ErrFmtToKind) {
		t.Fatalf("--to bot on a .bot: %v", err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "yaml", Printer: jp}); !errors.Is(err, ErrFmtToKind) {
		t.Fatalf("--to yaml on a document: %v", err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Baseline: "x", Printer: jp}); !errors.Is(err, ErrFmtToBaseline) {
		t.Fatalf("--to with --baseline: %v", err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "json", Printer: jp}); err == nil || !strings.Contains(err.Error(), "bot or yaml") {
		t.Fatalf("--to json: %v", err)
	}
}

// fix takes no document: named one, it says which command writes the .bot
// it works on.
func TestFixTakesNoDocument(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "f/hello.bot.yaml", authorHelloDoc)
	jp, _ := jsonPrinter()
	_, err := RunFix(FixOptions{Paths: []string{doc}, Printer: jp})
	if err == nil || !strings.Contains(err.Error(), "author document") || !strings.Contains(err.Error(), "fmt --to bot") {
		t.Fatalf("fix on a document: %v", err)
	}
}
