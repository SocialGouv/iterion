package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const namesSeed = `schema io:
  text: string

agent review:
  model: "m"
  input: io
  output: io

workflow main:
  entry: review
  review -> review as fix_loop(3)
  review -> done
`

func parseNamesSeed(t *testing.T) *ast.File {
	t.Helper()
	res, err := parser.Parse("seed.bot", namesSeed)
	if err != nil || res.File == nil {
		t.Fatalf("seed does not parse: %v", err)
	}
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("seed does not parse: %v", d)
		}
	}
	return res.File
}

// A name a studio field took with a space, a hyphen or a quote has no
// written form: the save is refused naming it and where, with a spelling
// that works — not with the re-read's parse errors at columns of a line the
// author never wrote.
func TestVerify_RefusesANameTheSyntaxDoesNotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(f *ast.File)
		want []string
	}{
		{"a loop name with a space", func(f *ast.File) { f.Workflows[0].Edges[0].Loop.Name = "fix loop" },
			[]string{`workflow main, edge review -> review, loop "fix loop"`, `"fix_loop"`}},
		{"an agent name with a hyphen", func(f *ast.File) { f.Agents[0].Name = "code-review" },
			[]string{`agent "code-review"`, `"code_review"`}},
		{"a schema field with a space", func(f *ast.File) { f.Schemas[0].Fields[0].Name = "user text" },
			[]string{`schema io field "user text"`, `"user_text"`}},
		{"a workflow name starting with a digit", func(f *ast.File) { f.Workflows[0].Name = "2nd" },
			[]string{`workflow "2nd"`, `"_2nd"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := parseNamesSeed(t)
			tc.edit(f)
			err := Verify(f, Unparse(f))
			if err == nil {
				t.Fatal("Verify accepted a name the syntax does not read")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Verify = %v\nwant it to say %s", err, w)
				}
			}
			if strings.Contains(err.Error(), "does not parse") {
				t.Errorf("Verify fell through to the re-read's parse errors: %v", err)
			}
		})
	}
}

// A name in any letters the lexer reads — not only ASCII — is a name.
func TestVerify_KeepsUnicodeNames(t *testing.T) {
	f := parseNamesSeed(t)
	f.Workflows[0].Edges[0].Loop.Name = "цикл_правок"
	if err := Verify(f, Unparse(f)); err != nil {
		t.Fatalf("Verify refused a unicode loop name: %v", err)
	}
}

func TestSuggestName(t *testing.T) {
	for in, want := range map[string]string{
		"fix loop":    "fix_loop",
		"user-input":  "user_input",
		`"quoted"`:    "_quoted_",
		"2nd":         "_2nd",
		"ok_name":     "ok_name",
		"":            "_",
		"цикл правок": "цикл_правок",
	} {
		if got := suggestName(in); got != want {
			t.Errorf("suggestName(%q) = %q, want %q", in, got, want)
		}
	}
}
