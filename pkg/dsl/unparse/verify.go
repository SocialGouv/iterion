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
// It answers that question about the document AS GIVEN: f is expected to be
// span-free, the shape every caller has (the JSON transport carries no
// spans). A caller that hands it a file parsed from disk asks a different
// question — see the re-parse below.
func Verify(f *ast.File, text string) error {
	// No source file: f came through the JSON transport with no spans, so
	// ir.Compile refuses its {{include}} markers. Naming a file here would
	// make the re-parse resolve them instead, and the two sides could never
	// agree on a document that uses one. Only include resolution and
	// diagnostic positions read this name, and SameProgram compares codes.
	pr := parser.Parse("", text)
	var errs []string
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			// The empty file name above leaves a leading colon on the
			// position; the refusal reads "12:3: error …", a line:column in
			// the text the caller is about to write, which is the file it
			// names anyway.
			errs = append(errs, strings.TrimPrefix(d.Error(), ":"))
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
