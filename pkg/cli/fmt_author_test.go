package cli

import (
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// authorHelloDoc is the smallest author document that describes a program.
const authorHelloDoc = "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// looseHelloDoc describes the same program in a form the writer does not
// produce: flow mappings, a sequence at the margin.
const looseHelloDoc = "dsl: 2\nprompts: {ask: Say hello.}\nnodes:\n- agent: hello\n  model: m\n  system: ask\nworkflow: {name: hello, entry: hello, edges: [hello -> done]}\n"

// settledDoc is a document whose prompt body the .bot reads otherwise than
// it was written: the lexer settles the leading and trailing blank lines
// (E053).
const settledDoc = "dsl: 2\nprompts:\n  spaced: |\n\n\n      Indented and surrounded by blank lines.\n      Second line.\n\n\nnodes:\n  - agent: a\n    model: m\n    system: spaced\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n"

// `fmt --to bot x.bot.yaml` writes the .bot the document stands for, beside
// it, proven the same program; --check says so first, writing nothing; a
// second run is a no-op; a .bot already there and different is refused
// without --force and left as it is — under --check as well, which reports
// what the write would do — and --force overwrites it, "would write" under
// --check.
func TestFmtToBotWritesTheBotADocumentStandsFor(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "d/hello.bot.yaml", authorHelloDoc)
	bot := "d/hello.bot"
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Check: true, Printer: jp})
	if !errors.Is(err, ErrFmtWouldChange) || len(res.Files) != 1 || !res.Files[0].Changed || res.Files[0].Written {
		t.Fatalf("--check with no .bot beside the document: %v %+v, want ErrFmtWouldChange and a file to write", err, res.Files)
	}
	if _, err := os.Stat(bot); err == nil {
		t.Fatal("--check wrote the .bot")
	}
	res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if err != nil {
		t.Fatalf("--to bot: %v", err)
	}
	got, err := os.ReadFile(bot)
	if err != nil {
		t.Fatalf("the .bot was not written: %v", err)
	}
	pr := parsertest.Parse(bot, string(got))
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
	res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Check: true, Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "--force") {
		t.Fatalf("--check on a differing destination without --force: %v %v, want the refusal the write gives", err, res.Refused)
	}
	res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Check: true, Force: true, Printer: jp})
	if !errors.Is(err, ErrFmtWouldChange) || len(res.Files) != 1 || !res.Files[0].Changed || res.Files[0].Written {
		t.Fatalf("--check --force on a differing destination: %v %+v, want ErrFmtWouldChange", err, res.Files)
	}
	if now, _ := os.ReadFile(bot); string(now) != edited {
		t.Fatalf("--check rewrote the destination:\n%s", now)
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

// --force replaces its destination whole, and says what that file carried
// that the text replacing it does not: the .bot's comments and frontmatter
// lines when a document writes it, the document's YAML comments when a
// .bot does — named by count and the first, under --check as well; a
// destination with none gets no note.
func TestFmtForceSaysWhatTheFileItReplacesCarried(t *testing.T) {
	inTempWorkspace(t)
	jp, _ := jsonPrinter()
	bot := writeBot(t, "d/x.bot", "## owner: jo\n## tags: [x]\n\n# Why this bot exists.\ndsl: 2\n\nagent hello:\n  model: \"m\" # trailing\n  system: \"Say hello.\"\n\nworkflow hello:\n  entry: hello\n\n  hello -> done\n")
	if _, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("--to yaml: %v", err)
	}
	doc := "d/x.bot.yaml"
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(doc, []byte(strings.Replace(string(raw), "Say hello.", "Say hello twice.", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	said := func(res FmtResult, want string) bool {
		for _, n := range res.Notices {
			if strings.Contains(n, want) {
				return true
			}
		}
		return false
	}
	for _, check := range []bool{true, false} {
		res, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Force: true, Check: check, Printer: jp})
		if check && !errors.Is(err, ErrFmtWouldChange) || !check && err != nil {
			t.Fatalf("--to bot --force (check %v): %v", check, err)
		}
		if !said(res, "d/x.bot: --force replaces it whole — 4 comment line(s) it carries are not in what d/x.bot.yaml writes") || !said(res, "owner: jo") {
			t.Fatalf("--to bot --force (check %v) replaced a commented .bot without saying so: %q", check, res.Notices)
		}
	}

	// The other way: a document's YAML comment, replaced by what the .bot writes.
	now, _ := os.ReadFile(doc)
	if err := os.WriteFile(doc, append([]byte("# keep me\n"), now...), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Force: true, Printer: jp})
	if err != nil || !said(res, "d/x.bot.yaml: --force replaces it whole — 1 comment line(s) it carries are not in what d/x.bot writes (the first: # keep me)") {
		t.Fatalf("--to yaml --force replaced a commented document without saying so: %v %q", err, res.Notices)
	}

	// Nothing carried, nothing said: the .bot now holds no comment.
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Force: true, Printer: jp}); err != nil {
		t.Fatal(err)
	}
	now, _ = os.ReadFile(doc)
	if err := os.WriteFile(doc, []byte(strings.Replace(string(now), "Say hello twice.", "Say hello thrice.", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Force: true, Printer: jp})
	if err != nil || said(res, "--force replaces it whole") {
		t.Fatalf("a note for a .bot that carried no comment: %v %q", err, res.Notices)
	}

	// A frontmatter is compared by key: the writer spells it its own way, so
	// a value the document carries (the quoted description) is not lost, and
	// a key it does not carry (the tags) is named.
	fm := writeBot(t, "f/y.bot", "## ---\n## name: hello\n## description: \"Says hello\"\n## tags:\n##   - a\n##   - b\n## ---\n\n# kept nowhere\ndsl: 2\n\nagent hello:\n  model: \"m\"\n  system: \"Say hello.\"\n\nworkflow hello:\n  entry: hello\n\n  hello -> done\n")
	if _, err := RunFmt(FmtOptions{Paths: []string{fm}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("--to yaml of a .bot with a frontmatter: %v", err)
	}
	ydoc, _ := os.ReadFile("f/y.bot.yaml")
	if err := os.WriteFile("f/y.bot.yaml", []byte(strings.Replace(string(ydoc), "Say hello.", "Say hello twice.", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = RunFmt(FmtOptions{Paths: []string{"f/y.bot.yaml"}, To: "bot", Force: true, Printer: jp})
	if err != nil || !said(res, "f/y.bot: --force replaces it whole — 1 comment line(s) and the frontmatter key(s) tags it carries are not in what f/y.bot.yaml writes (the first comment: kept nowhere)") || said(res, "description") {
		t.Fatalf("--force over a .bot with a frontmatter names the wrong losses: %v %q", err, res.Notices)
	}

	// A document that is not one YAML document — not YAML, comments alone
	// (a draft commented out), a second document — is replaced uncounted, and
	// says so.
	for _, target := range []string{
		"# a comment\nnodes: [\"unclosed\n",
		"# dsl: 2\n# nodes:\n#   - agent: hello\n# a draft commented out\n",
		"dsl: 2\n---\n# the second document\nnodes: []\n",
		string([]byte{0xFF, 0xFE, 'd', 0, 's', 0, 'l', 0, ':', 0, ' ', 0, '2', 0, '\n', 0}), // UTF-16, which yaml.v3 also reads
	} {
		if err := os.WriteFile("f/y.bot.yaml", []byte(target), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err = RunFmt(FmtOptions{Paths: []string{fm}, To: "yaml", Force: true, Printer: jp})
		if err != nil || !said(res, "f/y.bot.yaml: --force replaces it whole — it does not read as one YAML document") {
			t.Fatalf("--force over %q said nothing of what it replaced: %v %q", target, err, res.Notices)
		}
	}

	// The block's own YAML comments are named — the writer keeps none — and a
	// key written twice is one key, as the catalogue reads it.
	fm2 := writeBot(t, "g/z.bot", "## ---\n## # owned by team-x: do not rename\n## name: old\n## name: hello # the public name\n## description: Says hello\n## ---\n\ndsl: 2\n\nagent hello:\n  model: \"m\"\n  system: \"Say hello.\"\n\nworkflow hello:\n  entry: hello\n\n  hello -> done\n")
	if _, err := RunFmt(FmtOptions{Paths: []string{fm2}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("--to yaml of a .bot whose frontmatter carries comments: %v", err)
	}
	zdoc, _ := os.ReadFile("g/z.bot.yaml")
	if err := os.WriteFile("g/z.bot.yaml", []byte(strings.Replace(string(zdoc), "Say hello.", "Say hello twice.", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = RunFmt(FmtOptions{Paths: []string{"g/z.bot.yaml"}, To: "bot", Force: true, Printer: jp})
	if err != nil || !said(res, "g/z.bot: --force replaces it whole — 2 comment line(s) it carries are not in what g/z.bot.yaml writes (the first comment: # owned by team-x: do not rename)") || said(res, "frontmatter key") {
		t.Fatalf("--force over a .bot whose frontmatter carries comments names the wrong losses: %v %q", err, res.Notices)
	}

	// A block that is not one document — a `...` ends the first — is compared
	// line by line: what follows the end marker is named, never dropped.
	fm3 := writeBot(t, "h/w.bot", "## ---\n## name: hello\n## ...\n## owner: team-x\n## ---\n\ndsl: 2\n\nagent hello:\n  model: \"m\"\n  system: \"Say hello.\"\n\nworkflow hello:\n  entry: hello\n\n  hello -> done\n")
	if _, err := RunFmt(FmtOptions{Paths: []string{fm3}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("--to yaml of a .bot whose frontmatter holds an end marker: %v", err)
	}
	wdoc, _ := os.ReadFile("h/w.bot.yaml")
	if err := os.WriteFile("h/w.bot.yaml", []byte(strings.Replace(string(wdoc), "Say hello.", "Say hello twice.", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = RunFmt(FmtOptions{Paths: []string{"h/w.bot.yaml"}, To: "bot", Force: true, Printer: jp})
	if err != nil || !said(res, "h/w.bot: --force replaces it whole") || !said(res, "comment line(s)") {
		t.Fatalf("--force over a .bot whose frontmatter holds an end marker said nothing of what follows it: %v %q", err, res.Notices)
	}

	// A .bot the parser only recovers on is replaced uncounted, and says so:
	// what it carries past the error cannot be listed.
	doc2 := writeBot(t, "k/v.bot.yaml", authorHelloDoc)
	writeBot(t, "k/v.bot", "# keep me\ndsl: 2\n\nagent hello:\n  model: \"m\n# after the error\n")
	res, err = RunFmt(FmtOptions{Paths: []string{doc2}, To: "bot", Force: true, Printer: jp})
	if err != nil || !said(res, "k/v.bot: --force replaces it whole — it does not parse as a .bot") {
		t.Fatalf("--force over a .bot that does not parse said nothing of what it replaced: %v %q", err, res.Notices)
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

// A document the .bot reads otherwise than it was written — a prompt body
// the lexer settles (E053) — is refused by fmt and left as it is: the
// writer would put the reading in the author's place, silently. --to bot
// still writes the .bot, which carries the reading, and says so once, under
// the name the command was given.
func TestFmtRefusesToSettleADocumentsPromptBody(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "s/settled.bot.yaml", settledDoc)
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc}, Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "E053") || !strings.Contains(res.Refused[0], "otherwise") {
		t.Fatalf("a document the .bot reads otherwise than written was not refused by name: %v %v", err, res.Refused)
	}
	if got, _ := os.ReadFile(doc); string(got) != settledDoc {
		t.Fatalf("the author's text was settled:\n%s", got)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{"s"}, Check: true, Printer: jp}); !errors.Is(err, ErrFmtRefused) {
		t.Fatalf("--check in a walk: %v, want ErrFmtRefused", err)
	}
	res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if err != nil {
		t.Fatalf("--to bot: %v", err)
	}
	if len(res.Notices) != 1 || !strings.HasPrefix(res.Notices[0], doc+":") || !strings.Contains(res.Notices[0], "[E053]") || strings.Count(res.Notices[0], "settled.bot.yaml") != 1 {
		t.Fatalf("the note does not name the reading once, under the name given: %q", res.Notices)
	}
	got, err := os.ReadFile("s/settled.bot")
	if err != nil || !strings.Contains(string(got), "prompt spaced:\n  Indented and surrounded by blank lines.\n  Second line.\n") {
		t.Fatalf("the .bot written does not carry the settled reading: %v\n%s", err, got)
	}
}

// `fmt --to bot` says the YAML comments the .bot is not written with — the
// mirror of `--to yaml`'s note on the .bot's comments — counting them and
// naming the first, so a ` #` that cut a plain value short is seen at the
// conversion: the .bot is still written, the value as YAML read it.
func TestFmtToBotSaysTheCommentsItDoesNotCarry(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "c/x.bot.yaml", "# a draft\ndsl: 2\nnodes:\n  - tool: status\n    command: echo \"see #123\"\nworkflow:\n  name: w\n  entry: status\n  edges:\n    - status -> done\n")
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if err != nil {
		t.Fatalf("--to bot: %v", err)
	}
	joined := strings.Join(res.Notices, "\n")
	if !strings.Contains(joined, doc+": 2 YAML comment line(s) are not carried into the .bot (the first: # a draft)") {
		t.Fatalf("the comments the .bot is not written with were not said: %q", res.Notices)
	}
	got, err := os.ReadFile("c/x.bot")
	if err != nil || !strings.Contains(string(got), `command: "echo \"see"`) {
		t.Fatalf("the .bot written does not carry the value as YAML read it: %v\n%s", err, got)
	}
	// The comment a ` #` started is named before the one written under it,
	// which yaml.v3 hangs on the key.
	under := writeBot(t, "u/x.bot.yaml", "dsl: 2\nnodes:\n  - tool: status\n    command: echo \"see #123\"\n    # retries are handled by the caller\nworkflow:\n  name: w\n  entry: status\n  edges:\n    - status -> done\n")
	res, err = RunFmt(FmtOptions{Paths: []string{under}, To: "bot", Printer: jp})
	if err != nil || !strings.Contains(strings.Join(res.Notices, "\n"), under+": 2 YAML comment line(s) are not carried into the .bot (the first: #123\")") {
		t.Fatalf("the note does not name first the comment the ` #` started: %v %q", err, res.Notices)
	}
	plain := writeBot(t, "p/x.bot.yaml", "dsl: 2\nnodes:\n  - tool: status\n    command: echo ready\nworkflow:\n  name: w\n  entry: status\n  edges:\n    - status -> done\n")
	res, err = RunFmt(FmtOptions{Paths: []string{plain}, To: "bot", Printer: jp})
	if err != nil || len(res.Notices) != 0 {
		t.Fatalf("a document without a comment: %v, notices %q — want none", err, res.Notices)
	}
}

// A document is rewritten on its own bytes, as a .bot is: a BOM and CRLF
// line endings are kept, and a canonical document written with them is
// already canonical to --check.
func TestFmtKeepsADocumentsOwnBytes(t *testing.T) {
	inTempWorkspace(t)
	canonical, err := author.Write(author.Parse("x.bot.yaml", []byte(looseHelloDoc)).File)
	if err != nil {
		t.Fatal(err)
	}
	withBytes := "\ufeff" + strings.ReplaceAll(string(canonical), "\n", "\r\n")
	doc := writeBot(t, "b/hello.bot.yaml", withBytes)
	jp, _ := jsonPrinter()
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, Check: true, Printer: jp}); err != nil {
		t.Fatalf("--check on a canonical document with a BOM and CRLF: %v", err)
	}
	loose := writeBot(t, "b/loose.bot.yaml", "\ufeff"+strings.ReplaceAll(looseHelloDoc, "\n", "\r\n"))
	if _, err := RunFmt(FmtOptions{Paths: []string{loose}, Printer: jp}); err != nil {
		t.Fatalf("fmt: %v", err)
	}
	if got, _ := os.ReadFile(loose); string(got) != withBytes {
		t.Fatalf("the BOM or the line endings were lost:\n%q\nwant\n%q", got, withBytes)
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
	if why := ir.SameProgram(ir.Compile(back.File), ir.Compile(parsertest.Parse(bot, botText).File)); why != "" {
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
	if strings.Contains(joined, "carries no `catalog:`") {
		t.Fatalf("a document that carries a catalog was said not to: %q", res.Notices)
	}

	broken := writeBot(t, "y/broken.bot", "agent :\n  model\n")
	res, err = RunFmt(FmtOptions{Paths: []string{broken}, To: "yaml", Force: true, Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "does not parse") {
		t.Fatalf("a .bot that does not parse was converted: %v %v", err, res.Refused)
	}
	if !strings.HasPrefix(res.Refused[0], broken+": does not parse: "+broken+":") || strings.Count(res.Refused[0], "broken.bot") != 2 {
		t.Fatalf("the refusal does not name the file as given — once for the file, once for the position: %q", res.Refused[0])
	}
	if _, err := os.Stat("y/broken.bot.yaml"); err == nil {
		t.Fatal("a document was written from a text the parser recovered on")
	}
}

// The document `--to yaml` writes reads back — as the same program — before
// it is written: a float the .bot spells with a leading 0 is written as the
// digits the reader takes, not as a spelling the next validate refuses; and
// a writer that renders a document the reader refuses, or reads as another
// program — a command, a field's type, a prompt changed in a fragment that
// compiles to no workflow as in a bot — is refused by the proof, nothing
// written.
func TestFmtToYamlWritesADocumentThatReadsBack(t *testing.T) {
	inTempWorkspace(t)
	botText := "dsl: 2\n\nvars:\n  ratio: float = 01.5\n  n: int = 010\n\nagent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n\n  a -> done\n"
	bot := writeBot(t, "r/numbers.bot", botText)
	jp, _ := jsonPrinter()
	if _, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("--to yaml: %v", err)
	}
	out, err := os.ReadFile("r/numbers.bot.yaml")
	if err != nil {
		t.Fatalf("the document was not written: %v", err)
	}
	back := author.Parse("r/numbers.bot.yaml", out)
	if back.HasErrors() {
		t.Fatalf("the document written does not read: %v\n%s", back.Diagnostics, out)
	}
	if why := ir.SameProgram(ir.Compile(back.File), ir.Compile(parsertest.Parse(bot, botText).File)); why != "" || !strings.Contains(string(out), "default: 1.5\n") {
		t.Fatalf("the document written is not the .bot's program, its float in the reader's digits (%s):\n%s", why, out)
	}
	// A .bot that compiles to no workflow — a fragment, the same literals —
	// is compared as re-read, serialised ASTs, each literal by its value; and
	// a parameter a vendor names `<<` is written as the text, not YAML's
	// merge key.
	for path, text := range map[string]string{
		"r/lib/numbers.bot": "dsl: 2\n\nvars:\n  ratio: float = 01.5\n  n: int = 010\n\nagent a:\n  model: \"m\"\n",
		"r/lib/profile.bot": "prompt ask:\n  Say yes.\n",
		// The writer puts `system:` before `user:`: the inline prompts come
		// back in another order, which says nothing.
		"r/lib/inline.bot":    "dsl: 2\n\nagent a:\n  model: \"m\"\n  user: \"the user's text\"\n  system: \"the system's text\"\n",
		"r/lib/import.bot":    "dsl: 2\nimport \"lib/shared.bot\"\n\ntool clean:\n  command: \"rm -rf build\"\n\nprompt ask:\n  Say yes.\n\nschema verdict:\n  ok: bool\n",
		"r/lib/reordered.bot": "dsl: 2\n\nschema z:\n  ok: bool\n\nschema a:\n  ok: bool\n\nprompt z:\n  Zeta.\n\nprompt a:\n  Alpha.\n\ntool z:\n  command: \"zeta\"\n\ntool a:\n  command: \"alpha\"\n",
		"r/merge.bot":         "dsl: 2\n\ntool c:\n  action: forgejo.issue.comment\n  connection: \"forge-main\"\n  params:\n    \"<<\": \"x\"\n\nworkflow w:\n  entry: c\n\n  c -> done\n",
	} {
		frag := writeBot(t, path, text)
		if _, err := RunFmt(FmtOptions{Paths: []string{frag}, To: "yaml", Printer: jp}); err != nil {
			t.Fatalf("--to yaml %s: %v", path, err)
		}
		if out, err := os.ReadFile(path + ".yaml"); err != nil || author.Parse(path+".yaml", out).HasErrors() {
			t.Fatalf("the document of %s was not written, or does not read: %v\n%s", path, err, out)
		}
	}
	// A prompt that includes a file resolves it beside the .bot, a file on
	// disk: the proof reads the document back where the .bot is, not under
	// the document's own name, which nothing has written yet.
	writeBot(t, "r/rules.md", "Be brief.\n")
	inc := writeBot(t, "r/include.bot", "dsl: 2\n\nprompt ask:\n  Say hello. {{include \"rules.md\"}}\n\nagent a:\n  model: \"m\"\n  system: ask\n\nworkflow w:\n  entry: a\n\n  a -> done\n")
	if res, err := RunFmt(FmtOptions{Paths: []string{inc}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("--to yaml of a .bot whose prompt includes a file beside it: %v %q", err, res.Refused)
	}
	if _, err := os.Stat("r/include.bot.yaml"); err != nil {
		t.Fatalf("the document of a .bot with an include was not written: %v", err)
	}

	defer func(w func(*ast.File) ([]byte, error)) { writeDocument = w }(writeDocument)
	doc := "dsl: 2\nnodes:\n  - agent: a\n    model: m\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n"
	fragment := "dsl: 2\nnodes:\n  - tool: clean\n    command: rm -rf build\nschemas:\n  verdict:\n    ok: bool\nprompts:\n  ask: Say yes.\n"
	for _, tc := range []struct{ bot, written, want string }{
		{"dsl: 2\n\nagent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n\n  a -> done\n", strings.Replace(doc, "    model: m\n", "    model: m\n    max_tokens: 010\n", 1), "the document written does not read back: r/other.bot.yaml:5:17: error [E051]"},
		{"dsl: 2\n\nagent a:\n  model: \"m\"\n\nworkflow w:\n  entry: a\n\n  a -> done\n", strings.Replace(doc, "model: m", "model: n", 1), "the document written reads back as another program"},
		{"dsl: 2\n\nprompt ask:\n  Say yes.\n\nschema verdict:\n  ok: bool\n\ntool clean:\n  command: \"rm -rf build\"\n", strings.Replace(fragment, "rm -rf build", "rm -rf /", 1), "the document written reads back as another program"},
		{"dsl: 2\n\nprompt ask:\n  Say yes.\n\nschema verdict:\n  ok: bool\n\ntool clean:\n  command: \"rm -rf build\"\n", strings.Replace(fragment, "ok: bool", "ok: string", 1), "the document written reads back as another program"},
		{"dsl: 2\n\nprompt ask:\n  Say yes.\n\nschema verdict:\n  ok: bool\n\ntool clean:\n  command: \"rm -rf build\"\n", strings.Replace(fragment, "Say yes.", "Say no.", 1), "the document written reads back as another program"},
	} {
		written := tc.written
		writeDocument = func(*ast.File) ([]byte, error) { return []byte(written), nil }
		other := writeBot(t, "r/other.bot", tc.bot)
		res, err := RunFmt(FmtOptions{Paths: []string{other}, To: "yaml", Printer: jp})
		if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], tc.want) {
			t.Fatalf("a document the proof must refuse was converted: %v %q, want %q\n--- written:\n%s", err, res.Refused, tc.want, written)
		}
		if _, err := os.Stat("r/other.bot.yaml"); err == nil {
			t.Fatal("a document that does not read back as the program was written")
		}
	}
}

// Where no workflow compiles, the rank of prompts and schemas is not a
// change of the fragment: the compiled program holds them by name. The
// read-back proof accepts a writer that emits them in another order, and
// still writes the document it proved.
func TestFmtToYamlAcceptsAFragmentReadBackWithReorderedDeclarations(t *testing.T) {
	inTempWorkspace(t)
	bot := writeBot(t, "r/reordered.bot", "dsl: 2\n\nprompt z:\n  Zeta.\n\nprompt a:\n  Alpha.\n\nschema z:\n  ok: bool\n\nschema a:\n  ok: bool\n")
	written := "dsl: 2\nschemas:\n  a:\n    ok: bool\n  z:\n    ok: bool\nprompts:\n  a: Alpha.\n  z: Zeta.\n"
	defer func(w func(*ast.File) ([]byte, error)) { writeDocument = w }(writeDocument)
	writeDocument = func(*ast.File) ([]byte, error) { return []byte(written), nil }
	jp, _ := jsonPrinter()
	if res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("a reordered fragment was refused: %v %q", err, res.Refused)
	}
	if out, err := os.ReadFile(bot + ".yaml"); err != nil || string(out) != written {
		t.Fatalf("the proved document was not written: %v\n%s", err, out)
	}
}

func fmtTwoUsesBot(first, second string) string {
	use := map[string]string{
		"one": "use pair as r1 with {\n  label: \"one\",\n}\n\n",
		"two": "use pair as r2 with {\n  label: \"two\",\n}\n\n",
	}
	return "dsl: 2\n\nschema out:\n  ok: bool\n\ngroup pair(label):\n" +
		"  tool first:\n    command: \"printf '{\\\"ok\\\":true}' # {{params.label}}\"\n    output: out\n" +
		"  tool second:\n    command: \"printf '{\\\"ok\\\":true}'\"\n    output: out\n" +
		"  first -> second\n\n" + use[first] + use[second] +
		"workflow w:\n  entry: r1.first\n  r1.second -> r2.first\n  r2.second -> done\n"
}

// A declaration order the launcher reads is not a spelling the write proof
// may normalise. Supervisors land in an ordered IR slice, uses feed ordered
// edges, and imports decide the order the unit loader merges their fragments.
// Each mutation is refused before a write, with unit.LoadDir + SameProgram as
// the premise that the two orders really are two programs.
func TestFmtToYamlRefusesAFragmentReadBackWhoseOrderChangesTheProgram(t *testing.T) {
	useOriginal, useWorkflow, _ := strings.Cut(fmtTwoUsesBot("one", "two"), "workflow w:")
	useReordered, _, _ := strings.Cut(fmtTwoUsesBot("two", "one"), "workflow w:")
	const supervisorMain = "dsl: 2\nimport \"lib/fragment.bot\"\n\nprompt ask:\n  Work.\n\nprompt pol_a:\n  Policy A.\n\nprompt pol_b:\n  Policy B.\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow w:\n  entry: hello\n  hello -> done\n"
	for _, tc := range []struct {
		name, main, original, reordered, want string
		extra                                 map[string]string
	}{
		{
			name: "supervisors", main: supervisorMain,
			original:  "dsl: 2\n\nsupervisor beta:\n  watches: [hello]\n  model: \"m\"\n  system: pol_b\n\nsupervisor alpha:\n  watches: [hello]\n  model: \"m\"\n  system: pol_a\n",
			reordered: "dsl: 2\n\nsupervisor alpha:\n  watches: [hello]\n  model: \"m\"\n  system: pol_a\n\nsupervisor beta:\n  watches: [hello]\n  model: \"m\"\n  system: pol_b\n",
			want:      "Supervisors",
		},
		{
			name: "uses", main: "dsl: 2\nimport \"lib/fragment.bot\"\n\nworkflow w:" + useWorkflow,
			original: useOriginal, reordered: useReordered,
			want: "edge r1.first -> r1.second differs",
		},
		{
			name: "imports", main: supervisorMain,
			original:  "dsl: 2\nimport \"a.bot\"\nimport \"z.bot\"\n",
			reordered: "dsl: 2\nimport \"z.bot\"\nimport \"a.bot\"\n",
			want:      "Supervisors",
			extra: map[string]string{
				"lib/a.bot": "dsl: 2\n\nsupervisor alpha:\n  watches: [hello]\n  model: \"m\"\n  system: pol_a\n",
				"lib/z.bot": "dsl: 2\n\nsupervisor beta:\n  watches: [hello]\n  model: \"m\"\n  system: pol_b\n",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inTempWorkspace(t)
			for _, dir := range []string{"original", "reordered"} {
				writeBot(t, dir+"/main.bot", tc.main)
				for path, text := range tc.extra {
					writeBot(t, dir+"/"+path, text)
				}
			}
			fragment := writeBot(t, "original/lib/fragment.bot", tc.original)
			writeBot(t, "reordered/lib/fragment.bot", tc.reordered)
			a := ir.Compile(unittest.Whole(unit.LoadDir("original/main.bot")).Merged)
			b := ir.Compile(unittest.Whole(unit.LoadDir("reordered/main.bot")).Merged)
			if why := ir.SameProgram(a, b); why == "" || !strings.Contains(why, tc.want) {
				t.Fatalf("the fixture's orders are not the expected two programs: %q, want a difference naming %q", why, tc.want)
			}

			reordered := parsertest.Parse("reordered/lib/fragment.bot", tc.reordered)
			if diagnosticErrors(reordered.Diagnostics) != "" {
				t.Fatalf("the reordered fragment does not parse: %v", reordered.Diagnostics)
			}
			written, err := author.Write(reordered.File)
			if err != nil {
				t.Fatal(err)
			}
			defer func(w func(*ast.File) ([]byte, error)) { writeDocument = w }(writeDocument)
			writeDocument = func(*ast.File) ([]byte, error) { return written, nil }
			jp, _ := jsonPrinter()
			res, conversionErr := RunFmt(FmtOptions{Paths: []string{fragment}, To: "yaml", Printer: jp})
			if !errors.Is(conversionErr, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "reads back as another program") {
				t.Fatalf("a semantic reorder was accepted: %v %q", conversionErr, res.Refused)
			}
			if _, err := os.Stat(fragment + ".yaml"); err == nil {
				t.Fatal("a document whose declaration order changes the program was written")
			}
		})
	}
}

// The `catalog:` a document carries is what the catalog reader reads off
// the .bot's frontmatter — the one reading (workflowfile.DecodeFrontmatter)
// — and the note says, off the document written, when it carries none and
// why: a key named twice keeps its last value, as the catalogue has always
// read it; a scalar where a list is expected leaves the .bot with no
// identity and the document with no `catalog:`; a block with none of the
// four keys the same.
func TestFmtToYamlCarriesTheCatalogTheCatalogReaderReads(t *testing.T) {
	inTempWorkspace(t)
	body := "\ndsl: 2\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n"
	for name, tc := range map[string]struct {
		head     string
		catalog  bool
		noNote   string // a fragment no note may carry
		wantNote string // a fragment one note carries
	}{
		"a key named twice": {"## ---\n## name: first\n## description: d\n## name: last\n## ---\n", true, "carries no `catalog:`", ""},
		"a scalar triggers": {"## ---\n## name: probe\n## triggers: just-one\n## ---\n", false, "", "the catalog reader reads"},
		// A frontmatter that is not YAML at all arrives with the remedy of
		// the author document's table: the same author, the same mistake.
		"a value holding a colon": {"## ---\n## name: probe\n## description: a: b\n## ---\n", false, "", "quote the whole value"},
		"none of the four":        {"## ---\n## owner: jo\n## ---\n", false, "", "none of the four keys"},
		"the four, and more":      {"## ---\n## name: probe\n## owner: jo\n## ---\n", true, "carries no `catalog:`", "1 key(s) of the frontmatter are not carried by `catalog:` (owner)"},
	} {
		t.Run(name, func(t *testing.T) {
			bot := writeBot(t, "c/"+strings.ReplaceAll(name, " ", "_")+".bot", tc.head+body)
			jp, _ := jsonPrinter()
			res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp})
			if err != nil {
				t.Fatalf("--to yaml: %v", err)
			}
			doc, _ := os.ReadFile(bot + ".yaml")
			if has := strings.Contains(string(doc), "\ncatalog:\n"); has != tc.catalog {
				t.Fatalf("the document carries a catalog: %v, want %v\n%s\nnotes: %q", has, tc.catalog, doc, res.Notices)
			}
			// What the catalogue reads off the .bot, it reads off the .bot
			// the document writes back.
			want := testFrontmatter(t, []byte(tc.head+body))
			got := testFrontmatter(t, []byte(unparse.Unparse(author.Parse(bot+".yaml", doc).File)))
			if catalogIdentity(got) != catalogIdentity(want) {
				t.Fatalf("the .bot the document writes back carries %s, the original %s", catalogIdentity(got), catalogIdentity(want))
			}
			joined := strings.Join(res.Notices, "\n")
			if tc.noNote != "" && strings.Contains(joined, tc.noNote) {
				t.Fatalf("a note says %q of a document that carries a catalog: %q", tc.noNote, res.Notices)
			}
			if tc.wantNote != "" && !strings.Contains(joined, tc.wantNote) {
				t.Fatalf("no note says %q: %q", tc.wantNote, res.Notices)
			}
		})
	}
}

// The comment lines of a frontmatter block that did not become the
// document's `catalog:` — not closed, not read, none of the four keys — are
// lost with the head's other comments, and counted with them; a block that
// did is represented, and only the comments beyond it are counted.
func TestFmtToYamlCountsTheLinesOfABlockTheDocumentDoesNotCarry(t *testing.T) {
	inTempWorkspace(t)
	body := "\ndsl: 2\n\nprompt ask:\n  Say hello.\n\n## the agent\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n"
	for name, tc := range map[string]struct {
		head string
		want string
	}{
		"not closed":         {"## ---\n## name: probe\n## more prose\n", "4 comment line(s) are not represented"},
		"not read":           {"## ---\n## name: probe\n## triggers: nope\n## ---\n", "5 comment line(s) are not represented"},
		"none of the four":   {"## ---\n## owner: jo\n## ---\n", "4 comment line(s) are not represented"},
		"carried, a control": {"## ---\n## name: probe\n## ---\n", "1 comment line(s) are not represented"},
	} {
		t.Run(name, func(t *testing.T) {
			bot := writeBot(t, "l/"+strings.ReplaceAll(name, " ", "_")+".bot", tc.head+body)
			jp, _ := jsonPrinter()
			res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Printer: jp})
			if err != nil {
				t.Fatalf("--to yaml: %v", err)
			}
			if joined := strings.Join(res.Notices, "\n"); !strings.Contains(joined, tc.want) {
				t.Fatalf("the lines the document does not carry were not counted (%q): %q", tc.want, res.Notices)
			}
		})
	}
}

// catalogIdentity renders a catalog identity for a comparison, an empty one
// as none.
func catalogIdentity(fm *bundle.Frontmatter) string {
	if fm.Empty() {
		return "(none)"
	}
	return fmt.Sprintf("name=%q description=%q triggers=%q capabilities=%q", fm.Name, fm.Description, fm.Triggers, fm.Capabilities)
}

// In a walk, fmt gives an author document its canonical YAML form as it
// gives a .bot its canonical text — proven the same program, idempotent,
// said by --check — and the .bot beside it is formatted all the same.
func TestFmtCanonicalisesADocumentInAWalk(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "w/hello.bot.yaml", looseHelloDoc)
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
	want, err := author.Write(author.Parse(doc, []byte(looseHelloDoc)).File)
	if err != nil || string(got) != string(want) {
		t.Fatalf("the document is not in its canonical form (%v):\n%s\nwant\n%s", err, got, want)
	}
	if unparse.Unparse(author.Parse(doc, got).File) != unparse.Unparse(author.Parse(doc, []byte(looseHelloDoc)).File) {
		t.Fatal("the canonical document is another program")
	}
	if b, _ := os.ReadFile(bot); string(b) == looseBot {
		t.Fatal("the .bot beside the document was not formatted")
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{"w"}, Check: true, Printer: jp}); err != nil {
		t.Fatalf("--check after fmt: %v", err)
	}
}

// A document that carries YAML comments is rewritten in a walk in its
// canonical form with its head, line and foot comments where they were, and
// --check is green on it afterwards; --to bot still reads it, since the .bot
// it writes carries the program, not the document.
func TestFmtRewritesACommentedDocumentKeepingItsComments(t *testing.T) {
	inTempWorkspace(t)
	loose := strings.Replace(authorHelloDoc, "  - agent: hello\n    model: m\n    system: ask\n", "- agent: hello\n  model: m\n  system: ask\n", 1)
	if loose == authorHelloDoc {
		t.Fatal("the fixture no longer holds the sequence this test moves to the margin")
	}
	commented := "# keep me\n" + strings.Replace(loose, "  ask: Say hello.\n", "  ask: Say hello. # and me\n", 1) + "# the foot\n"
	doc := writeBot(t, "k/hello.bot.yaml", commented)
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{"k"}, Printer: jp})
	if err != nil || len(res.Refused) != 0 {
		t.Fatalf("a commented document was refused: %v %v", err, res.Refused)
	}
	want := "# keep me\n" + strings.Replace(authorHelloDoc, "  ask: Say hello.\n", "  ask: Say hello. # and me\n", 1) + "# the foot\n"
	if got, _ := os.ReadFile(doc); string(got) != want {
		t.Fatalf("the commented document was not rewritten with its comments in place:\n%s\nwant\n%s", got, want)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{"k"}, Check: true, Printer: jp}); err != nil {
		t.Fatalf("--check after fmt on a commented document: %v", err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp}); err != nil {
		t.Fatalf("--to bot on a commented document: %v", err)
	}
	if _, err := os.Stat("k/hello.bot"); err != nil {
		t.Fatalf("the .bot of a commented document was not written: %v", err)
	}
}

