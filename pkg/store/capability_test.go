package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type decorator struct{ RunStore }

func (d decorator) Unwrap() RunStore { return d.RunStore }

type opaqueDecorator struct{ RunStore }

type correctingDecorator struct{ decorator }

func (correctingDecorator) SetRunOutputCorrection(context.Context, string, string, OutputCorrectionEpisode) error {
	return nil
}

// TestCapabilityLooksThroughADecorator: a probe finds the capability of the
// store a decorator wraps, and a decorator that carries it answers first.
func TestCapabilityLooksThroughADecorator(t *testing.T) {
	base, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := AsScratchBankStore(decorator{base}); got == nil || any(got) != any(base) {
		t.Fatalf("the scratch bank of the wrapped store was not found: %v", got)
	}
	if got := AsBackendSessionStore(decorator{decorator{base}}); got == nil {
		t.Fatal("a capability two decorators deep was not found")
	}
	if got := AsScratchBankStore(opaqueDecorator{base}); got != nil {
		t.Fatalf("a decorator without Unwrap exposed %T — this test no longer tells the two apart", got)
	}
	d := correctingDecorator{decorator{base}}
	if got := AsOutputCorrectionStore(d); any(got) != any(d) {
		t.Fatalf("the decorator's own capability did not answer first: %T", got)
	}
	if got := AsScratchBankStore(nil); got != nil {
		t.Fatalf("a nil store answered %T", got)
	}
}

// TestEveryProbeLooksThroughDecorators: every As* probe of the package goes
// through capability, so none of them is hidden by a decorator.
func TestEveryProbeLooksThroughDecorators(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	probes := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "As") || fn.Type.Params.NumFields() != 1 {
				continue
			}
			if id, ok := fn.Type.Params.List[0].Type.(*ast.Ident); !ok || id.Name != "RunStore" {
				continue
			}
			probes++
			ret, ok := onlyStatement(fn).(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 || !callsCapability(ret.Results[0]) {
				t.Errorf("%s: %s does not return capability[...](s): a decorator would hide what it probes", name, fn.Name.Name)
			}
		}
	}
	if probes < 20 {
		t.Fatalf("found %d As* probes, want the package's ~26 — the scan no longer reads them", probes)
	}
}

func onlyStatement(fn *ast.FuncDecl) ast.Stmt {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return nil
	}
	return fn.Body.List[0]
}

func callsCapability(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	idx, ok := call.Fun.(*ast.IndexExpr)
	if !ok {
		return false
	}
	id, ok := idx.X.(*ast.Ident)
	return ok && id.Name == "capability"
}
