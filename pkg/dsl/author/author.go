package author

import (
	"errors"
	"fmt"
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
	// CommentReadings is every comment reading of the document
	// (IsCommentReading), reported or not: Diagnostics reports at most
	// parser.MaxReported of them, and a comparison of two documents' —
	// the canonical form's proof that its written form keeps each — needs
	// them all. One a value at most, so the read budget bounds them.
	CommentReadings []parser.Diagnostic
	// Spent is what reading the .bot the document spells took of the read
	// budget: the files the document imports share what is left of it
	// (unit.LoadDirWithMainAST).
	Spent int
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
	root, diags, readings := decode(name, src)
	if root == nil {
		res.Diagnostics = diags
		return res
	}
	res.CommentReadings = readings
	// On a document that reads, decode's second return is its warnings,
	// not refusals — they ride beside the converter's and the parser's.
	text, lines, sdiags, _ := spell(name, src, root)
	res.Text = text
	m := &mapper{name: name, lines: lines}
	var pdiags []parser.Diagnostic
	pr, err := parser.Parse(name, text)
	var refusal *parser.ReadRefusal
	switch {
	case errors.As(err, &refusal):
		// The .bot the document spells is past a bound of its reader: no
		// program is read off it — File stays the empty file — and the
		// refusal is the document's, with the document's remedy: the
		// author wrote YAML, and `import`, the .bot's way to split, is not
		// a key of it.
		pdiags = []parser.Diagnostic{documentRefusal(m.diagnostics([]parser.Diagnostic{refusal.Diagnostic})[0], refusal.Bound, len(text))}
	case err != nil:
		panic(fmt.Sprintf("author: parser.Parse failed with %v, which is no read refusal", err))
	default:
		res.Spent = pr.Spent
		m.spans(pr.File)
		res.File = pr.File
		dropDirective(res.File)
		pdiags = m.diagnostics(pr.Diagnostics)
	}
	res.Diagnostics = append(append(diags, sdiags...), pdiags...)
	sort.SliceStable(res.Diagnostics, func(i, j int) bool {
		a, b := res.Diagnostics[i], res.Diagnostics[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return res
}

// documentRefusal is the parser's refusal of the whole spelled text, said
// of the document: past the read budget, the reader's own words and the
// document's remedy; past the size cap, the size of the spelling, which a
// document under the cap can reach — a quoted value spelled with its
// escapes is longer than the document wrote it.
func documentRefusal(d parser.Diagnostic, bound parser.Bound, size int) parser.Diagnostic {
	switch bound {
	case parser.BoundBudget:
		d.Hint = docReadBudgetHint
	case parser.BoundSize:
		d.Message = fmt.Sprintf("the .bot text the document spells exceeds the maximum size (%d bytes > %d)", size, maxSourceSize)
		d.Hint = docSizeHint
	}
	return d
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
