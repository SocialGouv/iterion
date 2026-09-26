package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// diagramJSON runs RunDiagram in JSON mode and decodes its result.
func diagramJSON(t *testing.T, file, view string) (DiagramResult, error) {
	t.Helper()
	jp, out := jsonPrinter()
	err := RunDiagram(DiagramOptions{File: file, View: view}, jp)
	var res DiagramResult
	if err == nil {
		if jerr := json.Unmarshal(out.Bytes(), &res); jerr != nil {
			t.Fatalf("diagram printed no result JSON (%v):\n%s", jerr, out.String())
		}
	}
	return res, err
}

// loopDoc describes a program with a judge, a bounded loop and its exit: a
// diagram with more to draw than one edge.
const loopDoc = "dsl: 2\nprompts:\n  ask: Say hello.\n  judge_it: Is it right?\nschemas:\n  verdict:\n    ok: bool\nnodes:\n  - agent: hello\n    model: m\n    system: ask\n  - judge: check\n    model: m\n    system: judge_it\n    output: verdict\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> check\n    - check -> done when ok\n    - check -> hello when not ok as again(2)\n    - check -> done\n"

// `iterion diagram x.bot.yaml` draws the .bot the document stands for: in
// every view, the diagram `diagram x.bot` draws once `fmt --to bot` has
// written that .bot — which the diagram itself never writes — and the result
// says it read a document, and which .bot it stands for.
func TestDiagramDrawsTheBotADocumentStandsFor(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "d/hello.bot.yaml", loopDoc)
	abs, err := filepath.Abs("d/hello.bot")
	if err != nil {
		t.Fatal(err)
	}
	views := []string{"compact", "detailed", "full"}
	fromDoc := map[string]DiagramResult{}
	for _, view := range views {
		res, err := diagramJSON(t, doc, view)
		if err != nil {
			t.Fatalf("diagram --view %s of the document: %v", view, err)
		}
		if res.SourceKind != "author" || res.BotPath != abs || res.File != doc || res.WorkflowName != "hello" || !strings.Contains(res.Mermaid, "check") {
			t.Fatalf("--view %s: source_kind %q bot_path %q file %q workflow %q, want author / %s / the document / hello, and a diagram that draws check:\n%s", view, res.SourceKind, res.BotPath, res.File, res.WorkflowName, abs, res.Mermaid)
		}
		fromDoc[view] = res
	}
	if _, err := os.Stat("d/hello.bot"); err == nil {
		t.Fatal("diagram wrote the .bot the document stands for")
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
		t.Fatalf("fmt --to bot: %v", err)
	}
	for _, view := range views {
		res, err := diagramJSON(t, "d/hello.bot", view)
		if err != nil {
			t.Fatalf("diagram --view %s of the .bot: %v", view, err)
		}
		if res.SourceKind != "" || res.BotPath != "" {
			t.Fatalf("a .bot drawn as a document: source_kind %q bot_path %q", res.SourceKind, res.BotPath)
		}
		if res.Mermaid != fromDoc[view].Mermaid || res.WorkflowName != fromDoc[view].WorkflowName {
			t.Fatalf("--view %s: the document's diagram is not the one of the .bot it stands for:\n%s\nwant\n%s", view, fromDoc[view].Mermaid, res.Mermaid)
		}
	}
	hp, out := humanPrinter()
	if err := RunDiagram(DiagramOptions{File: doc}, hp); err != nil {
		t.Fatalf("diagram, human form: %v", err)
	}
	if !strings.Contains(out.String(), "Reads as") || !strings.Contains(out.String(), abs) {
		t.Fatalf("the human form does not say which .bot the document stands for:\n%s", out.String())
	}
}

