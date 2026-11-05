package model

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

// setVarsReaders are the only production callers of SetVars: they hand the
// executor a READ value — the engine's resolveVars output (pkg/runtime's
// pushExecutorVars), and the dry run forwarding what the engine pushed to it
// into its program executor. Every other seam holds a launch's TEXT, and text
// enters through SetLaunchVars, the one reading of it.
var setVarsReaders = []string{
	"pkg/dryrun/executor.go",
	"pkg/runtime/resume.go",
}

// Every seam that seeds an executor with launch values is held to ONE
// reading: a SetVars call outside the engine's push stores text as text, and
// a raw seed hands a `string[]` var to an `array` action param as a string
// (#2991) and leaves a resumed run's undeclared inputs in its vars.
//
// The sweep is over the production tree (pkg/, cmd/, internal/; tests and
// testdata excluded), so a new seam is caught where it is written.
//
// Mutations seen red (each restored): the three seams put back to SetVars —
// pkg/runview/executor.go (BuildExecutor), pkg/runview/service_launch.go and
// pkg/cli/resume_helpers.go (the two resume builders) — each named here.
func TestLaunchTextReachesTheExecutorOnlyThroughSetLaunchVars(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	fset := token.NewFileSet()
	var callers []string
	for _, top := range []string{"pkg", "cmd", "internal"} {
		walkErr := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "vendor" || strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Contains(src, []byte("SetVars(")) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetVars" {
					callers = append(callers, filepath.ToSlash(rel))
				}
				return true
			})
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", top, walkErr)
		}
	}
	if len(callers) == 0 {
		t.Fatal("the sweep found no SetVars call at all — the predicate stopped matching")
	}
	for _, c := range callers {
		if !slices.Contains(setVarsReaders, c) {
			t.Errorf("%s calls SetVars: launch text is seeded through SetLaunchVars, which reads it at each var's declared type (#2991)", c)
		}
	}
}
