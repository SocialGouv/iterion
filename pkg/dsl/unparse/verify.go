package unparse

import (
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
	if err := inexpressible(f); err != nil {
		return err
	}
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
		// Compare through firstJSONDifference rather than on the bytes: a
		// block the writer legitimately left out (droppableEmptyBlock) makes
		// the two encodings differ byte for byte while the documents are the
		// same, and a byte gate would refuse the save with an empty reason.
		if why := firstJSONDifference(a, b); why != "" {
			return fmt.Errorf("the serialised source is not the same document: %s", why)
		}
	}
	return nil
}

// inexpressible names a declaration the .bot syntax cannot write down. The
// syntax requires an indented body under every header, and three kinds have
// no property that means nothing: a group's body IS its nodes, a prompt's IS
// its text, a schema's IS its fields — any placeholder would BECOME content.
// (Every other declaration gets a no-op property from the writer; a nested
// block gets one too, except the two whose blank form means nothing at all
// and are left out.) Say which declaration by name, instead of letting the
// re-parse answer "expected INDENT, got EOF" about a file the author cannot
// see — the message reaches them as an HTTP 422 on a save they will have to
// undo by hand otherwise.
func inexpressible(f *ast.File) error {
	for _, g := range f.Groups {
		if len(g.Agents)+len(g.Judges)+len(g.Routers)+len(g.Humans)+len(g.Tools)+len(g.Computes)+len(g.Edges) == 0 {
			return fmt.Errorf("group %q is empty: the .bot syntax cannot express a group with no node — add a node to it or remove it", g.Name)
		}
	}
	for _, p := range f.Prompts {
		if strings.TrimSpace(p.Body) == "" {
			return fmt.Errorf("prompt %q has an empty body: the .bot syntax cannot express it — write the prompt text or remove the declaration", p.Name)
		}
	}
	for _, s := range f.Schemas {
		if len(s.Fields) == 0 {
			return fmt.Errorf("schema %q has no field: the .bot syntax cannot express it — add a field or remove the declaration", s.Name)
		}
	}
	return nil
}

// sourceFile is the file the document's prompts were read from, "" when it
// came through the JSON transport (no spans). Includes are the only thing
// a file name decides, and they live in prompts.
func sourceFile(f *ast.File) string {
	for _, p := range f.Prompts {
		if p.Span.Start.File != "" {
			return p.Span.Start.File
		}
	}
	return ""
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

// droppableEmptyBlock reports a block that was declared and left blank, and
// whose blank form the compiler reads exactly as it reads no block at all
// (`compaction:`, `memory:`, `vars:`, `resources:`, `secrets:`,
// `attachments:`, `presets:`). A header with no indented body does not parse,
// so the writer leaves such a block out; losing nothing is not a document
// that changed. A block that HELD something does not marshal to `{}`, so the
// tolerance cannot hide a real loss.
//
// `sandbox` is the exception and is named: a mode-less sandbox block also
// marshals to `{}`, but the compiler reads a DECLARED block as inline mode,
// so dropping it changes the node. It has no written form either — the block
// syntax always parses back as mode inline — so it must be refused here
// rather than tolerated.
func droppableEmptyBlock(key string, v any) bool {
	if key == "sandbox" {
		return false
	}
	m, ok := v.(map[string]any)
	return ok && len(m) == 0
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
				if droppableEmptyBlock(k, xe) {
					continue
				}
				return path + "." + k + " is missing after the round-trip"
			}
			if d := diffAny(path+"."+k, xe, ye); d != "" {
				return d
			}
		}
		for k, ye := range yv {
			if _, ok := xv[k]; !ok {
				if droppableEmptyBlock(k, ye) {
					continue
				}
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
