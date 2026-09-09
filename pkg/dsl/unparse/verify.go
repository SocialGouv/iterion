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
func Verify(f *ast.File, text string) error {
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
// came through the JSON transport (no spans) — and "" too when the prompts
// disagree (a bundle's prompts/*.md merged beside a main.bot's): one name
// would resolve some prompt's include against the wrong directory, and
// the refusal is the same on both sides. Includes are the only thing a
// file name decides, and they live in prompts.
func sourceFile(f *ast.File) string {
	file := ""
	for _, p := range f.Prompts {
		switch {
		case p.Span.Start.File == "":
		case file == "":
			file = p.Span.Start.File
		case p.Span.Start.File != file:
			return ""
		}
	}
	return file
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
	return diffAny("document", x, y)
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