// footTwinHelloDoc carries `# twice` as a mid-document FOOT comment — a
// blank line below it — so the same text written in the trailing region is a
// second comment, over nodes that carry none in between.
const footTwinHelloDoc = "dsl: 2\nprompts:\n  ask: Say hello.\n  # twice\n\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// quotedHelloDoc ends on a multi-line QUOTED value whose last line starts
// with `#`: the author's text up to the closing quote, and no comment.
const quotedHelloDoc = "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\nprompts:\n  ask: \"Say hello.\n    # not a comment\"\n"

// flowCloseHelloDoc writes the same comment text twice around a flow
// collection — inside it and after its closing bracket — on a closing line
// whose own value holds a `#`, which a reader of that line must look past
// to find the comment; flowBareCloseHelloDoc writes the pair as a bare `#`,
// which a scan one character short of the line's end never meets.
const flowCloseHelloDoc = "dsl: 2\nprompts: {ask: \"Say hello.\", # twice\n  other: \"a # b\"} # twice\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

const flowBareCloseHelloDoc = "dsl: 2\nprompts: {ask: \"Say hello.\", #\n  other: b} #\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"

// commentLinesOf counts the comment lines text carries, independently of the
// reader `fmt`'s own guard calls: a comment runs to the end of its line, so
// a line carries at most ONE. Every `#` opening a line or written after a
// blank anywhere else on it is a candidate — the first of a line may sit
// inside a quoted value and the comment after it — minus the texts the
// caller declares as the author's own; the first candidate that survives
// that subtraction is the line's comment. A `strings.Contains` of one kept
// text cannot see a lost duplicate of it; this count can.
func commentLinesOf(text string, notComments ...string) string {
	skip := map[string]int{}
	for _, l := range notComments {
		skip[l]++
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		for ci := 0; ci < len(line); ci++ {
			if line[ci] != '#' || (ci > 0 && line[ci-1] != ' ' && line[ci-1] != '\t') {
				continue
			}
			found := strings.TrimSpace(line[ci:])
			if skip[found] > 0 {
				skip[found]--
				continue
			}
			out = append(out, found)
			break
		}
	}
	sort.Strings(out)
	return strings.Join(out, "|")
}

