package author

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/internal/docfences"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// docsRoot is the repository root, seen from this package.
var docsRoot = filepath.Join("..", "..", "..")

// authorFencePolicy reads the info string of a ```yaml fence: an author
// document is tagged `author` — alone for a document that must read and
// compile clean, `author invalid` for an anti-example refused with any
// error, `author invalid:<CODE>` for one refused with that code. Any other
// ```yaml fence is other YAML (a manifest, a CI file) and is not read; a tag
// that starts like `author` and is none of these is a typo, reported.
func authorFencePolicy(info string) (policy string, isAuthor bool, err error) {
	switch {
	case info == "author":
		return "", true, nil
	case info == "author invalid" || strings.HasPrefix(info, "author invalid:"):
		return strings.TrimPrefix(info, "author "), true, nil
	case strings.HasPrefix(info, "author"):
		return "", false, fmt.Errorf("unknown author fence tag %q — use `yaml author`, `yaml author invalid` or `yaml author invalid:<CODE>`", info)
	}
	return "", false, nil
}

// documentFindings reads a document and, when it reads, compiles the program
// it describes: the error diagnostics of both, each with its code. C018 (no
// model and no backend) is waived as the ir guard waives it: whether the
// host can auto-detect a backend is the one compile check that reads the
// environment.
func documentFindings(name, body string) []string {
	res := Parse(name, []byte(body))
	var errs []string
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if len(errs) > 0 || len(res.File.Workflows) == 0 {
		return errs
	}
	for _, d := range ir.Compile(res.File).Diagnostics {
		if d.Severity == ir.SeverityError && d.Code != ir.DiagMissingModelOrBackend {
			errs = append(errs, d.Error())
		}
	}
	return errs
}

// Every ```yaml author fence of the documentation is a document a reader
// copies: read by the converter and compiled clean — or refused as its tag
// says. The count is reported, and a documentation that stopped carrying
// author fences fails, as the ir guard fails on no ```iter fence.
func TestDocsAuthorFencesRead(t *testing.T) {
	files, err := docfences.Files(docsRoot)
	if err != nil {
		t.Fatal(err)
	}
	documents, antiExamples := 0, 0
	for _, path := range files {
		fences, err := docfences.Extract(path, "yaml")
		if err != nil {
			t.Errorf("%v", err)
		}
		for _, f := range fences {
			policy, isAuthor, err := authorFencePolicy(f.Info)
			rel := strings.TrimPrefix(f.File, docsRoot+string(filepath.Separator))
			where := fmt.Sprintf("%s:%d fence `yaml %s`", rel, f.Line, f.Info)
			if err != nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			if !isAuthor {
				continue
			}
			if f.Malformed != "" {
				t.Errorf("%s: %s", where, f.Malformed)
				continue
			}
			errs := documentFindings(filepath.Base(f.File)+".bot.yaml", f.Body)
			switch policy {
			case "":
				documents++
				if len(errs) > 0 {
					t.Errorf("%s: the document does not read and compile clean:\n      %s", where, strings.Join(errs, "\n      "))
					continue
				}
				// A document a reader copies is one `iterion fmt` would not
				// rewrite: the canonical form, as the writer puts it.
				if out, err := Write(Parse(filepath.Base(f.File)+".bot.yaml", []byte(f.Body)).File); err != nil || string(out) != f.Body {
					t.Errorf("%s: the document is not in its canonical form (%v); `iterion fmt` writes it:\n%s", where, err, out)
				}
			case "invalid":
				antiExamples++
				if len(errs) == 0 {
					t.Errorf("%s: is tagged as an anti-example and reads clean", where)
				}
			default:
				antiExamples++
				code := strings.TrimPrefix(policy, "invalid:")
				if !strings.Contains(strings.Join(errs, "\n"), "["+code+"]") {
					t.Errorf("%s: is tagged as the %s anti-example and is not refused with it:\n      %s", where, code, strings.Join(errs, "\n      "))
				}
			}
		}
	}
	t.Logf("%d author documents and %d anti-examples in the documentation", documents, antiExamples)
	if documents == 0 {
		t.Fatal("no ```yaml author fence in the documentation — the extractor or the tags are broken")
	}
}

// Every ```iter fence of the documentation that stands alone — a program,
// which the ir guard parses and compiles clean — or declares top-level
// declarations (`fragment`) round-trips through the author document as the
// corpus does: written as YAML, read back, the same AST once comments are
// set aside, and a program the same once compiled. A fence the writer
// refuses by name (two workflows in one fence) is counted apart, never
// compared. Counted apart from the corpus's round trip.
func TestDocsIterFencesRoundTripThroughTheAuthorDocument(t *testing.T) {
	files, err := docfences.Files(docsRoot)
	if err != nil {
		t.Fatal(err)
	}
	programs, fragments, refused := 0, 0, 0
	for _, path := range files {
		fences, err := docfences.Extract(path, "iter")
		if err != nil {
			continue // the ir guard reports an unclosed fence
		}
		for _, f := range fences {
			if f.Malformed != "" || (f.Info != "" && f.Info != "fragment") {
				continue
			}
			rel := strings.TrimPrefix(f.File, docsRoot+string(filepath.Separator))
			where := fmt.Sprintf("%s:%d fence `iter %s`", rel, f.Line, f.Info)
			name := filepath.Join(filepath.Dir(f.File), fmt.Sprintf("fence-%d.bot", f.Line))
			pr := parser.Parse(name, f.Body)
			if hasParseErrors(pr.Diagnostics) {
				continue // the ir guard reports it
			}
			out, err := Write(pr.File)
			if err != nil {
				// One refusal is the writer's by design: a document holds
				// one workflow. Any other is a fence the corpus would write
				// and this one does not.
				if !strings.Contains(err.Error(), "the document holds one workflow") {
					t.Errorf("%s: the writer refuses it: %v", where, err)
					continue
				}
				refused++
				t.Logf("%s: the writer refuses it by name: %v", where, err)
				continue
			}
			res := Parse(name+".yaml", out)
			if errs := errorsOf(res); len(errs) > 0 {
				t.Errorf("%s: the written document is refused:\n  %s\n--- written:\n%s", where, strings.Join(errs, "\n  "), out)
				continue
			}
			if why := sameProgramASTModuloComments(pr.File, res.File); why != "" {
				t.Errorf("%s: not the same AST after the round trip: %s\n--- written:\n%s", where, why, out)
				continue
			}
			if f.Info == "" {
				programs++
				if why := ir.SameProgram(ir.Compile(pr.File), ir.Compile(res.File)); why != "" {
					t.Errorf("%s: not the same program once compiled: %s\n--- written:\n%s", where, why, out)
				}
			} else {
				fragments++
			}
		}
	}
	t.Logf("docs fences round-tripped: %d programs, %d fragments; %d refused by the writer by name", programs, fragments, refused)
	if programs == 0 || fragments == 0 {
		t.Fatalf("the bench compared %d programs and %d fragments — the fences are not being read", programs, fragments)
	}
}