// A document that stands for a bundle's main is drawn in that bundle — its
// prompts/*.md in scope — whether or not the main.bot is written; one that
// stands for any other workflow file of a bundle is drawn in the bundle that
// file belongs to, as the .bot's own diagram draws it.
func TestDiagramReadsADocumentInItsBundle(t *testing.T) {
	inTempWorkspace(t)
	writeBot(t, "b/manifest.yaml", "schema_version: 1\nname: probe\n")
	writeBot(t, "b/prompts/ask.md", "Say hello.\n")
	body := "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
	main := writeBot(t, "b/main.bot.yaml", body)
	if _, err := diagramJSON(t, main, ""); err != nil {
		t.Fatalf("the bundle's prompt was not in scope for the main's document: %v", err)
	}
	if _, err := os.Stat("b/main.bot"); err == nil {
		t.Fatal("diagram wrote main.bot")
	}
	writeBot(t, "b/main.bot", "dsl: 2\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n")
	other := writeBot(t, "b/other.bot.yaml", body)
	res, err := diagramJSON(t, other, "")
	if err != nil {
		t.Fatalf("the bundle's prompt was not in scope for another workflow's document: %v", err)
	}
	writeBot(t, "b/other.bot", "dsl: 2\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n")
	fromBot, err := diagramJSON(t, "b/other.bot", "")
	if err != nil {
		t.Fatalf("the .bot's own diagram: %v", err)
	}
	if res.Mermaid != fromBot.Mermaid {
		t.Fatalf("the document of another workflow is not drawn as its .bot:\n%s\nwant\n%s", res.Mermaid, fromBot.Mermaid)
	}
}

// A document's imports are read beside the .bot it stands for: a node its
// fragment declares is drawn.
func TestDiagramReadsADocumentsFragments(t *testing.T) {
	inTempWorkspace(t)
	writeBot(t, "u/lib/nodes.bot", "dsl: 2\n\nprompt ask:\n  Say hello.\n\nagent helper:\n  model: \"m\"\n  system: ask\n")
	doc := writeBot(t, "u/main.bot.yaml", "dsl: 2\nimports: [lib/nodes.bot]\nworkflow:\n  name: w\n  entry: helper\n  edges:\n    - helper -> done\n")
	res, err := diagramJSON(t, doc, "")
	if err != nil {
		t.Fatalf("the document's fragment was not read: %v", err)
	}
	if !strings.Contains(res.Mermaid, "helper") {
		t.Fatalf("the node the fragment declares is not drawn:\n%s", res.Mermaid)
	}
}

// What leaves no .bot to draw refuses the diagram, by name, and writes
// nothing: a document that does not read, a program with no written .bot
// form (E054), a fragment that cannot be read — said at the document's
// own line, not at the .bot's name — a program that does not compile. A
// file that is neither a .bot nor a document is refused as ever.
func TestDiagramRefusesWhatLeavesNoBotToDraw(t *testing.T) {
	inTempWorkspace(t)
	desc := strings.Repeat("    a line of the description\n", 40)
	for name, tc := range map[string]struct{ doc, want string }{
		"a document that does not read":   {"dsl: 2\nnodes: nope\n", "E051"},
		"no written .bot form":            {"dsl: 1\ncatalog:\n  name: probe\n  description: |\n" + desc + "nodes:\n  - tool: t\n    command: \"printf `x` \\\"y\\\"\"\n    output: out\nschemas:\n  out:\n    ok: bool\nworkflow:\n  name: w\n  entry: t\n  edges:\n    - t -> done\n", "E054"},
		"a fragment that cannot be read":  {"dsl: 2\nimports: [lib/missing.bot]\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n", "x.bot.yaml:2:11: error [E046]"},
		"a program that does not compile": {strings.Replace(authorHelloDoc, "entry: hello", "entry: nope", 1), "compile error"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := strings.ReplaceAll(name, " ", "_")
			doc := writeBot(t, dir+"/x.bot.yaml", tc.doc)
			if _, err := diagramJSON(t, doc, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, tc.want)
			}
			if _, err := os.Stat(dir + "/x.bot"); err == nil {
				t.Fatal("a refused diagram wrote the .bot")
			}
		})
	}
	writeBot(t, "z/x.yaml", authorHelloDoc)
	if _, err := diagramJSON(t, "z/x.yaml", ""); err == nil || !strings.Contains(err.Error(), ".bot.yaml") {
		t.Fatalf("a .yaml that is not a document: %v, want the refusal naming both kinds", err)
	}
}

