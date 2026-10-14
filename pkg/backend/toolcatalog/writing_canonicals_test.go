package toolcatalog

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The file-writing concepts, pinned by name.
//
// WritingCanonicals is read by the surfaces its own godoc enumerates — the
// withholding, the operator notice, both corpus rules and the argv
// vocabulary — and they must not disagree about a word. Growing it WIDENS
// them all at once: a concept added here turns every shipped node that
// declares the tool into an offender, so the set is typed here before it can
// move, and a member deleted from it reddens instead of silently narrowing
// both corpus rules.
//
// `bash` is the member that must never be added: `readonly:` keeps the shell
// on claude_code by design, so reading it as a declared writer would refuse
// the read-only nodes the flag leaves a shell to.
func TestTheFileWritingConceptsArePinned(t *testing.T) {
	want := []string{"write", "edit", "notebookedit"}
	got := WritingCanonicals()
	if !slices.Equal(got, want) {
		t.Errorf("WritingCanonicals() = %v, want %v: the set the readonly withholding and both corpus rules read moved, so what the catalogue's prompts and the docs say about this flag moves in the same change, or they are lies", got, want)
	}
	for _, name := range []string{"bash", "read", "glob", "grep", "ls", "agent"} {
		if slices.Contains(got, name) {
			t.Errorf("%q is a member of WritingCanonicals() (%v): the flag leaves that tool to a read-only node, so every shipped node declaring it would become an offender of a rule it does not break", name, got)
		}
	}
	// Each member is a canonical of the table, or no spelling can reach it
	// and the withholding projects nothing.
	for _, member := range got {
		if c := CanonicalToolName(member); c != member {
			t.Errorf("writing concept %q is not a canonical of the shared table (→ %q): no `tools:` spelling folds onto it, so it withholds nothing", member, c)
		}
		if _, ok := CanonicalRows()[member]; !ok {
			t.Errorf("writing concept %q has no row in the shared table: the fold that classifies a declared writer cannot reach it", member)
		}
	}
	// Copied, not the package's own slice: a consumer that appended to the
	// returned value would grow the set for the withholding and both corpus
	// rules at once.
	WritingCanonicals()[0] = "mutated"
	if again := WritingCanonicals(); !slices.Equal(again, want) {
		t.Errorf("WritingCanonicals() = %v after a caller wrote to a previous result, want %v: the set is shared state", again, want)
	}
}

// writingCanonicalsName is the identifier the readers call. The walk below
// prefilters files on these bytes, so a rename of the function reaches this
// constant in the same edit — and the floor under the walk reddens if it
// does not.
const writingCanonicalsName = "WritingCanonicals"

// writingCanonicalsReader is one call site of the set, named by the function
// that makes it — or by its file, when the call sits at package level, which
// no reader does today.
type writingCanonicalsReader struct {
	file string
	name string
}

// writingCanonicalsReaders reads, out of the tree, every function OUTSIDE
// this package that calls WritingCanonicals.
//
// From the AST and not from a grep: the set's godoc names these readers, and
// a mention of one inside a comment is exactly what this guard checks the
// doc against — counting a comment as a call would make it agree with a doc
// that enumerates nothing.
//
// A caller inside this package is not a reader of the shared set: it reads it
// to TYPE it (the pin above), and naming it in the doc would say the set
// exists for its own test.
func writingCanonicalsReaders(t *testing.T) []writingCanonicalsReader {
	t.Helper()
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs(.): %v", err)
	}
	var out []writingCanonicalsReader
	for _, top := range []string{"pkg", "bots", "cmd", "internal"} {
		root := filepath.Join("..", "..", "..", top)
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if base := filepath.Base(path); base == "vendor" || base == "testdata" || base == "node_modules" {
					return fs.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Contains(raw, []byte(writingCanonicalsName)) {
				return nil
			}
			dir, err := filepath.Abs(filepath.Dir(path))
			if err != nil {
				return err
			}
			if dir == self {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, raw, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("%s: %v — a file naming %s that does not parse is a finding, not a skip: this walk would otherwise report no reader there", path, err, writingCanonicalsName)
			}
			var calls []token.Pos
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel != nil && fun.Sel.Name == writingCanonicalsName {
						calls = append(calls, call.Pos())
					}
				case *ast.Ident:
					if fun.Name == writingCanonicalsName {
						calls = append(calls, call.Pos())
					}
				}
				return true
			})
			for _, pos := range calls {
				name := filepath.Base(path)
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if ok && fn.Name != nil && fn.Pos() <= pos && pos <= fn.End() {
						name = fn.Name.Name
						break
					}
				}
				if !slices.ContainsFunc(out, func(r writingCanonicalsReader) bool { return r.name == name }) {
					out = append(out, writingCanonicalsReader{file: path, name: name})
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", root, err)
		}
	}
	slices.SortFunc(out, func(a, b writingCanonicalsReader) int { return strings.Compare(a.name, b.name) })
	return out
}

// TestTheWritingSetsDocNamesEveryReaderOfIt is the class contract of
// WritingCanonicals' own godoc: the set is shared BECAUSE several surfaces
// must agree about a word, so the doc says which — and an enumeration
// maintained by hand names the readers of the day it was written. The block
// this guard was written against named two of the six functions that call
// the set; restoring it reddens here with four unnamed, one of them the
// production function that decides what the operator notice prints.
//
// The readers come out of the tree, so a new one is a doc edit in the same
// change; the doc may describe a reader as it likes, as long as the name is
// in it.
func TestTheWritingSetsDocNamesEveryReaderOfIt(t *testing.T) {
	readers := writingCanonicalsReaders(t)
	// A FLOOR on the walk: it prefilters on an identifier over four roots, so
	// a rename or a root that stopped matching would leave this guard
	// checking a doc against an empty set. 4 is under the 6 readers today, so
	// one may be retired without touching this line.
	if len(readers) < 4 {
		t.Fatalf("the walk found %d function(s) outside this package calling %s(), want at least 4: the identifier or one of the roots moved, and this guard is now green over a doc it compared to nothing", len(readers), writingCanonicalsName)
	}
	doc := writingCanonicalsDoc(t)
	for _, r := range readers {
		if !strings.Contains(doc, r.name) {
			t.Errorf("%s calls %s() and the set's godoc does not name it (%s): the doc is what tells the next author who else has to agree about a word, and it has already gone stale twice — name the reader, or stop enumerating them", r.name, writingCanonicalsName, r.file)
		}
	}
}

// writingCanonicalsDoc is the doc comment of WritingCanonicals, read from
// this package's own source.
//
// Read at the position the function is DECLARED, not matched in the file's
// text: the identifier appears in several comments here, and a block
// elsewhere in the file would answer for the one that documents the set.
func writingCanonicalsDoc(t *testing.T) string {
	t.Helper()
	const src = "canonical.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", src, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != writingCanonicalsName {
			continue
		}
		if fn.Doc == nil {
			t.Fatalf("%s: %s has no doc comment: the enumeration of its readers is what keeps them agreeing about a word", src, writingCanonicalsName)
		}
		return fn.Doc.Text()
	}
	t.Fatalf("%s: no declaration of %s: this guard reads the doc of the shared set and the set moved", src, writingCanonicalsName)
	return ""
}
