package cli_test

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// everyBotInTheTree is every `.bot` this repository holds — the bots the
// catalogue serves, the examples, the end-to-end missions, the fixtures:
// the corpus an author adopting the YAML twin of a .bot works on.
func everyBotInTheTree(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	var out []string
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The rule the CLI's own collector walks by: two spellings of
			// "which files are in the tree" put this test and
			// `iterion fmt --check` in a disagreement no baseline settles.
			if p != root && workflowfile.SkipWalkDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if workflowfile.IsWorkflowFile(p) {
			out = append(out, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) < 150 {
		t.Fatalf("only %d .bot found in the tree — the walk has gone blind", len(out))
	}
	return out
}

// theProgramOfTheBot is the program a launcher runs from the .bot at path:
// its unit loaded off the disk, merged and compiled, as the engine does.
func theProgramOfTheBot(t *testing.T, path string) *ir.CompileResult {
	t.Helper()
	return ir.Compile(unittest.Whole(unit.LoadDir(path)).Merged)
}

// theProgramOfTheDocument is the program the document at path describes,
// read as `validate` reads it: the document's own AST as the main of the
// unit rooted at the .bot it stands for, merged and compiled.
func theProgramOfTheDocument(t *testing.T, bot, doc string) *ir.CompileResult {
	t.Helper()
	src := mustReadFile(t, doc)
	res := author.Parse(doc, src)
	if res.HasErrors() {
		t.Fatalf("the document %s does not read: %v", doc, res.Diagnostics)
	}
	return ir.Compile(unittest.Whole(unit.LoadDirWithMainAST(bot, doc, res.File, src, res.Spent)).Merged)
}

// aPairOfItsOwn copies the .bot at path and every fragment its unit reads
// into a directory of its own, each file under its unit-relative name, and
// writes the author document of that copy with `fmt --to yaml`: the pair an
// author holds after one conversion. The reason it could not be made comes
// back instead, for a corpus file no conversion writes a document from.
func aPairOfItsOwn(t *testing.T, path string) (bot, doc, why string) {
	t.Helper()
	u := unittest.Whole(unit.LoadDir(path))
	if u.Merged == nil || len(u.Files) == 0 {
		return "", "", "the unit holds no file"
	}
	dir := t.TempDir()
	for _, f := range u.Files {
		at := filepath.Join(dir, filepath.FromSlash(f.Rel))
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, f.Source, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bot = filepath.Join(dir, filepath.FromSlash(u.Main))
	if out, err := runFmtTwin(t, cli.FmtOptions{Paths: []string{bot}, To: "yaml"}); err != nil {
		return "", "", strings.ReplaceAll(firstLine(out), dir, "<dir>")
	}
	return bot, bot + ".yaml", ""
}

// firstLine is the first line of a report, for a table of reasons.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// theEditsOfACorpusBot are the edits of a .bot this test holds E056
// against, each mapped to whether the program the file describes survives
// it. A PRESERVING edit is the feature's whole value — the pair a
// conversion leaves differs by a comment for ever, and a rank the compiled
// program does not read is no difference either — so the oracle is required
// to answer "one program" on every corpus file it reaches. A CHANGING edit
// is best-effort: whether it reaches a given file's program is the
// compiler's business, and for a few it does not (a renamed agent no
// workflow reaches), so there the oracle decides and E056 is held to it.
//
// No edit here touches a rank the compiled program reads: which ranks those
// are is the compiler's business and this table is not their list, and the
// corpus materialises them badly anyway — a unit holds one workflow (a .bot
// declaring two is refused by the load, E010), no tracked .bot declares two
// `supervisor`s (9 declare one) and exactly one declares a `use`. The
// witnesses written for those ranks carry them instead, on fixtures of
// their own.
var theEditsOfACorpusBot = map[string]bool{
	"the .bot the document writes":            true,
	"a comment line added":                    true,
	"two prompts declared in the other order": true,
	"an agent renamed":                        false,
	"an edge removed":                         false,
}

// editsOf applies theEditsOfACorpusBot to one corpus .bot. Each is
// generated from the file's own AST, so a corpus file that has no such
// construct simply yields fewer — and an edit no corpus file yields at all
// is a guard that cannot fail, which the caller reddens on.
func editsOf(t *testing.T, path string) map[string][]byte {
	t.Helper()
	raw := mustReadFile(t, path)
	edits := map[string][]byte{
		"a comment line added": append([]byte("## a line no document has a form for\n"), raw...),
	}
	// A kind whose rank the compiled program does not read: the prompts,
	// the schemas, the agents — reached by name or by id, never by rank.
	if text, ok := unparsedWith(t, path, func(f *ast.File) bool {
		if len(f.Prompts) < 2 {
			return false
		}
		f.Prompts[0], f.Prompts[1] = f.Prompts[1], f.Prompts[0]
		return true
	}); ok {
		edits["two prompts declared in the other order"] = text
	}
	if text, ok := unparsedWith(t, path, func(f *ast.File) bool {
		if len(f.Agents) == 0 {
			return false
		}
		f.Agents[0].Name = f.Agents[0].Name + "_renamed"
		return true
	}); ok {
		edits["an agent renamed"] = text
	}
	if text, ok := unparsedWith(t, path, func(f *ast.File) bool {
		if len(f.Workflows) == 0 || len(f.Workflows[0].Edges) == 0 {
			return false
		}
		f.Workflows[0].Edges = f.Workflows[0].Edges[1:]
		return true
	}); ok {
		edits["an edge removed"] = text
	}
	return edits
}

// parsedBot is path's AST, or nil where the parser only recovers on the
// file: such a text holds no program to edit.
func parsedBot(t *testing.T, path string) *ast.File {
	t.Helper()
	pr := parsertest.Parse(path, string(mustReadFile(t, path)))
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			return nil
		}
	}
	return pr.File
}

