package cli_test

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// writeTheBotBeside writes the .bot an author document stands for, as
// `fmt --to bot` writes it.
func writeTheBotBeside(t *testing.T, doc string) {
	t.Helper()
	p, out := newTestPrinter(cli.OutputJSON)
	if _, err := cli.RunFmt(t.Context(), cli.FmtOptions{Paths: []string{doc}, To: "bot", Printer: p}); err != nil {
		t.Fatalf("fmt --to bot %s: %v\n%s", doc, err, out.String())
	}
}

// diagnosticWithCode is the one finding of res carrying code, or a failure.
func diagnosticWithCode(t *testing.T, res cli.ValidateResult, code, out string) cli.ValidateDiagnostic {
	t.Helper()
	var found []cli.ValidateDiagnostic
	for _, d := range res.Diagnostics {
		if d.Code == code {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d finding(s) with code %s, want one:\n%s", len(found), code, out)
	}
	return found[0]
}

// readsAsLine matches the value of a human output's `Reads as` line — the
// .bot a document stands for — and nothing else of the output.
var readsAsLine = regexp.MustCompile(`(?m)^ *Reads as: +(.+?) *$`)

// readsAsValue is the .bot the human output of validate or diagram says the
// document reads as, or a failure when the line is absent.
func readsAsValue(t *testing.T, out string) string {
	t.Helper()
	m := readsAsLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the output carries no `Reads as` line:\n%s", out)
	}
	return m[1]
}

// runFmtTwin runs one conversion as the operator runs it — the two remedies
// E056's hint names, and the canonical-form check that is not E056 — and
// returns its error and the report it printed.
func runFmtTwin(t *testing.T, opts cli.FmtOptions) (string, error) {
	t.Helper()
	p, out := newTestPrinter(cli.OutputJSON)
	opts.Printer = p
	_, err := cli.RunFmt(t.Context(), opts)
	return out.String(), err
}

// canonicalFormIsRed fails unless `fmt --to bot --check` refuses the pair of
// doc: the gate on the canonical form, red wherever the .bot beside a
// document is not byte-for-byte the text it writes.
func canonicalFormIsRed(t *testing.T, doc string) {
	t.Helper()
	if out, err := runFmtTwin(t, cli.FmtOptions{Paths: []string{doc}, To: "bot", Check: true}); err == nil {
		t.Fatalf("`fmt --to bot --check` is green on this pair, so it holds no byte difference for E056 to be silent about:\n%s", out)
	}
}

