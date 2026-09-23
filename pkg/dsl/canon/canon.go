// Package canon gives a `.bot` file its canonical form: the text the studio
// saves (pkg/dsl/unparse), proven to read as the same program AND to carry
// the same comments before it is handed back (unparse.Verify), written on
// the file's own bytes — its BOM and its line endings kept — and refused by
// name when the writer cannot produce it without changing the file.
package canon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/internal/rewrite"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// ErrRefused marks a file that cannot be given its canonical form without
// changing it; the error says why.
var ErrRefused = errors.New("canonical form refused")

// Bytes is the canonical form of src, the file named name: the same bytes
// when the file already has it, else the writer's text mapped back onto the
// file's own BOM and line endings. A refusal (ErrRefused) leaves the file
// as it is: one that does not parse, or one the proof refuses — which now
// includes a comment the round trip would lose or move off its
// declaration, since the writer puts each one back where it was written
// and unparse.Verify compares them (#1282).
func Bytes(name string, src []byte) ([]byte, error) {
	norm := rewrite.Normalize(src)
	pr := parser.Parse(name, norm.Text)
	var errs []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%w: does not parse: %s", ErrRefused, strings.Join(errs, "; "))
	}
	text, err := Text(name, pr.File, []byte(norm.Text))
	if err != nil {
		if errors.Is(err, ErrRefused) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: cannot be rewritten without changing the program: %v", ErrRefused, err)
	}
	if text == norm.Text {
		return src, nil
	}
	// The whole text is one edit, mapped back onto the original bytes: the
	// BOM stays, and every line ending follows the file's own.
	return norm.MapBack(src, []rewrite.Edit{{Start: 0, End: len(norm.Text), Repl: text}}), nil
}

// Text is the writer's text for f, proven to read as the same program and
// to carry the same comments (unparse.Verify), and refused (ErrRefused)
// when the writer would take the multi-line form away from a value f holds
// over several lines (#1612). It is what every path that turns a document
// into the bytes of a `.bot` goes through — `iterion fmt` through Bytes,
// and the studio's saves — so the one guarantee is written once.
//
// src is the current text of that file, and the refusal is a statement
// ABOUT IT: folding is a before/after property, and a value its author
// already wrote as `"a\nb"` comes back on one line having lost nothing. No
// src, no before, nothing claimed.
//
// A verify failure comes back unwrapped: the caller says what it was doing
// when it could not render the document, and only the fold is a refusal of
// this file as it stands. The caller also supplies the remedy, since what
// to do differs between a path about to WRITE the file and one displaying
// it.
func Text(name string, f *ast.File, src []byte) (string, error) {
	text := unparse.Unparse(f)
	if err := unparse.Verify(f, text); err != nil {
		return "", err
	}
	if line, size, folds := Folds(name, string(src), text); folds {
		return "", fmt.Errorf("%w: the value at line %d is written over several lines and the writer has no form for one — its %d characters would come back as a single line (#1612)", ErrRefused, line, size)
	}
	return text, nil
}

// Folds reports whether replacing stored by incoming takes away the
// multi-line form of a value stored writes over several lines (#1612), and
// where stored writes the first of them. Asked either of a render (Text)
// or of two texts somebody else produced — a write route that receives
// file CONTENT rather than a document.
//
// The writer has ONE multi-line form, the backtick raw string, and it
// abandons that form for the WHOLE file the moment a single value needs
// the strict escape (unparse.str: a backtick together with a quote, a
// carriage return, a newline inside a group body — and every value under
// `## strict-escape`). So a render is all or nothing: one that folds
// anything has no value left over its lines at all. That is what is asked
// here — incoming folds something AND keeps nothing spread — and against a
// render it is exact, which is why every spread value of stored folds and
// naming the first of them is right.
//
// Against text an author wrote by hand the same question is a tight
// approximation rather than a proof: such a file may hold an escaped
// `"a\nb"` on one line beside a value over its lines, and it is precisely
// by keeping one spread that it says it did not fold. What it does not
// catch is an author folding one value while spreading another in the same
// write; what it will not do is refuse a file its own bytes back, or
// refuse a declaration being deleted.
func Folds(name, stored, incoming string) (line, size int, folds bool) {
	if !foldsSomething(name, incoming) {
		return 0, 0, false
	}
	if _, _, after := spreadValues(name, incoming); after > 0 {
		return 0, 0, false
	}
	line, size, before := spreadValues(name, stored)
	if before == 0 {
		return 0, 0, false
	}
	return line, size, true
}

// spreadValues counts the values text writes over SEVERAL lines, and gives
// the line and length of the first.
func spreadValues(name, text string) (line, size, n int) {
	for _, t := range parser.NewLexer(name, text).All() {
		if t.Type != parser.TokenString || t.EndLine <= t.Line {
			continue
		}
		if n == 0 {
			line, size = t.Line, t.End-t.Offset
		}
		n++
	}
	return line, size, n
}

// foldsSomething reports whether text writes a value holding a newline on
// ONE line.
func foldsSomething(name, text string) bool {
	for _, t := range parser.NewLexer(name, text).All() {
		if t.Type == parser.TokenString && strings.Contains(t.Value, "\n") && t.EndLine == t.Line {
			return true
		}
	}
	return false
}