// The count `fmt`'s tables measure against is held to what YAML says a
// comment is: one per line, running to the end of it. A text carrying a
// ` #` of its own is ONE comment, and the candidate the declared-text
// subtraction leaves is the line's — the quoted `#` of a flow collection's
// closing line skipped, the comment after it taken.
func TestCommentLinesOfCountsOneCommentPerLine(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		notComments      []string
	}{
		{name: "a comment carrying a hash of its own", text: "a: b # c # d\n", want: "# c # d"},
		{name: "the same, on its own line", text: "# c # d\na: b\n", want: "# c # d"},
		{name: "three hashes in one comment", text: "a: b # 1 # 2 # 3\n", want: "# 1 # 2 # 3"},
		{name: "a declared quoted hash, the comment after it", text: "  other: \"a # b\"} # twice\n", notComments: []string{`# b"} # twice`}, want: "# twice"},
		{name: "a line with no comment at all", text: "a: b\n", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commentLinesOf(tc.text, tc.notComments...); got != tc.want {
				t.Fatalf("commentLinesOf(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// A comment written below the last content line that no node holds — the
// comment on a `...` marker's own line, a block the one below the marker
// displaces, anything under a commented marker line — used to make `fmt`
// refuse the document: the writer emitted no marker, so the canonical form
// had no place for it. Since #2359 the writer re-emits the marker with the
// region it sits in: `fmt` formats these documents, every comment line of
// the author's file is in the result, the marker lines are still there, and
// `--check` is green afterwards. `--to bot` is the other half and does not
// move: a .bot carries no YAML comment, so it counts them and names the
// first. A comment the rewrite keeps on a node, and a `#` that is text, are
// rewritten all the same.
func TestFmtKeepsTheCommentsOfADocumentEndMarker(t *testing.T) {
	inTempWorkspace(t)
	for _, tc := range []struct {
		dir, body, tail, first string
		n                      int
	}{
		{"m", authorHelloDoc, "... # end note\n", "# end note", 1},
		{"a", authorHelloDoc, "# before\n...\n# after\n", "# before", 2},
		{"b", authorHelloDoc, "...  # end\n# after\n", "# end", 2},
		{"r", authorHelloDoc, "... # one\n...  # two\n", "# one", 2},
		// A block a blank line keeps off the node above AND the block
		// below a bare marker displaces off the document's foot is hung on
		// nothing: the refusal is the only thing between it and a rewrite
		// that deletes it without a word.
		{"d", authorHelloDoc, "\n# p\n\n...\n# q\n", "# p", 2},
		{"s", authorHelloDoc, "\n# x\n\n...  # x\n", "# x", 2},
		// The SAME text as a mid-document foot comment and on the marker's
		// line: two comments, one of which no written form can hold. Read
		// as one, the marker's line is deleted and the rewrite says nothing.
		{"ft", footTwinHelloDoc, "...  # twice\n", "# twice", 2},
		{"ft2", footTwinHelloDoc, "...\n\n# twice\n", "# twice", 2},
		{"ft3", footTwinHelloDoc, "...\n...\n# twice\n", "# twice", 2},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			src := tc.body + tc.tail
			doc := writeBot(t, tc.dir+"/end.bot.yaml", src)
			jp, _ := jsonPrinter()
			res, err := RunFmt(FmtOptions{Paths: []string{doc}, Printer: jp})
			if err != nil || len(res.Refused) != 0 {
				t.Fatalf("a document whose marker region the writer re-emits was refused: %v %q", err, res.Refused)
			}
			got, _ := os.ReadFile(doc)
			if had, kept := commentLinesOf(src), commentLinesOf(string(got)); had != kept {
				t.Fatalf("the rewrite carries the comment lines %q, the author's file %q:\n%s", kept, had, got)
			}
			if had, kept := markerLinesOf(src), markerLinesOf(string(got)); had != kept {
				t.Fatalf("the rewrite carries %d marker line(s), the author's file %d:\n%s", kept, had, got)
			}
			if _, err := RunFmt(FmtOptions{Paths: []string{tc.dir}, Check: true, Printer: jp}); err != nil {
				t.Fatalf("--check after fmt: %v", err)
			}
			res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
			want := fmt.Sprintf("%s: %d YAML comment line(s) are not carried into the .bot (the first: %s)", doc, tc.n, tc.first)
			if err != nil || !strings.Contains(strings.Join(res.Notices, "\n"), want) {
				t.Fatalf("--to bot did not count the marker's comments: %v %q, want %q", err, res.Notices, want)
			}
		})
	}
	// An INDENTED comment above the marker is one yaml.v3 hangs on the last
	// node it is written under: the rewrite keeps it, so the document is
	// rewritten rather than refused, --check is green afterwards and `--to
	// bot` counts it ONCE. The same holds of a block scalar's body line
	// that starts with `#`, which is no comment at all. The rows marked
	// `same` are the ones #2359 turned into a NO-OP: body and marker region
	// already in the writer's own form, `fmt` hands the bytes back as they
	// are — which is the acceptance the ticket asked for.
	for _, tc := range []struct {
		dir, src string
		text     []string
		n        int
		same     bool
	}{
		{dir: "i", src: authorHelloDoc + "  # inner\n...\n", n: 1, same: true},
		{dir: "i2", src: authorHelloDoc + "  # i1\n...\n# below\n", n: 2, same: true},
		{dir: "i3", src: authorHelloDoc + "  # i1\n# i2\n...\n", n: 2, same: true},
		{dir: "bs", src: strings.Replace(authorHelloDoc, "  ask: Say hello.\n", "  ask: |\n    Say hello.\n    # Role\n", 1) + "...\n", text: []string{"# Role"}, n: 0},
		// A mid-document FOOT comment and its twin above a bare marker are
		// two comments the rewrite keeps where they hang: read as one, the
		// second is deleted and the rewrite says nothing.
		{dir: "ft4", src: footTwinHelloDoc + "# twice\n...\n", n: 2},
		{dir: "ft5", src: footTwinHelloDoc + "\n# twice\n", n: 2},
		// A multi-line QUOTED value whose last line starts with `#` carries
		// no comment at all: read as one, the document is unformattable and
		// `--to bot` names the author's own prose.
		{dir: "q", src: quotedHelloDoc, text: []string{`# not a comment"`, "# not a comment'"}, n: 0},
		{dir: "q2", src: quotedHelloDoc + "...\n", text: []string{`# not a comment"`, "# not a comment'"}, n: 0},
		// The same text written INSIDE a flow collection and after its
		// closing bracket is two comments: read as one, `fmt` left these
		// two files alone for a comment it said it would write twice,
		// though its own rewrite carries both.
		{dir: "fc", src: flowCloseHelloDoc, text: []string{`# b"} # twice`, "# b'"}, n: 2},
		{dir: "fcb", src: flowBareCloseHelloDoc, n: 2},
	} {
		t.Run(tc.dir+" is rewritten", func(t *testing.T) {
			doc := writeBot(t, tc.dir+"/end.bot.yaml", tc.src)
			jp, _ := jsonPrinter()
			res, err := RunFmt(FmtOptions{Paths: []string{doc}, Printer: jp})
			if err != nil || len(res.Refused) != 0 {
				t.Fatalf("a document whose rewrite keeps every comment was refused: %v %q", err, res.Refused)
			}
			got, _ := os.ReadFile(doc)
			if tc.same && string(got) != tc.src {
				t.Fatalf("a document already in the writer's form was rewritten:\n%q\nwant\n%q", got, tc.src)
			}
			if !tc.same && string(got) == tc.src {
				t.Fatalf("the document was not rewritten:\n%s", got)
			}
			if had, kept := commentLinesOf(tc.src, tc.text...), commentLinesOf(string(got), tc.text...); had != kept {
				t.Fatalf("the rewrite carries the comment lines %q, the author's file %q:\n%s", kept, had, got)
			}
			if _, err := RunFmt(FmtOptions{Paths: []string{tc.dir}, Check: true, Printer: jp}); err != nil {
				t.Fatalf("--check after fmt: %v", err)
			}
			res, err = RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
			if err != nil {
				t.Fatalf("--to bot: %v", err)
			}
			notices := strings.Join(res.Notices, "\n")
			if tc.n == 0 {
				if strings.Contains(notices, "YAML comment line(s)") {
					t.Fatalf("--to bot counted a comment a document carries none of: %q", res.Notices)
				}
				return
			}
			want := fmt.Sprintf("%s: %d YAML comment line(s) are not carried into the .bot", doc, tc.n)
			if !strings.Contains(notices, want) {
				t.Fatalf("--to bot counted the comments otherwise: %q, want %q", res.Notices, want)
			}
		})
	}
	t.Run("a hash that is text", func(t *testing.T) {
		src := strings.Replace(looseHelloDoc, "ask: Say hello.", `ask: "a#b and a # inside quotes"`, 1)
		if src == looseHelloDoc {
			t.Fatal("the fixture no longer holds the prompt this test rewrites")
		}
		doc := writeBot(t, "t/text.bot.yaml", src)
		jp, _ := jsonPrinter()
		res, err := RunFmt(FmtOptions{Paths: []string{doc}, Printer: jp})
		if err != nil || len(res.Refused) != 0 {
			t.Fatalf("a document whose only `#` are text was refused: %v %q", err, res.Refused)
		}
		got, _ := os.ReadFile(doc)
		if string(got) == src || !strings.Contains(string(got), "a#b and a # inside quotes") {
			t.Fatalf("the text was not rewritten, or the `#` of its value did not survive:\n%s", got)
		}
	})
}

// markerLinesOf counts the explicit document markers of text: a `---` or a
// `...` at column 0, followed by a blank or the end of its line, which is
// where YAML reads one.
func markerLinesOf(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "---") && !strings.HasPrefix(line, "...") {
			continue
		}
		if len(line) == 3 || line[3] == ' ' || line[3] == '\t' {
			n++
		}
	}
	return n
}