// helloDocWith is a document of one agent whose prompt is declared, with
// extra lines under its workflow.
func helloDocWith(workflowLines string) string {
	return "dsl: 2\nprompts:\n  ask: Say hello.\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n" + workflowLines + "  edges:\n    - hello -> done\n"
}

// What refuses the .bot after its compile refuses the document read as it,
// in the same words: a program naming an MCP server nobody declares, a
// .mcp.json beside it that does not parse.
func TestDiagramRefusesADocumentAsItsBotIsRefused(t *testing.T) {
	inTempWorkspace(t)
	for name, tc := range map[string]struct{ doc, mcpJSON string }{
		"an MCP server nobody declares":   {doc: helloDocWith("  mcp:\n    servers: [github]\n")},
		"a .mcp.json that does not parse": {doc: helloDocWith(""), mcpJSON: `{ "mcpServers": { "broken":`},
	} {
		t.Run(name, func(t *testing.T) {
			dir := strings.ReplaceAll(name, " ", "_")
			doc := writeBot(t, dir+"/x.bot.yaml", tc.doc)
			if tc.mcpJSON != "" {
				writeBot(t, dir+"/.mcp.json", tc.mcpJSON)
			}
			_, docErr := diagramJSON(t, doc, "")
			if docErr == nil {
				t.Fatal("the document was drawn")
			}
			if _, err := os.Stat(dir + "/x.bot"); err == nil {
				t.Fatal("a refused diagram wrote the .bot")
			}
			if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
				t.Fatalf("fmt --to bot: %v", err)
			}
			if _, botErr := diagramJSON(t, dir+"/x.bot", ""); botErr == nil || botErr.Error() != docErr.Error() {
				t.Fatalf("the document is refused with %q and its .bot with %v: want the same refusal", docErr, botErr)
			}
		})
	}
}

// A manifest export naming the .bot a document stands for is that .bot,
// written or not: the document of an exported main, and of another exported
// workflow, are drawn in their bundle — and validated, for the main — as
// their .bot is once written.
func TestDiagramReadsADocumentAManifestExports(t *testing.T) {
	inTempWorkspace(t)
	agentDoc := "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n"
	writeBot(t, "m/manifest.yaml", "schema_version: 1\nname: probe\nexports:\n  workflows:\n    - id: main\n      path: main.bot\n")
	writeBot(t, "m/prompts/ask.md", "Say hello.\n")
	writeBot(t, "x/manifest.yaml", "schema_version: 1\nname: probe\nexports:\n  workflows:\n    - id: other\n      path: workflows/other.bot\n")
	writeBot(t, "x/prompts/ask.md", "Say hello.\n")
	writeBot(t, "x/main.bot", "dsl: 2\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n")
	jp, _ := jsonPrinter()
	for _, doc := range []string{writeBot(t, "m/main.bot.yaml", agentDoc), writeBot(t, "x/workflows/other.bot.yaml", agentDoc)} {
		fromDoc, err := diagramJSON(t, doc, "full")
		if err != nil {
			t.Fatalf("%s: the document of an exported workflow is refused: %v", doc, err)
		}
		if strings.HasSuffix(doc, "main.bot.yaml") {
			if err := RunValidate(doc, jp); err != nil {
				t.Fatalf("%s: validate refuses the document of an exported main: %v", doc, err)
			}
		}
		if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
			t.Fatalf("fmt --to bot: %v", err)
		}
		fromBot, err := diagramJSON(t, strings.TrimSuffix(doc, ".yaml"), "full")
		if err != nil || fromBot.Mermaid != fromDoc.Mermaid {
			t.Fatalf("%s: the .bot written draws (%v):\n%s\nwant the document's\n%s", doc, err, fromBot.Mermaid, fromDoc.Mermaid)
		}
	}
}