// prependTo writes text above the bytes of path: a comment line the file
// carries and the program it describes has no form for.
func prependTo(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, append([]byte(text), mustReadFile(t, path)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustReadFile is path's bytes, or a failure.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// documentAhead is authorHello with a prompt, a node and two edges it does
// not have: the program one side of a pair describes and the other does not.
func documentAhead() string {
	ahead := strings.Replace(authorHello, "  ask: Say hello.", "  ask: Say hello.\n  check: Judge it.", 1)
	ahead = strings.Replace(ahead, "    system: ask\n", "    system: ask\n  - agent: judge\n    model: m\n    system: check\n", 1)
	return strings.Replace(ahead, "    - hello -> done", "    - hello -> judge\n    - judge -> done", 1)
}

// theBotOf is the .bot the document text writes, written by `fmt --to bot` in
// a directory of its own: the real producer, for a fixture that needs a
// canonical .bot of another program than the document beside it.
func theBotOf(t *testing.T, document string) []byte {
	t.Helper()
	dir := t.TempDir()
	writeTheBotBeside(t, writeFixture(t, dir, "hello.bot.yaml", document))
	return mustReadFile(t, filepath.Join(dir, "hello.bot"))
}

// twoAgentsDocument is a document of two agent nodes, the one named first
// written first, importing the `lib/a.bot` fragment or not. The program is
// the same whichever is written first — a node is reached by its id, and the
// edges name both — so two of these are one program told in another order.
func twoAgentsDocument(importing bool, first string) string {
	imports := ""
	if importing {
		imports = "imports:\n  - lib/a.bot\n"
	}
	nodes := map[string]string{
		"hello": "  - agent: hello\n    model: m\n    system: ask\n",
		"judge": "  - agent: judge\n    model: m\n    system: check\n",
	}
	second := "judge"
	if first == "judge" {
		second = "hello"
	}
	return "dsl: 2\n" + imports + "prompts:\n  ask: Say hello.\n  check: Judge it.\nnodes:\n" +
		nodes[first] + nodes[second] +
		"workflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> judge\n    - judge -> done\n"
}

// twoSupervisorsDocument is a document of two supervisors watching one
// agent, the one named first written first, importing the `lib/a.bot`
// fragment or not. Unlike two nodes, two of these are two PROGRAMS: a
// supervisor is no graph node reached by its id, and the compiled form
// holds them in the order they are declared (ir.Workflow.Supervisors).
func twoSupervisorsDocument(importing bool, first string) string {
	imports := ""
	if importing {
		imports = "imports:\n  - lib/a.bot\n"
	}
	watcher := map[string]string{
		"alpha": "  alpha:\n    watches: [hello]\n    model: m\n    system: pol_a\n",
		"beta":  "  beta:\n    watches: [hello]\n    model: m\n    system: pol_b\n",
	}
	second := "beta"
	if first == "beta" {
		second = "alpha"
	}
	return "dsl: 2\n" + imports + "prompts:\n  ask: Say hello.\n  pol_a: Policy A.\n  pol_b: Policy B.\nsupervisors:\n" +
		watcher[first] + watcher[second] +
		"nodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
}

// twoUsesBot is a .bot using one group twice, the use named first written
// first. The group holds an INTERNAL edge, so each use expands into one
// edge of the compiled workflow and the two uses are ranked by the program
// itself (ir.Workflow.Edges).
func twoUsesBot(first, second string) string {
	use := map[string]string{
		"one": "use pair as r1 with {\n  label: \"one\",\n}\n\n",
		"two": "use pair as r2 with {\n  label: \"two\",\n}\n\n",
	}
	return "dsl: 2\n\nschema out:\n  ok: bool\n\ngroup pair(label):\n" +
		"  tool first:\n    command: \"printf '{\\\"ok\\\":true}' # {{params.label}}\"\n    output: out\n" +
		"  tool second:\n    command: \"printf '{\\\"ok\\\":true}'\"\n    output: out\n" +
		"  first -> second\n\n" +
		use[first] + use[second] +
		"workflow w:\n  entry: r1.first\n  r1.second -> r2.first\n  r2.second -> done\n"
}

// documentWithItsBot writes a document in a directory of its own, beside the
// `lib/a.bot` fragment an import of it names, and the .bot `fmt --to bot`
// writes from it: the pair an author holds after one conversion.
func documentWithItsBot(t *testing.T, text string) (doc, bot string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, filepath.Join("lib", "a.bot"), "prompt from_a:\n  A.\n")
	doc = writeFixture(t, dir, "hello.bot.yaml", text)
	writeTheBotBeside(t, doc)
	return doc, filepath.Join(dir, "hello.bot")
}

// theProgramALauncherRuns is the program a launcher opens for a .bot: its
// unit loaded off the disk, merged and compiled, as the engine does — the
// program E056 speaks of, read where no document is in the way.
func theProgramALauncherRuns(t *testing.T, bot string) *ir.CompileResult {
	t.Helper()
	return ir.Compile(unittest.Whole(unit.LoadDir(bot)).Merged)
}

// botTheCompilerRefuses is a .bot the parser accepts and the compiler
// refuses: an edge naming a node nobody declares. No launcher runs it.
const botTheCompilerRefuses = "dsl: 2\n\nworkflow hello:\n  entry: hello\n  hello -> ghost\n"

// lineOfIn is the 1-based line of the first occurrence of what in text.
func lineOfIn(t *testing.T, text, what string) int {
	t.Helper()
	i := strings.Index(text, what)
	if i < 0 {
		t.Fatalf("the fixture holds no %q", what)
	}
	return 1 + strings.Count(text[:i], "\n")
}

// brokenBot is a .bot the parser only recovers on — a comment line and an
// unterminated string literal: a text that is no program at all.
const brokenBot = "## keep me\ndsl: 2\n\nprompt ask:\n  Say hello.\n\nagent hello:\n  model: \"m\n  system: ask\n"

// noWrittenFormDocument describes a program with no written .bot form: a
// profile-1 value that needs the strict-escape directive, under a catalog
// long enough to push that directive out of the lexer's window.
func noWrittenFormDocument() string {
	desc := strings.Repeat("    a line of the description\n", 40)
	return "dsl: 1\ncatalog:\n  name: probe\n  description: |\n" + desc +
		"nodes:\n  - tool: t\n    command: \"printf `x` \\\"y\\\"\"\n    output: out\nschemas:\n  out:\n    ok: bool\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n"
}

// The .bot beside an author document is judged as a PROGRAM (E056): one
// that describes another program than the document — the difference named —
// and one that is no program at all — a text the parser only recovers on, a
// text the compiler refuses on its own account — are each said as a warning
// positioned on the document, in `--json` and in the human output, and the
// verdict stands: which of the two files is current the author alone knows.
// Both sides are read as a launcher reads them — the fragments their
// `import` lines name merged in, the prompts of the bundle they share
// merged into each — so neither an import nor a bundle prompt is a
// difference of its own. A comment, a blank line, an indentation and a
// spelling are not the question either: the pair they differ by is one
// program, and the byte difference is what `fmt --to bot --check` reads.
// Nothing is said where `fmt --to bot` would write no .bot at all — a
// document that does not read, or whose program has no written form —
// where no .bot is written yet, or where the program question has no
// subject: a side that describes no program and carries no refusal of its
// own is a fragment, and a DOCUMENT its own load or compile refuses leaves
// nothing to hold the .bot to.
func TestValidateSaysWhenTheBotBesideADocumentIsAnotherProgram(t *testing.T) {
	t.Run("no .bot beside the document", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("validate refused a valid document: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 where no .bot is written beside the document:\n%s", out)
		}
	})

	t.Run("the .bot the document writes", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		writeTheBotBeside(t, doc)
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("validate refused a valid document: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 beside the very .bot the document writes:\n%s", out)
		}
	})

	// A bundle's prompts are part of the program a launcher runs — a node's
	// `system:` names one — and the bundle is the one object the two files
	// share, so the comparison merges it into BOTH sides. Without that
	// merge each node reading a `prompts/*.md` raises its own C003 on each
	// side, and a document holding one node more differs from the .bot by
	// the COUNT of a code neither file is refused for: the oracle's answer
	// is then a list of compiler codes, and the node is never named.
	t.Run("a .bot whose prompt its bundle supplies", func(t *testing.T) {
		dir := t.TempDir()
		writeFixture(t, dir, "manifest.yaml", "schema_version: 1\nname: probe\n")
		if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(dir, "prompts"), "ask.md", "Say hello.\n")
		writeFixture(t, filepath.Join(dir, "prompts"), "check.md", "Judge it.\n")
		const oneNodeReadingTheBundle = "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
		doc := writeFixture(t, dir, "main.bot.yaml", oneNodeReadingTheBundle)
		writeTheBotBeside(t, doc)
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("the bundle's prompt was not in scope for the document: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 on the very .bot this document writes, its prompt supplied by the bundle:\n%s", out)
		}
		// The author adds a node reading another of the bundle's prompts and
		// has not rewritten the .bot: the node is what is named.
		ahead := strings.Replace(string(mustReadFile(t, doc)), "    system: ask\n", "    system: ask\n  - agent: judge\n    model: m\n    system: check\n", 1)
		ahead = strings.Replace(ahead, "    - hello -> done", "    - hello -> judge\n    - judge -> done", 1)
		if err := os.WriteFile(doc, []byte(ahead), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, err = validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, `node "judge" is missing`) {
			t.Fatalf("E056 does not name the node the .bot has not, on a pair whose prompts the bundle supplies: %+v", d)
		}
		// Never a count of a code the bundle merge accounts for: no command
		// the author can run reports a C003 on either of these two files.
		if strings.Contains(d.Message, "C003") {
			t.Fatalf("E056 reads the bundle's prompts as missing from one side: %+v", d)
		}
		// And the merge is what gives the document a program at all here.
		// Without it the document — whose prompts the bundle alone supplies
		// — is refused for every one of them while the .bot declaring them
		// in place is not, which reads as a refusal of the document's own
		// and silences E056 over a real difference.
		if err := os.WriteFile(doc, []byte(oneNodeReadingTheBundle), 0o644); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, dir, "main.bot", "dsl: 2\n\nprompt ask:\n  Another body entirely.\n\nagent hello:\n  model: m\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n")
		res, out, err = validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d = diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "prompts differ") {
			t.Fatalf("E056 does not name the prompt bodies the two sides differ by, one from the bundle and one in place: %+v", d)
		}
	})

	// The prompts of that bundle are READ once, for both sides. A prompt
	// file whose bytes differ between two reads is one program all the
	// same: the bundle is one object, and an author editing a `prompts/*.md`
	// while the verdict is taken has changed neither of the two files — so a
	// read per side would make E056 fire on the pair `fmt --to yaml` itself
	// wrote. The kernel's uuid file is that fact deterministically — a
	// regular file answered afresh on every read — where the shape the
	// defect takes in a hand (an editor saving, the studio validating live)
	// needs a writer racing the verdict.
	t.Run("a bundle prompt whose bytes differ on every read", func(t *testing.T) {
		const freshEveryRead = "/proc/sys/kernel/random/uuid"
		first, err := os.ReadFile(freshEveryRead)
		if err != nil {
			t.Skipf("this machine has no file whose bytes differ on every read: %s: %v", freshEveryRead, err)
		}
		if second, err := os.ReadFile(freshEveryRead); err != nil || string(second) == string(first) {
			t.Skipf("%s answers the same bytes twice (err %v): the witness needs a file that does not", freshEveryRead, err)
		}
		dir := t.TempDir()
		writeFixture(t, dir, "manifest.yaml", "schema_version: 1\nname: probe\n")
		if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(dir, "prompts"), "ask.md", "Say hello.\n")
		// A link: nothing is written to the target, and the temp directory's
		// cleanup unlinks the link itself.
		if err := os.Symlink(freshEveryRead, filepath.Join(dir, "prompts", "fresh.md")); err != nil {
			t.Skipf("cannot link a bundle prompt to %s: %v", freshEveryRead, err)
		}
		doc := writeFixture(t, dir, "main.bot.yaml", "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n")
		writeTheBotBeside(t, doc)
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("validate refused the pair fmt wrote: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 on the very .bot this document writes, over a bundle prompt read once per side:\n%s", out)
		}
	})

	// A DOCUMENT the load or the compile refuses on its own account
	// describes no program to hold the .bot to, and the verdict carries
	// that refusal already, at its position. The .bot is the sound side
	// here — the only copy of the program — so a second, cryptic sentence
	// would both count one cause twice and send the author to the two
	// `--force` conversions of the hint, each of which replaces its
	// destination whole.
	t.Run("a document the compiler refuses", func(t *testing.T) {
		for _, tc := range []struct{ name, document, code string }{
			{"an unknown prompt named by a node", strings.Replace(authorHello, "system: ask", "system: nosuch", 1), "C003"},
			{"an edge naming a node it does not declare", strings.Replace(authorHello, "    - hello -> done", "    - hello -> ghost\n    - ghost -> done", 1), "C001"},
			{"a fragment it alone imports", strings.Replace(authorHello, "dsl: 2\n", "dsl: 2\nimports:\n  - lib/missing.bot\n", 1), "E046"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				doc := writeFixture(t, dir, "hello.bot.yaml", tc.document)
				// The .bot of the healthy program, written by `fmt --to bot`
				// before the document was broken: the sound side.
				if err := os.WriteFile(filepath.Join(dir, "hello.bot"), theBotOf(t, authorHello), 0o644); err != nil {
					t.Fatal(err)
				}
				res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
				// The premise: the refusal is the document's own, and the
				// verdict says so.
				if res.Valid || !hasDiagCode(res, tc.code) {
					t.Fatalf("the fixture's premise is broken — %s is not what refuses this document:\n%s", tc.code, out)
				}
				if hasDiagCode(res, "E056") {
					t.Fatalf("E056 beside a document the compiler refuses on its own account:\n%s", out)
				}
			})
		}
	})

	// Where both sides compiled a workflow, the construct those workflows
	// differ by is what the author can go and read. The sorted diagnostic
	// codes the oracle compares FIRST are its answer to a transport that
	// carries no workflow, and name nothing editable: here each side is
	// refused for the same code — so neither carries a refusal of its own —
	// in a different NUMBER.
	t.Run("a .bot refused for the same code in another number", func(t *testing.T) {
		const oneNode = "dsl: 2\nnodes:\n  - agent: one\n    model: m\n    system: nowhere\nworkflow:\n  name: w\n  entry: one\n  edges:\n    - one -> done\n"
		dir := t.TempDir()
		doc := writeFixture(t, dir, "m.bot.yaml", oneNode)
		writeTheBotBeside(t, doc)
		twoNodes := strings.Replace(oneNode, "    system: nowhere\n", "    system: nowhere\n  - agent: two\n    model: m\n    system: nowhere\n", 1)
		if err := os.WriteFile(doc, []byte(strings.Replace(twoNodes, "    - one -> done", "    - one -> two\n    - two -> done", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, `node "two" is missing`) {
			t.Fatalf("E056 does not name the node the two workflows differ by: %+v", d)
		}
		if strings.Contains(d.Message, "diagnostics differ") {
			t.Fatalf("E056 answers with the oracle's list of compiler codes: %+v", d)
		}
	})

	// A refusal cascades — one unterminated literal refuses every
	// declaration after it — so the message names the FIRST position and
	// counts the rest: a `validate` of the .bot itself prints them all.
	// The author who copies the document over the .bot (a merge conflict,
	// an editor saving the YAML into it) is the shape that measures it.
	t.Run("a .bot the parser refuses many times over", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		bot := filepath.Join(dir, "hello.bot")
		// The document's own text over the .bot: YAML the .bot parser
		// refuses line after line.
		if err := os.WriteFile(bot, mustReadFile(t, doc), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		d := diagnosticWithCode(t, res, "E056", out)
		if strings.Count(d.Message, "; ") > 0 {
			t.Fatalf("E056 joins every refusal of the .bot into one sentence: %+v", d)
		}
		if !regexp.MustCompile(`\(and \d+ more\)$`).MatchString(d.Message) {
			t.Fatalf("E056 does not count the refusals it left out: %+v", d)
		}
		// The position the author goes to, named as the subject of the
		// sentence is named — the .bot as the document named it, never a
		// second form of the same name.
		if !strings.Contains(d.Message, bot+":") {
			t.Fatalf("E056 names the .bot's own position in another form than its subject: %+v\nwant a position under %s", d, bot)
		}
	})

	// And the same for a FRAGMENT pair, whose unit root is the parent of
	// lib/: the position is in the .bot itself, so it is named as the
	// subject is.
	t.Run("a fragment .bot named one way in the sentence", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		doc := writeFixture(t, dir, filepath.Join("lib", "frag.bot.yaml"), "dsl: 2\nprompts:\n  ask: Say hello.\n")
		bot := writeFixture(t, dir, filepath.Join("lib", "frag.bot"), "dsl: 2\n\nimport \"lib/missing.bot\"\n\nprompt ask:\n  Say hello.\n")
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.HasPrefix(d.Message, bot+" is ") || !strings.Contains(d.Message, bot+":3:") {
			t.Fatalf("E056's subject and its position name the .bot in two forms: %+v\nwant both as %s", d, bot)
		}
	})

	// The pair `fmt --to yaml` leaves on every commented .bot: the document
	// it writes carries no form for a comment, so the two texts differ for
	// ever while the program is one.
	t.Run("a .bot that is the same program and carries a comment", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		writeTheBotBeside(t, doc)
		bot := filepath.Join(dir, "hello.bot")
		prependTo(t, bot, "## a line the document carries no form for\n")
		if fout, err := runFmtTwin(t, cli.FmtOptions{Paths: []string{bot}, To: "yaml", Force: true}); err != nil {
			t.Fatalf("fmt --to yaml --force %s: %v\n%s", bot, err, fout)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("validate refused the document fmt --to yaml wrote: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 on a pair whose only difference is a comment line:\n%s", out)
		}
		// And the byte difference is there all the same: it is the canonical
		// form's gate that reads it, not this warning.
		canonicalFormIsRed(t, doc)
	})

	// Each side carrying a comment of its own — the state the adoption path
	// leaves: still one program.
	t.Run("a .bot and a document each carrying a comment of its own", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		writeTheBotBeside(t, doc)
		prependTo(t, filepath.Join(dir, "hello.bot"), "## a comment the .bot keeps\n")
		prependTo(t, doc, "# a note the author keeps in the document\n")
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("validate refused a commented document: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 on a pair where each file carries a comment of its own:\n%s", out)
		}
		canonicalFormIsRed(t, doc)
	})

	t.Run("a .bot that is the same program spelled otherwise", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		writeTheBotBeside(t, doc)
		bot := filepath.Join(dir, "hello.bot")
		// Two blank lines at the end and every indentation widened: a text
		// the parser reads as the very program the document describes.
		reindented := strings.ReplaceAll(string(mustReadFile(t, bot)), "  ", "   ") + "\n\n"
		if err := os.WriteFile(bot, []byte(reindented), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("validate refused a valid document: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 on a .bot that differs by its spelling alone:\n%s", out)
		}
		canonicalFormIsRed(t, doc)
	})

	// A pair of FRAGMENTS — a lib/ file and the document beside it, neither
	// compiling to a workflow once its imports are merged: E056 says
	// nothing of what they declare, whatever it is. The program question
	// has no subject there, and the question about their texts is
	// `fmt --to bot --check`'s, which reddens on each pair that differs.
	t.Run("a pair of fragments", func(t *testing.T) {
		const twoPrompts = "dsl: 2\nprompts:\n  ask: Say hello.\n  check: Judge it.\n"
		for _, tc := range []struct {
			name, bot   string
			textDiffers bool
		}{
			{name: "the .bot the document writes", bot: twoPrompts},
			{name: "two prompts declared in the other order", bot: "dsl: 2\nprompts:\n  check: Judge it.\n  ask: Say hello.\n", textDiffers: true},
			{name: "a prompt renamed", bot: "dsl: 2\nprompts:\n  ask: Say hello.\n  judge: Judge it.\n", textDiffers: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
					t.Fatal(err)
				}
				doc := writeFixture(t, dir, filepath.Join("lib", "frag.bot.yaml"), twoPrompts)
				if err := os.WriteFile(filepath.Join(dir, "lib", "frag.bot"), theBotOf(t, tc.bot), 0o644); err != nil {
					t.Fatal(err)
				}
				res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
				if hasDiagCode(res, "E056") {
					t.Fatalf("E056 about a pair of fragments, which describe no program at all:\n%s", out)
				}
				if tc.textDiffers {
					canonicalFormIsRed(t, doc)
				}
			})
		}
	})

	// The program of a .bot is not its own declarations alone: it imports
	// fragments, and the program compared is the one the unit loader merges
	// — the fragments opened and read, as a launcher reads them. Two files
	// reading another fragment are two programs when the fragments declare
	// another program, and the difference is named as what it is.
	t.Run("a .bot importing another fragment", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, dir, filepath.Join("lib", "a.bot"), "prompt from_a:\n  A.\n")
		writeFixture(t, dir, filepath.Join("lib", "b.bot"), "prompt from_b:\n  B.\n")
		doc := writeFixture(t, dir, "hello.bot.yaml", strings.Replace(authorHello, "dsl: 2\n", "dsl: 2\nimports:\n  - lib/a.bot\n", 1))
		writeTheBotBeside(t, doc)
		bot := filepath.Join(dir, "hello.bot")
		// The control: the .bot that document writes, imports and all, is
		// silent — so what the edit below reddens is the import, not the
		// presence of an import.
		if res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{}); hasDiagCode(res, "E056") {
			t.Fatalf("E056 beside the very .bot an importing document writes:\n%s", out)
		}
		imported := strings.Replace(string(mustReadFile(t, bot)), "lib/a.bot", "lib/b.bot", 1)
		if !strings.Contains(imported, "lib/b.bot") {
			t.Fatalf("the fixture .bot carries no import line:\n%s", imported)
		}
		if err := os.WriteFile(bot, []byte(imported), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		d := diagnosticWithCode(t, res, "E056", out)
		// The fragments are opened: each declares a prompt of its own, so
		// the two programs differ by their prompts and the author is sent to
		// the construct, not to a list of file names.
		if !strings.Contains(d.Message, "describes another program") || !strings.Contains(d.Message, "prompts differ") {
			t.Fatalf("E056 does not name what the two merged programs differ by: %+v", d)
		}
		// Never the compiler's word for a file read alone, whose own text
		// sends the reader to `iterion validate`: the merge happens first,
		// so no side can carry it.
		if strings.Contains(d.Message, "[C030]") || strings.Contains(d.Message, "compile it as a unit") {
			t.Fatalf("E056 gives a file read alone as its reason: %+v", d)
		}
	})

	// A fragment NEITHER file can read is neither's fault: both units are
	// refused for it — the document's own refusal is the verdict's, said
	// once — and the two describe one program all the same, so nothing is
	// said of the .bot.
	t.Run("a fragment neither file can read", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", strings.Replace(authorHello, "dsl: 2\n", "dsl: 2\nimports:\n  - lib/missing.bot\n", 1))
		writeTheBotBeside(t, doc)
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		// The premise: the import is refused, on the document's own
		// account, by the verdict itself.
		if res.Valid {
			t.Fatalf("a document importing a fragment that is not there validated OK:\n%s", out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 reads a fragment neither file can read as this .bot's own refusal:\n%s", out)
		}
	})

	// The same fragment missing for the .bot ALONE: there the load's
	// refusal is its own, and E056 says so — naming the fragment, never
	// the compiler's word for a file read alone (C030), which the merge
	// makes unreachable.
	t.Run("a fragment the .bot alone cannot read", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		writeTheBotBeside(t, doc)
		bot := filepath.Join(dir, "hello.bot")
		canonical := string(mustReadFile(t, bot))
		withImport := strings.Replace(canonical, "dsl: 2\n", "dsl: 2\n\nimport \"lib/missing.bot\"\n", 1)
		if withImport == canonical {
			t.Fatalf("the .bot `fmt --to bot` wrote carries no `dsl: 2` line:\n%s", canonical)
		}
		if err := os.WriteFile(bot, []byte(withImport), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "is no program this document could describe") || !strings.Contains(d.Message, "lib/missing.bot") {
			t.Fatalf("E056 does not read a .bot whose fragment is not there as no program, or does not name it: %+v", d)
		}
		if strings.Contains(d.Message, "[C030]") || strings.Contains(d.Message, "compile it as a unit") {
			t.Fatalf("E056 gives the compiler's word for a file read alone as its reason: %+v", d)
		}
	})

	// An `import` line is no difference of its own: the fragment is merged
	// before either side is compiled, so a .bot importing a fragment that
	// declares nothing is the document's own program — the premise asserted
	// below — and nothing is said of it. Where the fragment declares
	// something, the merged programs differ and that difference is what is
	// named (the subtest above).
	t.Run("a .bot importing a fragment that declares nothing", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		// A fragment declaring nothing: the merge adds no declaration, so
		// the program a launcher runs from the importing .bot is the
		// document's exactly — the premise asserted below.
		writeFixture(t, dir, filepath.Join("lib", "empty.bot"), "dsl: 2\n")
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		writeTheBotBeside(t, doc)
		bot := filepath.Join(dir, "hello.bot")
		canonical := string(mustReadFile(t, bot))
		withImport := strings.Replace(canonical, "dsl: 2\n", "dsl: 2\n\nimport \"lib/empty.bot\"\n", 1)
		if withImport == canonical {
			t.Fatalf("the .bot `fmt --to bot` wrote carries no `dsl: 2` line:\n%s", canonical)
		}
		if err := os.WriteFile(bot, []byte(withImport), 0o644); err != nil {
			t.Fatal(err)
		}
		asTheDocumentWritesIt := writeFixture(t, t.TempDir(), "hello.bot", canonical)
		if why := ir.SameProgram(theProgramALauncherRuns(t, bot), theProgramALauncherRuns(t, asTheDocumentWritesIt)); why != "" {
			t.Fatalf("the fixture's premise is broken — a launcher runs two programs from the two .bots: %s", why)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 on a pair its own oracle reads as one program:\n%s", out)
		}
		// The two texts differ all the same — the `import` line is in one
		// and not the other: that pair is the canonical form's gate.
		canonicalFormIsRed(t, doc)
	})

	// The same shape an author reaches by moving a prompt into lib/: the
	// document imports the fragment that declares it, the .bot declares it
	// itself. One merged program, the premise asserted below, so nothing is
	// said — where a comparison of the two files' own declarations would
	// read the prompt as one the .bot alone carries.
	t.Run("a .bot declaring what the document imports", func(t *testing.T) {
		const promptText = "prompt ask:\n  Say hello.\n"
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, dir, filepath.Join("lib", "ask.bot"), promptText)
		// The document's own prompts moved into the fragment it imports.
		imported := strings.Replace(authorHello, "prompts:\n  ask: Say hello.\n", "", 1)
		doc := writeFixture(t, dir, "hello.bot.yaml", strings.Replace(imported, "dsl: 2\n", "dsl: 2\nimports:\n  - lib/ask.bot\n", 1))
		writeTheBotBeside(t, doc)
		bot := filepath.Join(dir, "hello.bot")
		importing := theProgramALauncherRuns(t, bot)
		// The .bot of the document that declares the prompt in place: the
		// same program, written without the import.
		if err := os.WriteFile(bot, theBotOf(t, authorHello), 0o644); err != nil {
			t.Fatal(err)
		}
		if why := ir.SameProgram(importing, theProgramALauncherRuns(t, bot)); why != "" {
			t.Fatalf("the fixture's premise is broken — a launcher runs two programs from the two .bots: %s", why)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 on a .bot declaring in place what the document imports — one program for a launcher:\n%s", out)
		}
		canonicalFormIsRed(t, doc)
	})

	// The `import` lines are said as themselves only where they are the
	// WHOLE difference: a .bot that also declares another program is one,
	// and naming the fragments there would leave the author reading lib/
	// for a difference that is in the file under his eyes.
	t.Run("a .bot importing and declaring another program", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, dir, filepath.Join("lib", "empty.bot"), "dsl: 2\n")
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		// The .bot of a program this document does not describe, importing
		// where the document does not: two differences at once.
		ahead := strings.Replace(string(theBotOf(t, documentAhead())), "dsl: 2\n", "dsl: 2\n\nimport \"lib/empty.bot\"\n", 1)
		if !strings.Contains(ahead, "lib/empty.bot") {
			t.Fatalf("the fixture .bot carries no import line:\n%s", ahead)
		}
		if err := os.WriteFile(filepath.Join(dir, "hello.bot"), []byte(ahead), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "describes another program") {
			t.Fatalf("E056 reads a .bot of another program as an import difference alone: %+v", d)
		}
	})

	// The rank of two declarations is no part of the program — except where
	// the compiled program reads it. Two supervisors written in the other
	// order are two programs for the oracle every conversion is proven with,
	// and the pair below is importing: the shape a single-file compile sees
	// no program in at all, where the unit loader is what gives the oracle
	// one, and erasing that rank would make E056 silent on a difference the
	// compiled oracle names.
	t.Run("a .bot whose supervisors are declared in the other order", func(t *testing.T) {
		doc, bot := documentWithItsBot(t, twoSupervisorsDocument(true, "alpha"))
		_, otherOrder := documentWithItsBot(t, twoSupervisorsDocument(true, "beta"))
		// The premise: a launcher runs two programs from the two .bots.
		if why := ir.SameProgram(theProgramALauncherRuns(t, bot), theProgramALauncherRuns(t, otherOrder)); why == "" {
			t.Fatal("the fixture's premise is broken — a launcher runs one program from the two .bots")
		}
		// The control: the .bot this document writes is silent, so what the
		// swap reddens is the order, not the pair.
		if res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{}); hasDiagCode(res, "E056") {
			t.Fatalf("E056 beside the very .bot this document writes:\n%s", out)
		}
		if err := os.WriteFile(bot, mustReadFile(t, otherOrder), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		// Named as the compiler names the field it reads them in.
		if !strings.Contains(d.Message, "describes another program") || !strings.Contains(d.Message, "Supervisors") {
			t.Fatalf("E056 reads two supervisors written in the other order as one program, or does not name them: %+v", d)
		}
	})

	// Another rank the compiled program reads, through a declaration that
	// expands: a `use` of a group holding an INTERNAL edge lands in
	// ir.Workflow.Edges, which is ordered. No doctrine site lists the ranks
	// the program reads — the oracle reads them for us — and this is the
	// witness for the one a group expansion carries, the swap reached
	// through the two `use` declarations alone.
	t.Run("a .bot whose group uses are declared in the other order", func(t *testing.T) {
		dir := t.TempDir()
		bot := writeFixture(t, dir, "pairs.bot", twoUsesBot("one", "two"))
		if out, err := runFmtTwin(t, cli.FmtOptions{Paths: []string{bot}, To: "yaml"}); err != nil {
			t.Fatalf("fmt --to yaml refused the fixture: %v\n%s", err, out)
		}
		doc := bot + ".yaml"
		// The control: the document written FROM this .bot is silent beside
		// it, so what the swap reddens is the rank and not the pair.
		if res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{}); hasDiagCode(res, "E056") {
			t.Fatalf("E056 beside the very .bot this document was written from:\n%s", out)
		}
		if err := os.WriteFile(bot, []byte(twoUsesBot("two", "one")), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "edge r1.first -> r1.second differs") {
			t.Fatalf("E056 reads two group uses written in the other order as one program, or does not name the edge they differ by: %+v", d)
		}
	})

	t.Run("a .bot whose program the document does not describe", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		// The .bot of another program: a node and two edges this document
		// does not describe, written by `fmt --to bot` from that program.
		if err := os.WriteFile(filepath.Join(dir, "hello.bot"), theBotOf(t, documentAhead()), 0o644); err != nil {
			t.Fatal(err)
		}
		// Given by its relative name: the warning names the .bot as the
		// document was named, the form `file` and `bot_path` carry.
		t.Chdir(dir)
		res, out, err := validateDocumentJSON(t, "hello.bot.yaml", cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if d.Severity != "warning" || d.Source != "parse" || d.File != doc || d.Line != 1 || d.Hint == "" {
			t.Fatalf("E056 without its severity, source, position or hint: %+v\n%s", d, out)
		}
		if !strings.HasPrefix(d.Message, "hello.bot is ") {
			t.Fatalf("E056 does not open on the .bot named as the document was named: %+v", d)
		}
		if strings.Contains(d.Message, dir) {
			t.Fatalf("E056 names the .bot by its absolute path: %+v", d)
		}
		// It says what it judged — a program — and names the difference it
		// found, in the frame the sentence sets: the subject is that .bot,
		// so a node it runs and this document does not has APPEARED.
		if !strings.Contains(d.Message, "describes another program") || !strings.Contains(d.Message, `node "judge" appeared`) {
			t.Fatalf("E056 does not name the program difference it found, read against this document: %+v", d)
		}
		// The human output carries it too, beside an OK verdict.
		hp, hout := newTestPrinter(cli.OutputHuman)
		if err := cli.RunValidateWith(doc, hp, cli.ValidateOptions{}); err != nil {
			t.Fatalf("validate, human form: %v\n%s", err, hout.String())
		}
		if !strings.Contains(hout.String(), "E056") || !strings.Contains(hout.String(), "result: OK") {
			t.Fatalf("the human output does not carry the warning beside an OK verdict:\n%s", hout.String())
		}
		// The diagram of the same document draws, and says nothing of it:
		// E056 is validate's word, and a diagram carries no diagnostic.
		dp, dout := newTestPrinter(cli.OutputJSON)
		if err := cli.RunDiagram(cli.DiagramOptions{File: doc}, dp); err != nil {
			t.Fatalf("the diagram of a document beside a differing .bot: %v\n%s", err, dout.String())
		}
		if strings.Contains(dout.String(), "E056") {
			t.Fatalf("the diagram carries a diagnostic:\n%s", dout.String())
		}
	})

	// A .bot that is no program at all — a text the parser only recovers on,
	// hand-edited, merge-mangled or truncated.
	t.Run("a .bot that is no program at all", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		bot := writeFixture(t, dir, "hello.bot", brokenBot)
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if d.Severity != "warning" {
			t.Fatalf("E056 beside a .bot that is no program is not a warning: %+v", d)
		}
		if !strings.Contains(d.Message, "is no program this document could describe") {
			t.Fatalf("E056 reads a text the parser only recovers on as a program: %+v", d)
		}
		// With the cause at its position in that .bot, named as the document
		// named it — never as the document's own finding.
		if !strings.Contains(d.Message, "hello.bot:") || !strings.Contains(d.Message, "error [") {
			t.Fatalf("E056 does not name what the parser refused in that .bot: %+v", d)
		}
		if !strings.Contains(d.Message, filepath.Base(bot)) {
			t.Fatalf("E056 does not name the .bot it read: %+v", d)
		}
		// One cause, once: what the compiler says of a text the parser only
		// recovered on — a file that declares no workflow (C006) because it
		// did not parse — counts that cause twice.
		if strings.Contains(d.Message, "[C006]") {
			t.Fatalf("E056 counts the parse error twice, as the compiler's word too: %+v", d)
		}
	})

	// A .bot the parser accepts and the COMPILER refuses on its own
	// account: no program either — nothing a launcher runs, and no program
	// this document could describe.
	t.Run("a .bot the compiler refuses", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		writeFixture(t, dir, "hello.bot", botTheCompilerRefuses)
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "is no program this document could describe") {
			t.Fatalf("E056 reads a text no launcher could run as another program: %+v", d)
		}
		// With the compiler's own refusal, at its position in that .bot,
		// named as the document named it.
		edge := fmt.Sprintf("hello.bot:%d:", lineOfIn(t, botTheCompilerRefuses, "hello -> ghost"))
		if !strings.Contains(d.Message, "[C001]") || !strings.Contains(d.Message, edge) {
			t.Fatalf("E056 does not name what the compiler refused in that .bot, at its line: %+v", d)
		}
	})

	// A second workflow is such a refusal too: a unit has one workflow, so
	// the load refuses the .bot and names both declarations (E010) — a
	// program no document describes, and the compiler's word for the same
	// cause (C007) is not counted a second time.
	t.Run("a .bot declaring a second workflow", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		writeTheBotBeside(t, doc)
		bot := filepath.Join(dir, "hello.bot")
		twoWorkflows := string(mustReadFile(t, bot)) + "\nworkflow second:\n\n  entry: hello\n\n  hello -> done\n"
		if err := os.WriteFile(bot, []byte(twoWorkflows), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "is no program this document could describe") || !strings.Contains(d.Message, "[E010]") {
			t.Fatalf("E056 does not read a .bot of two workflows as no program, by the code the load refused it with: %+v", d)
		}
		if strings.Contains(d.Message, "[C007]") {
			t.Fatalf("E056 counts one cause twice, as the compiler's word too: %+v", d)
		}
	})

	// The same beside an IMPORTING document: that document's own compile
	// refuses the file it reads alone (C030, the imports a launcher
	// resolves), so what the .bot is refused for is what it carries and the
	// document does not.
	t.Run("a .bot that is no program beside an importing document", func(t *testing.T) {
		doc, bot := documentWithItsBot(t, twoAgentsDocument(true, "hello"))
		if err := os.WriteFile(bot, []byte("dsl: 2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "is no program this document could describe") || !strings.Contains(d.Message, "[C006]") {
			t.Fatalf("E056 does not read a .bot with no workflow as no program: %+v", d)
		}
		// Not the import the document's own file-level compile refuses: a
		// code both sides carry is no refusal of this .bot.
		if strings.Contains(d.Message, "[C030]") {
			t.Fatalf("E056 names what reading the document's own file alone refuses: %+v", d)
		}
	})

	// A side that compiles to NO workflow once its fragments are merged is
	// a FRAGMENT — a lib/ file, a file of prompts or schemas alone — and
	// E056 says nothing of what it declares: the program question has no
	// subject. Here the document is the fragment and the .bot beside it a
	// whole program; the author is told the document carries no workflow,
	// once, by the verdict itself.
	t.Run("a document that compiles to no workflow", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", "dsl: 2\nprompts:\n  ask: Say hello.\n")
		if err := os.WriteFile(filepath.Join(dir, "hello.bot"), theBotOf(t, authorHello), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if res.Valid {
			t.Fatalf("a document carrying no workflow validated OK:\n%s", out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 about a document that describes no program at all:\n%s", out)
		}
	})

	t.Run("a document that does not read", func(t *testing.T) {
		dir := t.TempDir()
		// An anchor: the YAML document itself is refused (E050), so the
		// program the document describes is not known.
		doc := writeFixture(t, dir, "hello.bot.yaml", "dsl: 2\nprompts: &p\n  ask: Say hello.\n")
		writeFixture(t, dir, "hello.bot", "dsl: 2\n")
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if res.Valid {
			t.Fatalf("a document holding an anchor validated OK:\n%s", out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 on a document that does not read — `fmt --to bot` writes nothing from one:\n%s", out)
		}
	})

	t.Run("a document whose program has no written .bot form", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "long.bot.yaml", noWrittenFormDocument())
		writeFixture(t, dir, "long.bot", "dsl: 1\n")
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if !hasDiagCode(res, "E054") {
			t.Fatalf("the document whose program has no written .bot form drew no E054:\n%s", out)
		}
		if hasDiagCode(res, "E056") {
			t.Fatalf("E056 beside an E054 — `fmt --to bot` writes nothing from such a document:\n%s", out)
		}
	})

	// A document whose suffix is spelled otherwise — the case-folded form
	// `fmt`, the walks and `--to yaml` all read (#1762).
	t.Run("a document whose suffix is spelled otherwise", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "UPPER.BOT.YAML", authorHello)
		writeFixture(t, dir, "UPPER.BOT", "dsl: 2\n")
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("validate refused a document whose suffix is spelled otherwise: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "is no program this document could describe") {
			t.Fatalf("E056 on a .bot that declares a profile and no program: %+v", d)
		}
		if res.BotPath != filepath.Join(dir, "UPPER.BOT") {
			t.Fatalf("bot_path %q, want the .bot named as the document was named", res.BotPath)
		}
	})
}

