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
// Bytes keeps a .bot's. The document's YAML comments ride the nodes they
// hang on (author.WriteKeepingComments). Refused (ErrRefused), the bytes
// left the author's: a document that does not read; one the .bot reads
// otherwise than it was written — a prompt body the lexer settles, a line
// separator in a block scalar, the warnings author.Parse reports (E053) —
// since the writer would put that reading in the author's place; one
// opening a stream with a `%YAML`/`%TAG` directive, which the writer does
// not emit; one whose written form would lose a comment, or move one on a
// second pass; one whose written form reads as another program.
//
// The explicit document markers the author wrote come back: a `---` opening
// the document and the trailing region below its last content line — the
// `...` markers and the lines written around them — are re-emitted off the
// source by the writer, so a document carrying one is rewritten rather than
// losing its marker in silence.
func Document(name string, src []byte) ([]byte, error) {
	norm := rewrite.Normalize(src)
	text := []byte(norm.Text)
	res := author.Parse(name, text)
	if errs := diagnosticsOf(res.Diagnostics, parser.SeverityError); len(errs) > 0 {
		return nil, fmt.Errorf("%w: does not read: %s", ErrRefused, strings.Join(errs, "; "))
	}
	// A `%YAML`/`%TAG` directive opens a STREAM; the writer renders one
	// document and emits none, so a rewrite would drop the line the author
	// wrote. Refused, naming it, rather than dropped in silence: the
	// explicit markers of the document itself — a `---` opening it, a `...`
	// closing it — ARE re-emitted (author.WriteKeepingComments), and §5 of
	// ADR-102 holds the rest to a refusal.
	if ds := author.Directives(text); len(ds) > 0 {
		return nil, fmt.Errorf("%w: its written form would lose the YAML stream directive the author wrote, which the author writer does not emit: %s", ErrRefused, strings.Join(ds, "; "))
	}
	// A ` #` ending a plain value is the one reading a rewrite keeps as the
	// author wrote it — the comment rides the value — so it refuses only if
	// the written form would lose it (checked on the written form below).
	if read := diagnosticsOf(otherReadings(res.Diagnostics), parser.SeverityWarning); len(read) > 0 {
		return nil, fmt.Errorf("%w: the .bot reads it otherwise than it is written, and a rewrite would put that reading in the author's place: %s", ErrRefused, strings.Join(read, "; "))
	}
	out, err := author.WriteKeepingComments(res.File, text)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot be written as a document: %v", ErrRefused, err)
	}
	written := string(out)
	if written == norm.Text {
		return src, nil
	}
	if why := commentsMoved(author.Comments(text), author.Comments(out)); why != "" {
		return nil, fmt.Errorf("%w: its written form would not keep its YAML comments: %s", ErrRefused, why)
	}
	// The proof before the write: the written document reads as the same
	// program — the .bot text of the two is one.
	back := author.Parse(name, out)
	if errs := diagnosticsOf(back.Diagnostics, parser.SeverityError); len(errs) > 0 {
		return nil, fmt.Errorf("%w: its written form does not read back: %s", ErrRefused, strings.Join(errs, "; "))
	}
	if why := commentReadingLost(res.CommentReadings, back.CommentReadings); why != "" {
		return nil, fmt.Errorf("%w: the .bot reads it otherwise than it is written, and its written form would put that reading in the author's place: %s", ErrRefused, why)
	}
	if why := firstDifferentLine(unparse.Unparse(res.File), unparse.Unparse(back.File)); why != "" {
		return nil, fmt.Errorf("%w: its written form reads as another program (%s)", ErrRefused, why)
	}
	// The written form is its own canonical form: a comment the writer put
	// where yaml.v3 reads it back on another node would move again on the
	// next pass, and `fmt --check` would never be green on the result.
	again, err := author.WriteKeepingComments(back.File, out)
	if err != nil {
		return nil, fmt.Errorf("%w: its written form cannot be written again: %v", ErrRefused, err)
	}
	if string(again) != written {
		return nil, fmt.Errorf("%w: its written form would not keep its YAML comments where they are: written again, it moves them (%s)", ErrRefused, firstDifferentLine(written, string(again)))
	}
	// The whole text is one edit, mapped back onto the original bytes: the
	// BOM stays, and every line ending follows the file's own.
	return norm.MapBack(src, []rewrite.Edit{{Start: 0, End: len(norm.Text), Repl: written}}), nil
}

// otherReadings is diags without the comment readings (author.IsCommentReading).
func otherReadings(diags []parser.Diagnostic) []parser.Diagnostic {
	var out []parser.Diagnostic
	for _, d := range diags {
		if !author.IsCommentReading(d) {
			out = append(out, d)
		}
	}
	return out
}

// commentReadingLost names the first comment reading of the source the
// written form does not read as well — the value it reads, with the
// comment cutting it, gone or changed — "" when the written form keeps
// every one. The two are every reading of each (author.Result's
// CommentReadings), never the reported ones: a report keeps the first
// thousand in its document's order, and the writer orders the nodes.
func commentReadingLost(src, written []parser.Diagnostic) string {
	kept := map[string]int{}
	for _, d := range written {
		kept[d.Message]++
	}
	for _, d := range src {
		if kept[d.Message] == 0 {
			return d.Error()
		}
		kept[d.Message]--
	}
	return ""
}

// commentsMoved names the first YAML comment line the written document
// does not carry as often as the source — lost, or written twice — "" when
// the two carry the same lines. Order is not compared: a comment rides its
// node, and the writer orders the nodes.
func commentsMoved(src, written []string) string {
	count := map[string]int{}
	for _, c := range src {
		count[c]++
	}
	for _, c := range written {
		count[c]--
	}
	for _, c := range src {
		if count[c] > 0 {
			return fmt.Sprintf("%d of %d comment line(s) would be lost (the first: %s)", lostCount(count), len(src), c)
		}
	}
	for _, c := range written {
		if count[c] < 0 {
			return fmt.Sprintf("a comment line would be written twice (%s)", c)
		}
	}
	return ""
}

// lostCount is the number of comment lines a count left positive.
func lostCount(count map[string]int) int {
	n := 0
	for _, k := range count {
		if k > 0 {
			n += k
		}
	}
	return n
}

// firstDifferentLine names the first line where the .bot texts a and b
// differ, "" when they are one.
func firstDifferentLine(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) || i < len(lb); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			return fmt.Sprintf("line %d of its .bot text: %q, read back as %q", i+1, x, y)
		}
	}
	return ""
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
