package canon

import (
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/rewrite"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// Document is the canonical form of an author document (pkg/dsl/author, the
// YAML twin of a .bot) named name: the program it describes, written back
// by the author writer, proven to read as the same program before it is
// handed back — the .bot text of the document and of its written form is
// one — on the document's own bytes, its BOM and its line endings kept as
// Bytes keeps a .bot's. Refused (ErrRefused), the bytes left the author's:
// a document that does not read; one the .bot reads otherwise than it was
// written — a prompt body the lexer settles, a line separator in a block
// scalar, the warnings author.Parse reports (E053) — since the writer would
// put that reading in the author's place; one that carries YAML comments,
// which the writer keeps none of; one whose written form reads as another
// program.
func Document(name string, src []byte) ([]byte, error) {
	norm := rewrite.Normalize(src)
	text := []byte(norm.Text)
	res := author.Parse(name, text)
	if errs := diagnosticsOf(res.Diagnostics, parser.SeverityError); len(errs) > 0 {
		return nil, fmt.Errorf("%w: does not read: %s", ErrRefused, strings.Join(errs, "; "))
	}
	if read := diagnosticsOf(res.Diagnostics, parser.SeverityWarning); len(read) > 0 {
		return nil, fmt.Errorf("%w: the .bot reads it otherwise than it is written, and a rewrite would put that reading in the author's place: %s", ErrRefused, strings.Join(read, "; "))
	}
	if comments := author.Comments(text); len(comments) > 0 {
		return nil, fmt.Errorf("%w: carries %d YAML comment line(s) the writer does not keep (the first: %s)", ErrRefused, len(comments), comments[0])
	}
	out, err := author.Write(res.File)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot be written as a document: %v", ErrRefused, err)
	}
	written := string(out)
	if written == norm.Text {
		return src, nil
	}
	// The proof before the write: the written document reads as the same
	// program — the .bot text of the two is one.
	back := author.Parse(name, out)
	if back.HasErrors() || unparse.Unparse(back.File) != unparse.Unparse(res.File) {
		return nil, fmt.Errorf("%w: its written form reads as another program", ErrRefused)
	}
	// The whole text is one edit, mapped back onto the original bytes: the
	// BOM stays, and every line ending follows the file's own.
	return norm.MapBack(src, []rewrite.Edit{{Start: 0, End: len(norm.Text), Repl: written}}), nil
}

// diagnosticsOf renders the diagnostics of one severity, in order.
func diagnosticsOf(diags []parser.Diagnostic, severity parser.Severity) []string {
	var out []string
	for _, d := range diags {
		if d.Severity == severity {
			out = append(out, d.Error())
		}
	}
	return out
}