// A declaration's rank is no part of the program — bar the ranks the
// compiled program itself reads, which the subtests "a .bot whose
// supervisors are declared in the other order" and "a .bot whose group uses
// are declared in the other order" hold. A document whose own declarations are
// written in another order than the .bot beside it is one program with it —
// the unit a launcher loads compiles the same either way, asserted here —
// so nothing is said. With an `import` line as without: the fragments are
// merged before either side is compiled, so the two cases are one.
func TestValidateReadsADeclarationsRankAsNoPartOfTheProgram(t *testing.T) {
	for _, importing := range []bool{true, false} {
		name := "the same program, two declarations written in another order"
		if importing {
			name += ", importing a fragment"
		}
		t.Run(name, func(t *testing.T) {
			doc, bot := documentWithItsBot(t, twoAgentsDocument(importing, "hello"))
			_, other := documentWithItsBot(t, twoAgentsDocument(importing, "judge"))
			// The premise: the two orders are one program for a launcher.
			if why := ir.SameProgram(theProgramALauncherRuns(t, bot), theProgramALauncherRuns(t, other)); why != "" {
				t.Fatalf("the two orders are not one program for a launcher: %s", why)
			}
			// The .bot of one order, the document of the other — reached by
			// an ordinary edit of the document alone.
			if err := os.WriteFile(doc, []byte(twoAgentsDocument(importing, "judge")), 0o644); err != nil {
				t.Fatal(err)
			}
			res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
			if err != nil || !res.Valid {
				t.Fatalf("validate refused a valid document: %v\n%s", err, out)
			}
			if hasDiagCode(res, "E056") {
				t.Fatalf("E056 on a pair that differs by the rank of two declarations:\n%s", out)
			}
			// The two texts differ all the same: that pair is the canonical
			// form's gate, not this warning.
			canonicalFormIsRed(t, doc)
		})
	}
	// A declaration the other side does not hold is still a difference: the
	// order-blind comparison is not a blind one.
	t.Run("one prompt more, importing a fragment", func(t *testing.T) {
		doc, bot := documentWithItsBot(t, twoAgentsDocument(true, "hello"))
		if err := os.WriteFile(bot, append(mustReadFile(t, bot), []byte("\nprompt extra:\n  More.\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Message, "describes another program") {
			t.Fatalf("E056 reads an extra prompt as the same program: %+v", d)
		}
	})
}

// Each conversion E056's hint names writes its destination from the other
// file and replaces that destination whole: the pair the warning reports
// becomes one program either way, and the warning goes. `--to yaml` writes
// the .bot's own `<bot>.yaml` — this document where the document's suffix
// is spelled that way, another file where it is not (#1762), which is why
// the remedy proven on a case-folded pair is the one writing the .bot.
func TestTheConversionsE056sHintNamesWriteTheirDestinationWhole(t *testing.T) {
	// `fmt --to bot --force <document>` writes the .bot from the document.
	t.Run("the document's .bot written over the .bot", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", authorHello)
		bot := writeFixture(t, dir, "hello.bot", string(theBotOf(t, documentAhead())))
		prependTo(t, bot, "## a comment the .bot carries\n")
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Hint, "`iterion fmt --to bot --force <document>`") || !strings.Contains(d.Hint, "replaces its destination whole") {
			t.Fatalf("the hint does not name the conversion that writes the .bot, nor what it replaces: %q", d.Hint)
		}

		fout, ferr := runFmtTwin(t, cli.FmtOptions{Paths: []string{doc}, To: "bot", Force: true})
		if ferr != nil {
			t.Fatalf("the conversion the hint names refused the pair it is named for: %v\n%s", ferr, fout)
		}
		// Whole: the program that .bot described and the comment it carried
		// are both gone, and what is there is the text the document writes.
		after := string(mustReadFile(t, bot))
		if strings.Contains(after, "judge") || strings.Contains(after, "a comment the .bot carries") {
			t.Fatalf("the --force write did not replace the .bot whole:\n%s", after)
		}
		// Its account covers what the destination carried that the text
		// replacing it has no form for — the comment line — and not the
		// program it replaced, which E056 is what reads.
		if !strings.Contains(fout, "comment line(s)") || strings.Contains(fout, "judge") {
			t.Fatalf("the --force account does not count the comment line, or names the program it replaced:\n%s", fout)
		}
		res, out, err = validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || hasDiagCode(res, "E056") {
			t.Fatalf("E056 after the conversion the hint names: %v\n%s", err, out)
		}
	})

	// `fmt --to yaml --force <bot>` writes `<bot>.yaml` from the .bot.
	t.Run("the .bot's document written over the document", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "hello.bot.yaml", "# a note the author keeps\n"+authorHello)
		bot := writeFixture(t, dir, "hello.bot", string(theBotOf(t, documentAhead())))
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || !res.Valid {
			t.Fatalf("a warning flipped the verdict: %v\n%s", err, out)
		}
		d := diagnosticWithCode(t, res, "E056", out)
		if !strings.Contains(d.Hint, "`iterion fmt --to yaml --force <bot>`") || !strings.Contains(d.Hint, "`<bot>.yaml`") {
			t.Fatalf("the hint does not name the conversion that writes the document, nor the file it writes: %q", d.Hint)
		}

		fout, ferr := runFmtTwin(t, cli.FmtOptions{Paths: []string{bot}, To: "yaml", Force: true})
		if ferr != nil {
			t.Fatalf("the conversion the hint names refused the pair it is named for: %v\n%s", ferr, fout)
		}
		// Whole: the author's note is gone and the program is that .bot's.
		after := string(mustReadFile(t, doc))
		if strings.Contains(after, "a note the author keeps") || !strings.Contains(after, "judge") {
			t.Fatalf("the --force write did not replace the document whole with the .bot's program:\n%s", after)
		}
		res, out, err = validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || hasDiagCode(res, "E056") {
			t.Fatalf("E056 after the conversion the hint names: %v\n%s", err, out)
		}
	})

	// On a document whose `.yaml` suffix is spelled otherwise (#1762), the
	// conversion that writes THIS document's .bot clears the warning all
	// the same: the .bot a document stands for is its name less `.yaml`,
	// whatever its case.
	t.Run("the document's .bot written over the .bot, suffix spelled otherwise", func(t *testing.T) {
		dir := t.TempDir()
		doc := writeFixture(t, dir, "UPPER.BOT.YAML", authorHello)
		writeFixture(t, dir, "UPPER.BOT", "dsl: 2\n")
		if res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{}); !hasDiagCode(res, "E056") {
			t.Fatalf("no E056 on the pair the remedy is named for:\n%s", out)
		}
		if fout, ferr := runFmtTwin(t, cli.FmtOptions{Paths: []string{doc}, To: "bot", Force: true}); ferr != nil {
			t.Fatalf("the conversion the hint names refused the pair it is named for: %v\n%s", ferr, fout)
		}
		res, out, err := validateDocumentJSON(t, doc, cli.ValidateOptions{})
		if err != nil || hasDiagCode(res, "E056") {
			t.Fatalf("E056 after the conversion the hint names: %v\n%s", err, out)
		}
	})
}