// A document's fragments are read where its .bot's diagram reads them: a
// workflow file reached through a symlinked directory, with no lib/ of its
// own beside it, has them read beside its bundle's main. validate reads the
// document as `validate` reads that .bot — the same verdict.
func TestDiagramReadsADocumentsFragmentsWhereItsBotDoes(t *testing.T) {
	root := inTempWorkspace(t)
	writeBot(t, "s/bundle/manifest.yaml", "schema_version: 1\nname: probe\n")
	writeBot(t, "s/bundle/prompts/ask.md", "Say hello.\n")
	writeBot(t, "s/bundle/lib/nodes.bot", "dsl: 2\n\nagent helper:\n  model: \"m\"\n  system: ask\n")
	writeBot(t, "s/bundle/main.bot", "dsl: 2\n\nimport \"lib/nodes.bot\"\n\nworkflow main_wf:\n  entry: helper\n  helper -> done\n")
	writeBot(t, "s/elsewhere/wf/other.bot.yaml", "dsl: 2\nimports: [lib/nodes.bot]\nworkflow:\n  name: other_wf\n  entry: helper\n  edges:\n    - helper -> done\n")
	if err := os.Symlink(filepath.Join(root, "s", "elsewhere", "wf"), filepath.Join(root, "s", "bundle", "workflows")); err != nil {
		t.Fatal(err)
	}
	doc := "s/bundle/workflows/other.bot.yaml"
	fromDoc, err := diagramJSON(t, doc, "full")
	if err != nil {
		t.Fatalf("the document's fragments were not read where its .bot's diagram reads them: %v", err)
	}
	jp, _ := jsonPrinter()
	docValidate := RunValidate(doc, jp)
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
		t.Fatalf("fmt --to bot: %v", err)
	}
	fromBot, err := diagramJSON(t, "s/bundle/workflows/other.bot", "full")
	if err != nil || fromBot.Mermaid != fromDoc.Mermaid {
		t.Fatalf("the .bot written draws (%v):\n%s\nwant the document's\n%s", err, fromBot.Mermaid, fromDoc.Mermaid)
	}
	if botValidate := RunValidate("s/bundle/workflows/other.bot", jp); (docValidate == nil) != (botValidate == nil) {
		t.Fatalf("validate: the document gives %v and its .bot %v — want the same verdict", docValidate, botValidate)
	}
}

// A .bot path `fmt --to bot` will not write over whatever --force says — a
// directory, a file that cannot be read — is one the document stands for in
// vain: diagram and validate refuse the document, and fmt refuses to write,
// all three in the same words.
func TestADocumentWhoseBotFmtWillNotWriteOverIsRefused(t *testing.T) {
	inTempWorkspace(t)
	for name, tc := range map[string]struct {
		place func(t *testing.T, bot string)
		want  string
	}{
		"a directory": {func(t *testing.T, bot string) {
			if err := os.MkdirAll(bot, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "is there and is a directory, not a file"},
		"a file that cannot be read": {func(t *testing.T, bot string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a file whatever its mode")
			}
			writeBot(t, bot, "dsl: 2\n")
			if err := os.Chmod(bot, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(bot, 0o644) })
		}, "is there and cannot be read"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := strings.ReplaceAll(name, " ", "_")
			doc := writeBot(t, dir+"/x.bot.yaml", helloDocWith(""))
			tc.place(t, dir+"/x.bot")
			if _, err := diagramJSON(t, doc, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("diagram: %v, want the refusal saying the .bot %s", err, tc.want)
			}
			jp, _ := jsonPrinter()
			if err := RunValidate(doc, jp); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate: %v, want the refusal saying the .bot %s", err, tc.want)
			}
			res, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Force: true, Printer: jp})
			if !errors.Is(err, ErrFmtRefused) || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], tc.want) {
				t.Fatalf("fmt --to bot --force: %v %q, want the refusal saying the .bot %s", err, res.Refused, tc.want)
			}
		})
	}
}

