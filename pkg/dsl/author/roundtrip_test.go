package author

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/dsltest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// Every .bot of the repository round-trips through the author document:
// parsed, written as YAML, read back, it is the same program — the same
// AST once positions and the comments the document does not carry are set
// aside, the same catalog identity, and, compiled in its unit with the
// fragments its imports name, the same workflow and the same diagnostic
// codes (ir.SameProgram). The written document is a fixpoint of the
// writer. A divergence is a defect of the writer or of the converter,
// never an acceptable difference; the counts are reported so a bench that
// converted nothing cannot pass green.
func TestCorpusRoundTripsThroughTheAuthorDocument(t *testing.T) {
	rel := filepath.Join("..", "..", "..")
	files := dsltest.CorpusFiles(t, rel)
	converted, skipped := 0, 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		pr := parser.Parse(path, string(src))
		if hasParseErrors(pr.Diagnostics) {
			skipped++ // not a program: nothing to round-trip
			continue
		}
		converted++
		name := filepath.ToSlash(strings.TrimPrefix(path, rel+string(filepath.Separator)))
		t.Run(name, func(t *testing.T) {
			out, err := Write(pr.File)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			res := Parse(path+".yaml", out)
			if errs := errorsOf(res); len(errs) > 0 {
				t.Fatalf("the written document is refused:\n  %s\n--- written:\n%s", strings.Join(errs, "\n  "), out)
			}
			if why := sameProgramASTModuloComments(pr.File, res.File); why != "" {
				t.Fatalf("not the same AST after the round trip: %s\n--- written:\n%s", why, out)
			}
			// The catalog identity, read by the reader the catalogue uses
			// (bundle.ParseFrontmatter) off the original text and off the
			// .bot the document writes back through the unparser — the
			// `fmt --to bot` path — never through the writer's own reading.
			if a, b := catalogText(src), catalogText([]byte(unparse.Unparse(res.File))); a != b {
				t.Fatalf("the catalog identity changed:\n  before: %s\n  after:  %s\n--- written:\n%s", a, b, out)
			}
			direct := unit.LoadDir(path)
			via := unit.LoadDirWithMainAST(path, path+".yaml", res.File, out)
			if len(direct.Files) != len(via.Files) {
				t.Fatalf("the unit loaded %d files directly and %d through the document\n--- written:\n%s", len(direct.Files), len(via.Files), out)
			}
			if a, b := diagnosticCodes(direct.Diagnostics), diagnosticCodes(via.Diagnostics); a != b {
				t.Fatalf("the unit's diagnostics differ: %s directly, %s through the document\n--- written:\n%s", a, b, out)
			}
			if why := ir.SameProgram(ir.Compile(direct.Merged), ir.Compile(via.Merged)); why != "" {
				t.Fatalf("not the same program once compiled in its unit: %s\n--- written:\n%s", why, out)
			}
			if direct.Merged != nil && len(direct.Merged.Workflows) > 0 && !direct.HasErrors() {
				if cr := ir.Compile(via.Merged); cr.Workflow == nil {
					t.Fatalf("the original compiles to a workflow and the round trip does not\n--- written:\n%s", out)
				}
			}
			twice, err := Write(res.File)
			if err != nil {
				t.Fatalf("Write again: %v", err)
			}
			if string(twice) != string(out) {
				t.Fatalf("the writer's text is not a fixpoint:\n--- first:\n%s\n--- second:\n%s", out, twice)
			}
		})
	}
	t.Logf("%d .bot files round-tripped, %d skipped for parse errors", converted, skipped)
	if converted < 50 {
		t.Fatalf("only %d files round-tripped — the bench is not exercising the corpus", converted)
	}
}

func hasParseErrors(diags []parser.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}

// sameProgramASTModuloComments is sameProgramAST with every Comments
// field cleared and the inline prompts sorted by name: a .bot's comments
// do not travel (the frontmatter is compared apart), and the inline
// prompts of a file are named after their bodies, an order that says
// nothing.
func sameProgramASTModuloComments(a, b *ast.File) string {
	ca, cb := zeroPositions(a), zeroPositions(b)
	clearComments(reflect.ValueOf(ca))
	clearComments(reflect.ValueOf(cb))
	sortInlinePrompts(ca)
	sortInlinePrompts(cb)
	if reflect.DeepEqual(ca, cb) {
		return ""
	}
	return firstDifference(reflect.ValueOf(ca), reflect.ValueOf(cb), "File")
}

func clearComments(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			clearComments(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if f.Name == "Comments" && v.Field(i).Kind() == reflect.Slice {
				if v.Field(i).CanSet() {
					v.Field(i).Set(reflect.Zero(v.Field(i).Type()))
				}
				continue
			}
			clearComments(v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := 0; i < v.Len(); i++ {
			clearComments(v.Index(i))
		}
	}
}

func sortInlinePrompts(f *ast.File) {
	var declared, inline []*ast.PromptDecl
	for _, p := range f.Prompts {
		if p.Inline {
			inline = append(inline, p)
		} else {
			declared = append(declared, p)
		}
	}
	sort.Slice(inline, func(i, j int) bool { return inline[i].Name < inline[j].Name })
	f.Prompts = append(declared, inline...)
	if len(f.Prompts) == 0 {
		f.Prompts = nil
	}
}

// catalogText is the catalog identity a .bot text carries, read by the
// catalogue's own reader, rendered for a comparison.
func catalogText(src []byte) string {
	fm := bundle.ParseFrontmatter(src)
	if fm == nil {
		return "(none)"
	}
	return fmt.Sprintf("name=%q description=%q triggers=%q capabilities=%q", fm.Name, fm.Description, fm.Triggers, fm.Capabilities)
}

// diagnosticCodes renders a unit's diagnostics as their sorted codes —
// positions aside, the two units' coordinates being different files.
func diagnosticCodes(diags []parser.Diagnostic) string {
	codes := make([]string, 0, len(diags))
	for _, d := range diags {
		codes = append(codes, string(d.Code))
	}
	sort.Strings(codes)
	return strings.Join(codes, ",")
}