// headerCommentDoc declares its script's indentation with a header whose
// comment opens against the indicators (`|2#c`, no blank before the `#`,
// which the scanner reads as a comment all the same), and its body holds a
// line starting with `#` above the column its first line sits at.
const headerCommentDoc = "dsl: 2\nworkflow:\n  name: hello\n  entry: run\n  edges:\n    - run -> done\nnodes:\n  - tool: run\n    language: sh\n    script: |2#c\n        echo one\n      # not a comment\n"

// A header read short would take that body line for a comment the written
// form loses, and `fmt` would refuse a document it rewrites whole: the file
// is rewritten, nothing is named among the refusals, the body line survives
// and `--check` is green afterwards.
func TestFmtRewritesADocumentWhoseHeaderCommentOpensAgainstItsIndicators(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "hc/end.bot.yaml", headerCommentDoc)
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc}, Printer: jp})
	if err != nil || len(res.Refused) != 0 {
		t.Fatalf("a document whose rewrite keeps every comment was refused: %v %q", err, res.Refused)
	}
	got, _ := os.ReadFile(doc)
	if string(got) == headerCommentDoc {
		t.Fatalf("the document was not rewritten:\n%s", got)
	}
	if !strings.Contains(string(got), "# not a comment") {
		t.Fatalf("the rewrite dropped the body line the header declares:\n%s", got)
	}
	if !strings.Contains(string(got), "#c") {
		t.Fatalf("the rewrite dropped the header's own comment:\n%s", got)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{"hc"}, Check: true, Printer: jp}); err != nil {
		t.Fatalf("--check after fmt: %v", err)
	}
}