// A document named `UPPER.BOT.YAML` stands for `UPPER.BOT`, and every
// surface takes that name now that the workflow file suffix is case-folded
// (one rule with bundle.Detect, #1762): diagram draws the document, fmt
// --to bot writes the .bot, and the file written is one the same binary
// reads back — through validate, the walks and `--to yaml`.
func TestDiagramTakesADocumentWhoseBotEverySurfaceReadsBack(t *testing.T) {
	inTempWorkspace(t)
	doc := writeBot(t, "u/UPPER.BOT.YAML", helloDocWith(""))
	if _, err := diagramJSON(t, doc, ""); err != nil {
		t.Fatalf("diagram refuses UPPER.BOT.YAML: %v", err)
	}
	jp, _ := jsonPrinter()
	if err := RunValidate(doc, jp); err != nil {
		t.Fatalf("validate refuses what it reads as UPPER.BOT: %v", err)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot", Printer: jp}); err != nil {
		t.Fatalf("fmt --to bot on UPPER.BOT.YAML: %v, want the conversion", err)
	}
	if _, err := os.Stat("u/UPPER.BOT"); err != nil {
		t.Fatalf("UPPER.BOT not on disk: %v", err)
	}
}

// A main.bot.yaml in a directory that is no bundle of its own is not drawn
// in the bundle of a directory above it, as its .bot would not be: a bare
// main.bot is promoted to its own directory's bundle only, and the prompt
// that bundle carries is out of scope for both.
func TestDiagramDoesNotDrawAMainDocumentInABundleAbove(t *testing.T) {
	inTempWorkspace(t)
	writeBot(t, "outer/manifest.yaml", "schema_version: 1\nname: probe\n")
	writeBot(t, "outer/prompts/ask.md", "Say hello.\n")
	writeBot(t, "outer/main.bot", "dsl: 2\n\nagent hello:\n  model: \"m\"\n  system: ask\n\nworkflow hello:\n  entry: hello\n  hello -> done\n")
	doc := writeBot(t, "outer/sub/main.bot.yaml", "dsl: 2\nnodes:\n  - agent: hello\n    model: m\n    system: ask\nworkflow:\n  name: hello\n  entry: hello\n  edges:\n    - hello -> done\n")
	_, docErr := diagramJSON(t, doc, "")
	if docErr == nil || !strings.Contains(docErr.Error(), `unknown prompt "ask"`) {
		t.Fatalf("the document was drawn in the bundle above: %v", docErr)
	}
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
		t.Fatalf("fmt --to bot: %v", err)
	}
	if _, botErr := diagramJSON(t, "outer/sub/main.bot", ""); botErr == nil || botErr.Error() != docErr.Error() {
		t.Fatalf("the document is refused with %q and its .bot with %v: want the same refusal", docErr, botErr)
	}
}

// The document of a bundle's main whose bundle does not open — a manifest
// that does not decode — is refused as its .bot is, remedy included, by
// diagram and by validate.
func TestADocumentOfAMainWhoseBundleDoesNotOpenIsRefusedAsItsBot(t *testing.T) {
	inTempWorkspace(t)
	writeBot(t, "m/manifest.yaml", "schema_version: 1\nname: probe\nexports: [not: a mapping\n")
	doc := writeBot(t, "m/main.bot.yaml", helloDocWith(""))
	if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
		t.Fatalf("fmt --to bot: %v", err)
	}
	const remedy = "give main.bot a directory of its own if this is not its bundle"
	jp, _ := jsonPrinter()
	for _, path := range []string{doc, "m/main.bot"} {
		if _, err := diagramJSON(t, path, ""); err == nil || !strings.Contains(err.Error(), remedy) {
			t.Errorf("diagram %s: %v, want the refusal with its remedy", path, err)
		}
		if err := RunValidate(path, jp); err == nil || !strings.Contains(err.Error(), remedy) {
			t.Errorf("validate %s: %v, want the refusal with its remedy", path, err)
		}
	}
}