// `bot_path` is the .bot named as the document was named: the two paths of
// the result read in one form — relative where the operator typed a relative
// path, absolute where absolute — and the human output names it the same
// way.
func TestValidateNamesADocumentsBotAsTheDocumentWasNamed(t *testing.T) {
	dir := t.TempDir()
	// Under a directory of the working directory, so that the relative name
	// carries a directory part: a .bot named by its base name alone, or by
	// its absolute path, is another string than the one given. The directory
	// holds a space, as a path on a real disk may: the name is said whole.
	if err := os.MkdirAll(filepath.Join(dir, "d e"), 0o755); err != nil {
		t.Fatal(err)
	}
	abs := writeFixture(t, dir, filepath.Join("d e", "hello.bot.yaml"), authorHello)
	t.Chdir(dir)

	res, out, err := validateDocumentJSON(t, "d e/hello.bot.yaml", cli.ValidateOptions{})
	if err != nil || !res.Valid {
		t.Fatalf("validate refused the document by its relative path: %v\n%s", err, out)
	}
	if res.File != "d e/hello.bot.yaml" || res.BotPath != "d e/hello.bot" {
		t.Fatalf("file %q bot_path %q, want `d e/hello.bot.yaml` / `d e/hello.bot` — the two in the form the document was named", res.File, res.BotPath)
	}
	hp, hout := newTestPrinter(cli.OutputHuman)
	if err := cli.RunValidateWith("d e/hello.bot.yaml", hp, cli.ValidateOptions{}); err != nil {
		t.Fatalf("validate, human form: %v\n%s", err, hout.String())
	}
	// The value of the `Reads as` line, not its presence anywhere in the
	// output: the header already carries the document's own name, and every
	// positioned finding carries the document's absolute path.
	if got := readsAsValue(t, hout.String()); got != "d e/hello.bot" {
		t.Fatalf("the human output reads as %q, want `d e/hello.bot` — the .bot named as the document was given, space and all:\n%s", got, hout.String())
	}

	// An absolute document keeps the absolute form, as every surface that
	// resolves its input does (the MCP server resolves against its workdir).
	res, out, err = validateDocumentJSON(t, abs, cli.ValidateOptions{})
	if err != nil || !res.Valid {
		t.Fatalf("validate refused the document by its absolute path: %v\n%s", err, out)
	}
	if res.File != abs || res.BotPath != filepath.Join(dir, "d e", "hello.bot") {
		t.Fatalf("file %q bot_path %q, want the absolute document and %s", res.File, res.BotPath, filepath.Join(dir, "d e", "hello.bot"))
	}
}