// The --force note counts the YAML comment lines the document it replaces
// carries, and the author's own prose is none of them: a `#` opening a line
// of a multi-line quoted value, or written inside one, is that value's
// text — wherever yaml.v3 puts the value's position, which is its anchor or
// its tag when it carries one, in the shorthand form (`!!str`, `!a'b`) or in
// the verbatim one (`!<a'b>`, delimited by its `>`). This is the surface
// that reads a document's comments with NO .bot validity check in the way
// (an anchor is E050 at every other door), so it is where that count is
// proven; the last row is the control that the note still fires on a comment
// the target really carries.
func TestFmtForceCountsNoCommentInsideAPropertiedValue(t *testing.T) {
	inTempWorkspace(t)
	jp, _ := jsonPrinter()
	bot := writeBot(t, "p/x.bot", looseBot)
	carried := func(res FmtResult) string {
		for _, n := range res.Notices {
			if strings.Contains(n, "--force replaces it whole") {
				return n
			}
		}
		return ""
	}
	for _, tc := range []struct{ name, target, want string }{
		{"an anchor on a line of its own", "dsl: 2\nprompts:\n  ask: &a\n    \"Say hello.\n    # not a comment\"\n", ""},
		{"a tag on a line of its own", "dsl: 2\nprompts:\n  ask: !!str\n    \"Say hello.\n    # not a comment\"\n", ""},
		{"a comment carrying a quote between the two", "dsl: 2\nprompts:\n  ask: &a # \" anchored\n    \"Say hello.\n    # not a comment\"\n", `(the first: # " anchored)`},
		{"a tagged quoted value inside a flow collection", "dsl: 2\nprompts: {ask: !!str \"Say hello. # not a comment\"}\n", ""},
		{"an anchored quoted value inside one, over lines", "dsl: 2\nprompts: {ask: &a\n    \"Say hello.\n    # not a comment\"}\n", ""},
		{"a verbatim tag name carrying a quote", "dsl: 2\nprompts:\n  ask: !<a'b>\n    'Say hello.\n    # not a comment'\n", ""},
		{"a verbatim tag on a quoted value inside a flow collection", "dsl: 2\nprompts: {ask: !<a> \"Say hello. # not a comment\"}\n", ""},
		{"a body indentation declared behind a verbatim tag", "dsl: 2\nprompts:\n  ask: !<a> |2\n      echo one\n    # not a comment\n", ""},
		{"a comment the target really carries", "# real\ndsl: 2\nprompts:\n  ask: &a\n    \"Say hello.\n    # not a comment\"\n", "(the first: # real)"},
		// The property line carries a COMMENT, and the comment an
		// indicator: the block scalar below declares nothing, so its body
		// is the author's text and the count is the one comment the line
		// itself carries — not two.
		{"a `|9` written in the comment of a property line", "dsl: 2\nprompts:\n  ask: &a # |9\n    |\n    echo one\n    # not a comment\n", "1 comment line(s) it carries are not in what p/x.bot writes (the first: # |9)"},
		{"a `>9` written in the comment of a verbatim tag's line", "dsl: 2\nprompts:\n  ask: !<a> # >9\n    |\n    echo one\n    # not a comment\n", "1 comment line(s) it carries are not in what p/x.bot writes (the first: # >9)"},
		{"a comment of a property line with no indicator in it", "dsl: 2\nprompts:\n  ask: &a # plain\n    |\n    echo one\n    # not a comment\n", "1 comment line(s) it carries are not in what p/x.bot writes (the first: # plain)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile("p/x.bot.yaml", []byte(tc.target), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Force: true, Printer: jp})
			if err != nil {
				t.Fatalf("--to yaml --force: %v", err)
			}
			switch note := carried(res); {
			case tc.want == "" && note != "":
				t.Fatalf("--force named the author's own text a lost comment line: %q", note)
			case tc.want != "" && !strings.Contains(note, tc.want):
				t.Fatalf("--force counted the comments otherwise: %q, want %q", note, tc.want)
			}
		})
	}
}