// Over every .bot of bots/ and examples/ — fragments included — the diagram
// of its author document is measured in the state the contract is about:
// the document written beside a .bot that is NOT there (removed, the bundle
// and fragments around it the corpus's own), drawn in every view, named
// relative and absolute; then `fmt --to bot` writes the .bot and it is drawn
// the same way. The two agree on the verdict, on the diagram, and on the
// words of a refusal once the document's name is read as the .bot's and
// positions — the document's own lines — are set aside; where `fmt` refuses
// to write, the document was refused too. The original .bot is restored
// after each. The counts are reported, and a bench that measured too few
// fails: a corpus that stopped converting would pass green otherwise.
func TestTheDiagramOfADocumentIsTheDiagramOfItsBotOverTheCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("corpus bench")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root := inTempWorkspace(t)
	for _, tree := range []string{"bots", "examples"} {
		copyTreeForTest(t, filepath.Join(repo, tree), filepath.Join(root, tree))
	}
	var bots []string
	for _, tree := range []string{"bots", "examples"} {
		err := filepath.WalkDir(tree, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && workflowfile.IsWorkflowFile(p) {
				bots = append(bots, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(bots)
	position := regexp.MustCompile(`:\d+:\d+:`)
	type side struct{ mermaid, err string }
	draw := func(path, view string) side {
		res, err := diagramJSON(t, path, view)
		if err != nil {
			return side{err: err.Error()}
		}
		return side{mermaid: res.Mermaid}
	}
	views := []string{"compact", "detailed", "full"}
	compared, bothRefuse, fmtRefused, notWritten := 0, 0, 0, 0
	for _, bot := range bots {
		orig, err := os.ReadFile(bot)
		if err != nil {
			t.Fatal(err)
		}
		pr := parser.Parse(bot, string(orig))
		if diagnosticErrors(pr.Diagnostics) != "" {
			notWritten++
			continue
		}
		out, err := author.Write(pr.File)
		if err != nil {
			notWritten++
			continue
		}
		doc := bot + ".yaml"
		absDoc, err := filepath.Abs(doc)
		if err != nil {
			t.Fatal(err)
		}
		absBot := absDoc[:len(absDoc)-len(".yaml")]
		if err := os.WriteFile(doc, out, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(bot); err != nil {
			t.Fatal(err)
		}
		fromDoc := map[string]side{}
		for _, view := range views {
			fromDoc[view+" rel"] = draw(doc, view)
			fromDoc[view+" abs"] = draw(absDoc, view)
		}
		if _, err := os.Stat(bot); err == nil {
			t.Fatalf("%s: the diagram of its document wrote it", bot)
		}
		drawn, refused := 0, 0
		if _, err := RunFmt(FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
			fmtRefused++
			for key, s := range fromDoc {
				if s.err == "" {
					t.Errorf("%s (%s): fmt --to bot refuses to write it (%v) and its document draws", bot, key, err)
				}
			}
		} else {
			for _, view := range views {
				for _, form := range []string{"rel", "abs"} {
					path := bot
					if form == "abs" {
						path = absBot
					}
					d, b := fromDoc[view+" "+form], draw(path, view)
					switch {
					case (d.err == "") != (b.err == ""):
						t.Errorf("%s --view %s (%s): one side draws and the other refuses — the document: %q; the .bot: %q", bot, view, form, d.err, b.err)
					case d.err != "":
						asBot := strings.ReplaceAll(strings.ReplaceAll(d.err, absDoc, absBot), doc, bot)
						if position.ReplaceAllString(asBot, ":") != position.ReplaceAllString(b.err, ":") {
							t.Errorf("%s --view %s (%s): refused in other words — the document: %q; the .bot: %q", bot, view, form, d.err, b.err)
						}
						refused++
					case d.mermaid != b.mermaid:
						t.Errorf("%s --view %s (%s): the document's diagram is not the .bot's", bot, view, form)
					default:
						drawn++
					}
				}
			}
		}
		switch 2 * len(views) {
		case drawn:
			compared++
		case refused:
			bothRefuse++
		}
		if err := os.WriteFile(bot, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(doc); err != nil {
			t.Fatal(err)
		}
	}
	measured := len(bots) - notWritten
	t.Logf("bots=%d compared=%d bothRefuse=%d fmtRefused=%d notWrittenAsDocument=%d", len(bots), compared, bothRefuse, fmtRefused, notWritten)
	if notWritten*10 > len(bots) || compared*10 < measured*8 {
		t.Fatalf("%d of %d .bot measured, %d compared: the bench no longer measures the corpus", measured, len(bots), compared)
	}
}

// copyTreeForTest copies the regular files of src under dst, directories
// created as needed; a symlink is skipped.
func copyTreeForTest(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