// unparsedWith is the .bot text of path's AST once edit has changed it, and
// whether the file held the construct the edit needs. The text is written
// by the unparser, so the edit is a program edit and not a text one.
func unparsedWith(t *testing.T, path string, edit func(*ast.File) bool) ([]byte, bool) {
	t.Helper()
	f := parsedBot(t, path)
	if f == nil || !edit(f) {
		return nil, false
	}
	return []byte(unparse.Unparse(f)), true
}

// E056 is silent on the pair a conversion leaves EXACTLY when the two files
// are one program for the compiler, and names the difference otherwise.
// Measured over every .bot in the tree, against the author document
// `fmt --to yaml` writes from it, as it is and under five edits of its own
// AST: the oracle E056 answers with is the compiler's, so the test asks the
// compiler for the expectation and `validate` for the answer. A corpus file
// no conversion writes a document from is named with its reason in the
// table this test prints, never passed over in silence.
//
// The other half of the guarantee is held where the writer lives:
// TestCorpusRoundTripsThroughTheAuthorDocument (pkg/dsl/author) holds the
// same corpus to one program through the document, unit for unit. This one
// holds the surface that reports on it.
func TestE056IsSilentExactlyWhenTheTwoFilesAreOneProgram(t *testing.T) {
	var skipped []string
	pairs, silent, named, reachedNothing := 0, 0, 0, 0
	cases := map[string]int{} // an edit → the corpus files it materialised for
	for _, path := range everyBotInTheTree(t) {
		name := filepath.ToSlash(strings.TrimPrefix(path, ".."+string(filepath.Separator)+".."+string(filepath.Separator)))
		t.Run(name, func(t *testing.T) {
			bot, doc, why := aPairOfItsOwn(t, path)
			if why != "" {
				skipped = append(skipped, name+": "+why)
				// A corpus file that yields no pair is a hole in the
				// measure, and a t.Logf on a passing test reaches no gate:
				// the reason is said where a gate reads it.
				t.Errorf("no pair could be made from this corpus file: %s", why)
				return
			}
			pairs++
			canonical := mustReadFile(t, bot)
			theDocuments := theProgramOfTheDocument(t, bot, doc)
			edits := editsOf(t, path)
			edits["the .bot the document writes"] = canonical
			for _, edit := range slices.Sorted(maps.Keys(edits)) {
				preserving, declared := theEditsOfACorpusBot[edit]
				if !declared {
					t.Fatalf("%s: an edit this test's own table does not declare", edit)
				}
				if err := os.WriteFile(bot, edits[edit], 0o644); err != nil {
					t.Fatal(err)
				}
				cases[edit]++
				// The compiler's answer on the very pair on disk, and
				// validate's.
				want := ir.SameProgram(theDocuments, theProgramOfTheBot(t, bot))
				res, out, _ := validateDocumentJSON(t, doc, cli.ValidateOptions{})
				// A preserving edit is asserted, never derived: an oracle
				// grown sensitive to a comment or to a declaration's rank
				// would flip `want` with it, and this loop would stay green
				// on the very cases whose SILENCE is the feature.
				if preserving && want != "" {
					t.Fatalf("%s: the compiler reads a program-preserving edit as another program: %s", edit, want)
				}
				if want == "" {
					silent++
					if !preserving {
						reachedNothing++
					}
				} else {
					named++
				}
				switch said := hasDiagCode(res, "E056"); {
				case want == "" && said:
					t.Fatalf("%s: E056 on a pair the compiler reads as one program:\n%s", edit, out)
				case want != "" && !said:
					t.Fatalf("%s: no E056 on a pair the compiler reads as two programs (%s):\n%s", edit, want, out)
				}
			}
		})
	}
	// The totals are the corpus's, not one file's: they live in a subtest
	// of their own, so `-run` naming one corpus file debugs that file
	// instead of failing on a walk it narrowed itself.
	t.Run("the corpus exercises both answers", func(t *testing.T) {
		t.Logf("%d pairs: %d cases one program (E056 silent), %d cases two programs (E056 named); %d case(s) of a program-changing edit the compiler reads as one program", pairs, silent, named, reachedNothing)
		for _, edit := range slices.Sorted(maps.Keys(theEditsOfACorpusBot)) {
			t.Logf("%4d  %s", cases[edit], edit)
		}
		for _, s := range skipped {
			t.Logf("no pair: %s", s)
		}
		if pairs < 150 {
			t.Fatalf("%d pairs compared, %d files without one — the corpus has gone blind", pairs, len(skipped))
		}
		// An edit the table declares and no corpus file yields is a guard
		// that cannot fail: it is removed, or the witness that carries its
		// property is named beside it.
		for _, edit := range slices.Sorted(maps.Keys(theEditsOfACorpusBot)) {
			if cases[edit] == 0 {
				t.Errorf("%q materialised for none of the %d corpus files — a guard that cannot fail", edit, pairs)
			}
		}
		// Both answers are measured on the corpus: a test that only ever saw
		// the silent one would hold half the guarantee.
		if silent < 150 || named < 150 {
			t.Fatalf("%d silent cases and %d named: the corpus exercises one answer only", silent, named)
		}
	})
}