// `--to yaml --force` reads its target with no .bot validity check, so it
// is where the comments of a flow collection holding a DANGLING key are
// counted for real: yaml.v3 hands the comment written after that key back
// MOVED onto the collection, beside whatever the closing line and the
// block below it carry. The note counts the lines the target writes — the
// moved copy once, a same text written further down where it is written,
// and a block scalar's body line not at all — never one line more than the
// target holds.
func TestFmtForceCountsAMovedFlowCommentOnce(t *testing.T) {
	inTempWorkspace(t)
	jp, _ := jsonPrinter()
	bot := writeBot(t, "p/x.bot", looseBot)
	carried := func(res FmtResult) string {
		for _, n := range res.Notices {
			if strings.Contains(n, "--force replaces it whole") {
				return n
			}
		}
		return ""
	}
	for _, tc := range []struct{ name, target, want string }{
		{"the moved text alone", "dsl: 2\nprompts: {ask: # c\n  }\nother: y\n", "1 comment line(s) it carries are not in what p/x.bot writes (the first: # c)"},
		{"the same text on a content line below the close", "dsl: 2\nprompts: {ask: # c\n  }\nother: y # c\n", "2 comment line(s) it carries are not in what p/x.bot writes (the first: # c)"},
		{"a line between the close and the same text", "dsl: 2\nprompts: {ask: # c\n  }\nz: y # q\nw: v # c\n", "3 comment line(s) it carries are not in what p/x.bot writes (the first: # c)"},
		{"the same text on the closing line", "dsl: 2\nprompts: {ask: # c\n  } # c\n", "2 comment line(s) it carries are not in what p/x.bot writes (the first: # c)"},
		{"a bare hash on the closing line", "dsl: 2\nprompts: {ask: #\n  } #\n", "2 comment line(s) it carries are not in what p/x.bot writes (the first: #)"},
		{"an own-line same text below the close", "dsl: 2\nprompts: {ask: # c\n  }\n# c\nother: y\n", "2 comment line(s) it carries are not in what p/x.bot writes (the first: # c)"},
		{"the same text in a block scalar's body below", "dsl: 2\nprompts: {ask: # c\n  }\ns: |\n  # c\n", "1 comment line(s) it carries are not in what p/x.bot writes (the first: # c)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile("p/x.bot.yaml", []byte(tc.target), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := RunFmt(FmtOptions{Paths: []string{bot}, To: "yaml", Force: true, Printer: jp})
			if err != nil {
				t.Fatalf("--to yaml --force: %v", err)
			}
			if note := carried(res); !strings.Contains(note, tc.want) {
				t.Fatalf("--force counted the comments otherwise: %q, want %q", note, tc.want)
			}
		})
	}
}

