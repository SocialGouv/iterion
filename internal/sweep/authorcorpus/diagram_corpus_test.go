package authorcorpus

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

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
	// The module root is resolved from the package directory, before the
	// test moves into the workspace it draws in.
	repo := sweepwalk.ModuleRoot(t)
	root := t.TempDir()
	t.Chdir(root)
	for _, tree := range []string{"bots", "examples"} {
		copyAuthoringCorpusForTest(t, filepath.Join(repo, tree), filepath.Join(root, tree))
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
		pr := parsertest.Parse(bot, string(orig))
		if hasParseError(pr.Diagnostics) {
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
		if _, err := cli.RunFmt(t.Context(), cli.FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
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

func TestAuthoringCorpusCopyOwnsItsRuntimeState(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	doc := writeBot(t, filepath.Join(src, "main.bot.yaml"), helloDocWith(""))
	if result, err := cli.RunFmt(t.Context(), cli.FmtOptions{Paths: []string{doc}, To: "bot"}); err != nil {
		t.Fatalf("prepare the source with the real formatter: %v (%v)", err, result.Refused)
	}
	state := filepath.Join(src, ".iterion", "authoring")
	if info, err := os.Stat(state); err != nil || !info.IsDir() {
		t.Fatalf("the formatter did not establish source authoring state: %v", err)
	}
	private := []string{
		filepath.Join(".iterion", "authoring", "host-only-state"),
		filepath.Join("fragments", ".iterion", "run", "host-only-state"),
	}
	for _, path := range private {
		writeBot(t, filepath.Join(src, path), "source session state\n")
	}
	resources := map[string]string{
		filepath.Join("prompts", "context.md"):              "author context\n",
		filepath.Join("assets", ".iterion"):                 "a regular authored file\n",
		filepath.Join("assets", ".iterion-example", "data"): "an authored resource directory\n",
	}
	for path, body := range resources {
		writeBot(t, filepath.Join(src, path), body)
	}
	copyAuthoringCorpusForTest(t, src, dst)
	if err := os.Remove(filepath.Join(dst, "main.bot")); err != nil {
		t.Fatal(err)
	}
	if result, err := cli.RunFmt(t.Context(), cli.FmtOptions{Paths: []string{filepath.Join(dst, "main.bot.yaml")}, To: "bot"}); err != nil {
		t.Fatalf("the copied corpus must acquire its own authoring lock: %v (%v)", err, result.Refused)
	}
	for _, path := range private {
		if _, err := os.Stat(filepath.Join(dst, path)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("source session state crossed the corpus boundary at %s: %v", path, err)
		}
	}
	for path, want := range resources {
		got, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil || string(got) != want {
			t.Errorf("authored resource %s = %q, %v; want %q", path, got, err, want)
		}
	}
}

// copyAuthoringCorpusForTest copies regular authoring files, excluding the
// source session's .iterion directories and symlinks. The copy owns its state.
func copyAuthoringCorpusForTest(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".iterion" {
			return filepath.SkipDir
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

// diagramJSON runs cli.RunDiagram in JSON mode and decodes its result.
func diagramJSON(t *testing.T, file, view string) (cli.DiagramResult, error) {
	t.Helper()
	buf := &bytes.Buffer{}
	err := cli.RunDiagram(cli.DiagramOptions{File: file, View: view}, &cli.Printer{W: buf, Format: cli.OutputJSON})
	var res cli.DiagramResult
	if err == nil {
		if jerr := json.Unmarshal(buf.Bytes(), &res); jerr != nil {
			t.Fatalf("diagram printed no result JSON (%v):\n%s", jerr, buf.String())
		}
	}
	return res, err
}

// hasParseError reports whether the parser only recovered on a file: such a
// text holds no program the author document writer reads.
func hasParseError(diags []parser.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}
