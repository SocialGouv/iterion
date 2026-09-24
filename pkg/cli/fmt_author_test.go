package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
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
	if why := ir.SameProgram(ir.Compile(back.File), ir.Compile(parser.Parse(bot, botText).File)); why != "" || !strings.Contains(string(out), "default: 1.5\n") {
		t.Fatalf("the document written is not the .bot's program, its float in the reader's digits (%s):\n%s", why, out)
	}
	// A .bot that compiles to no workflow — a fragment, the same literals —
	// is compared declaration by declaration, each literal by its value; and
	// a parameter a vendor names `<<` is written as the text, not YAML's
	// merge key.
	for path, text := range map[string]string{
		"r/lib/numbers.bot": "dsl: 2\n\nvars:\n  ratio: float = 01.5\n  n: int = 010\n\nagent a:\n  model: \"m\"\n",
		// The writer puts `system:` before `user:`: the inline prompts come
		// back in another order, which says nothing.
		"r/lib/inline.bot": "dsl: 2\n\nagent a:\n  model: \"m\"\n  user: \"the user's text\"\n  system: \"the system's text\"\n",
		"r/merge.bot":      "dsl: 2\n\ntool c:\n  action: forgejo.issue.comment\n  connection: \"forge-main\"\n  params:\n    \"<<\": \"x\"\n\nworkflow w:\n  entry: c\n\n  c -> done\n",
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
		"a key named twice":  {"## ---\n## name: first\n## description: d\n## name: last\n## ---\n", true, "carries no `catalog:`", ""},
		"a scalar triggers":  {"## ---\n## name: probe\n## triggers: just-one\n## ---\n", false, "", "the catalog reader reads"},
		"none of the four":   {"## ---\n## owner: jo\n## ---\n", false, "", "none of the four keys"},
		"the four, and more": {"## ---\n## name: probe\n## owner: jo\n## ---\n", true, "carries no `catalog:`", "1 key(s) of the frontmatter are not carried by `catalog:` (owner)"},
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
			want := bundle.ParseFrontmatter([]byte(tc.head + body))
			got := bundle.ParseFrontmatter([]byte(unparse.Unparse(author.Parse(bot+".yaml", doc).File)))
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

// A document whose .bot name fmt would not read back — `UPPER.BOT.YAML`
// stands for `UPPER.BOT`, and a workflow file's suffix is `.bot`, lower-case
// — is refused before anything is written: a .bot only its launcher reads
// is no twin.
func TestFmtToBotRefusesADestinationFmtWouldNotRead(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "u/UPPER.BOT.YAML", authorHelloDoc)
	jp, _ := jsonPrinter()
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp}); !errors.Is(err, ErrFmtToDestination) || !strings.Contains(err.Error(), "u/UPPER.BOT") {
		t.Fatalf("--to bot on UPPER.BOT.YAML: %v, want ErrFmtToDestination naming the .bot", err)
	}
	if _, err := os.Stat("u/UPPER.BOT"); err == nil {
		t.Fatal("a .bot fmt does not read back was written")
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