// A destination that is there and is not a file — a directory — is refused,
// that file's alone: nothing is written over it, and the files named beside
// it are converted all the same.
func TestFmtToRefusesADestinationThatIsNotAFile(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "r/taken.bot.yaml", authorHelloDoc)
	other := writeBot(t, "r/free.bot.yaml", authorHelloDoc)
	if err := os.MkdirAll("r/taken.bot", 0o755); err != nil {
		t.Fatal(err)
	}
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc, other}, To: "bot", Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "a directory, not a file") {
		t.Fatalf("a directory at the destination was not refused by name: %v %v", err, res.Refused)
	}
	if info, err := os.Stat("r/taken.bot"); err != nil || !info.IsDir() {
		t.Fatal("the directory at the destination was replaced")
	}
	if len(res.Files) != 1 || res.Files[0].Path != "r/free.bot" || !res.Files[0].Written {
		t.Fatalf("the file beside the refused one was not converted: %+v", res.Files)
	}
}

// The .bot twin's name reads back everywhere the .bot is read: the workflow
// file suffix is case-folded (one rule with bundle.Detect, #1762), so
// `UPPER.BOT.YAML` converts to `UPPER.BOT`, and the file fmt writes is one
// fmt, the walks and `--to yaml` all read.
func TestFmtToBotWritesADestinationFmtReadsBack(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "u/UPPER.BOT.YAML", authorHelloDoc)
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if err != nil {
		t.Fatalf("--to bot on UPPER.BOT.YAML: %v, want the conversion to UPPER.BOT", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "u/UPPER.BOT" || !res.Files[0].Written {
		t.Fatalf("the .bot was not written under its own name: %+v", res.Files)
	}
	if _, err := os.Stat("u/UPPER.BOT"); err != nil {
		t.Fatalf("UPPER.BOT not on disk: %v", err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{"u/UPPER.BOT"}, To: "yaml", Printer: jp}); err != nil {
		t.Fatalf("--to yaml refuses the UPPER.BOT fmt wrote: %v", err)
	}
}

// A --to refusal names the file once, as the command was given it: the
// parse carries the absolute path, the message does not repeat it.
func TestFmtToNamesTheFileOnceInARefusal(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "o/bad.bot.yaml", "dsl: 2\nnodes: nope\n")
	jp, _ := jsonPrinter()
	res, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp})
	if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.HasPrefix(res.Refused[0], doc+": does not read: "+doc+":") || strings.Count(res.Refused[0], "bad.bot.yaml") != 2 {
		t.Fatalf("the refusal does not name the document as given, once for the file and once for the position: %v %q", err, res.Refused)
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

// testFrontmatter is bundle.ParseFrontmatter of a text whose block the
// reader reads: a refusal fails the test.
func testFrontmatter(t testing.TB, raw []byte) *bundle.Frontmatter {
	t.Helper()
	fm, err := bundle.ParseFrontmatter("main.bot", raw)
	if err != nil {
		t.Fatalf("the frontmatter is refused: %v", err)
	}
	return fm
}
