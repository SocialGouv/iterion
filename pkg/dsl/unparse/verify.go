package unparse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Verify reports whether text — the output of Unparse(f) — reads back as the
// program f: it parses without an error and compiles to the same workflow
// with the same diagnostics (ir.SameProgram). A caller that writes the text
// somewhere the program is later read from (the studio's save path) refuses
// on an error rather than write a file that means something else than the
// document it came from.
//
// Prompt bodies are compared in the lexer's canonical form
// (parser.CanonicalPromptBody): a canvas body with a paragraph break or a
// trailing newline has no other written form, and every reader of the file
// gets the canonical one — so that is the program the document IS, and the
// guard refuses only what would actually change. A body the syntax cannot
// carry at all (parser.CheckPromptBody: a first line indented deeper than a
// later one) IS such a change — its nearest written form de-indents it —
// and is refused by name before anything is compared.
func Verify(f *ast.File, text string) error {
	for _, p := range f.Prompts {
		if err := parser.CheckPromptBody(p.Body); err != nil {
			return fmt.Errorf("prompt %q cannot be written as .bot source: %v", p.Name, err)
		}
	}
	// A fallback route is written under its name; one with no name — the
	// canvas's route before it is named — has no written form, and the
	// writer leaves it out, which the comparison below would report as a
	// node that differs. Said by name instead.
	for _, a := range f.Agents {
		if err := checkFallbackNames(a.Fallbacks); err != nil {
			return fmt.Errorf("agent %q cannot be written as .bot source: %v", a.Name, err)
		}
	}
	for _, j := range f.Judges {
		if err := checkFallbackNames(j.Fallbacks); err != nil {
			return fmt.Errorf("judge %q cannot be written as .bot source: %v", j.Name, err)
		}
	}
	// A group's members go through the same writers.
	for _, g := range f.Groups {
		for _, a := range g.Agents {
			if err := checkFallbackNames(a.Fallbacks); err != nil {
				return fmt.Errorf("group %q, agent %q cannot be written as .bot source: %v", g.Name, a.Name, err)
			}
		}
		for _, j := range g.Judges {
			if err := checkFallbackNames(j.Fallbacks); err != nil {
				return fmt.Errorf("group %q, judge %q cannot be written as .bot source: %v", g.Name, j.Name, err)
			}
		}
	}
	f = canonicalPrompts(f)
	// The round-trip is parsed under the document's own source file, so an
	// {{include}} resolves — or is refused — on both sides alike. A document
	// from the JSON transport has no source file: naming one here made the
	// re-parse resolve its includes while the document itself could not,
	// and every bot with an include was refused at save.
	pr := parser.Parse(sourceFile(f), text)
	var errs []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			errs = append(errs, d.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("the serialised source does not parse: %s", strings.Join(errs, "; "))
	}
	// The profile is not program — the same AST compiles the same in
	// either — so SameProgram cannot see it lost: a document saved in the
	// wrong profile would read its strings and its prompts otherwise.
	if got, want := pr.File.EffectiveProfile(), f.EffectiveProfile(); got != want {
		return fmt.Errorf("the serialised source reads as dsl profile %d, the document is profile %d", got, want)
	}
	ca, cb := ir.Compile(f), ir.Compile(pr.File)
	if why := ir.SameProgram(ca, cb); why != "" {
		return fmt.Errorf("the serialised source is not the same program: %s", why)
	}
	if ca.Workflow == nil || cb.Workflow == nil {
		// No compiled program to compare — the shape of every half-authored
		// canvas document (no `workflow` yet). The AST mirror is span-free
		// and carries every declaration, so it is the oracle here; without
		// it the guard passed anything that did not compile. Comments are
		// not program: the writer moves them to the top and may add the
		// strict-escape directive, so they are left out.
		a, err := ast.MarshalFile(withoutComments(f))
		if err != nil {
			return fmt.Errorf("cannot compare the document: %w", err)
		}
		b, err := ast.MarshalFile(withoutComments(pr.File))
		if err != nil {
			return fmt.Errorf("cannot compare the serialised source: %w", err)
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("the serialised source is not the same document: %s", firstJSONDifference(a, b))
		}
	}
	return nil
}

// sourceFile is the file the document's prompts were read from, "" when it
// came through the JSON transport (no spans) — which is every production
// caller: both save handlers build the document with ast.UnmarshalFile.
// Includes are the only thing a file name decides, and they live in
// prompts, so the first prompt's file is the document's; a document whose
// prompts came from several files (a bundle's prompts/*.md merged beside
// a main.bot's) never reaches Verify.
func sourceFile(f *ast.File) string {
	for _, p := range f.Prompts {
		if p.Span.Start.File != "" {
			return p.Span.Start.File
		}
	}
	return ""
}

// checkFallbackNames reports the first fallback route with no name.
func checkFallbackNames(fbs []*ast.FallbackDecl) error {
	for i, fb := range fbs {
		if fb == nil || strings.TrimSpace(fb.Name) == "" {
			return fmt.Errorf("fallback route %d has no name; every route is written under its name", i+1)
		}
	}
	return nil
}

// canonicalPrompts is a shallow copy of f whose prompt bodies are in the
// lexer's canonical form — the form the writer emits and the re-parse
// yields. The document itself is left as it came.
func canonicalPrompts(f *ast.File) *ast.File {
	cp := *f
	cp.Prompts = make([]*ast.PromptDecl, len(f.Prompts))
	for i, p := range f.Prompts {
		q := *p
		q.Body = parser.CanonicalPromptBodyIn(f.EffectiveProfile(), p.Body)
		cp.Prompts[i] = &q
	}
	return &cp
}

// withoutComments is a shallow copy of f with its comment list dropped.
func withoutComments(f *ast.File) *ast.File {
	cp := *f
	cp.Comments = nil
	return &cp
}

// firstJSONDifference names the first key path at which two JSON documents
// diverge, so a refused save says which declaration did not survive.
func firstJSONDifference(a, b []byte) string {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return "the documents differ"
	}
	if d := diffAny("document", x, y); d != "" {
		return d
	}
	// The bytes differ but the decoded values do not (one side escaped a
	// rune the other wrote raw): still a refusal, never an empty reason.
	return "the documents differ in their encoding"
}

func diffAny(path string, x, y any) string {
	switch xv := x.(type) {
	case map[string]any:
		yv, ok := y.(map[string]any)
		if !ok {
			return path + " differs in kind"
		}
		for k, xe := range xv {
			ye, ok := yv[k]
			if !ok {
				return path + "." + k + " is missing after the round-trip"
			}
			if d := diffAny(path+"."+k, xe, ye); d != "" {
				return d
			}
		}
		for k := range yv {
			if _, ok := xv[k]; !ok {
				return path + "." + k + " appeared after the round-trip"
			}
		}
		return ""
	case []any:
		yv, ok := y.([]any)
		if !ok || len(xv) != len(yv) {
			return fmt.Sprintf("%s has a different length", path)
		}
		for i := range xv {
			if d := diffAny(fmt.Sprintf("%s[%d]", path, i), xv[i], yv[i]); d != "" {
				return d
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(x, y) {
			return fmt.Sprintf("%s: %v became %v", path, x, y)
		}
		return ""
	}
}
