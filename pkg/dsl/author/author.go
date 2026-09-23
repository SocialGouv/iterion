package author

import (
	"sort"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Result is what Parse reads off an author document.
type Result struct {
	// File is the program the document describes, every position of it on
	// the YAML; never nil — an unreadable document leaves an empty file
	// beside the diagnostics that refuse it.
	File *ast.File
	// Diagnostics are the converter's and the parser's, positioned on the
	// document, in source order. A warning names what the .bot reads
	// otherwise than the author may have meant (a prompt body's blank
	// lines); an error refuses the document.
	Diagnostics []parser.Diagnostic
	// Text is the .bot text the document was read through: the spelling
	// the parser saw, kept for a test and for diagnosing a conversion.
	Text string
}

// HasErrors reports whether any diagnostic is an error.
func (r *Result) HasErrors() bool {
	for _, d := range r.Diagnostics {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}

// Parse reads an author document named name — the name every position of
// the AST and every diagnostic carries — and returns the program it
// describes: the YAML spelled into .bot text (spell.go), the text read by
// the one parser of the language, the positions mapped back.
func Parse(name string, src []byte) *Result {
	res := &Result{File: &ast.File{}}
	root, diags := decode(name, src)
	if root == nil {
		res.Diagnostics = diags
		return res
	}
	text, lines, sdiags, _ := spell(name, src, root)
	res.Text = text
	pr := parser.Parse(name, text)
	m := &mapper{name: name, lines: lines}
	m.spans(pr.File)
	res.File = pr.File
	dropDirective(res.File)
	res.Diagnostics = append(append(diags, sdiags...), m.diagnostics(pr.Diagnostics)...)
	sort.SliceStable(res.Diagnostics, func(i, j int) bool {
		a, b := res.Diagnostics[i], res.Diagnostics[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return res
}

// dropDirective removes the strict-escape directive the speller writes at
// the head of a profile-1 text: it is the spelling's, not the document's
// (the unparser decides its own when it writes the .bot).
func dropDirective(f *ast.File) {
	kept := f.Comments[:0]
	for _, c := range f.Comments {
		if parser.IsStrictEscapeDirective(c.Text) {
			continue
		}
		kept = append(kept, c)
	}
	f.Comments = kept
	if len(f.Comments) == 0 {
		f.Comments = nil
	}
}
