package bots

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// processGlobalMutators are the calls a parallel test may not reach. They all
// mutate state the whole process shares, so they cannot be confined to one
// test the way a `t.TempDir()` fixture is.
//
// `Setenv` covers both spellings and they fail very differently: `t.Setenv`
// panics the moment a parallel test calls it (the testing package refuses it
// outright), while `os.Setenv` says nothing and silently rewrites the
// environment of every test running beside it. The quiet one is why this
// guard exists.
//
// `Chdir` is the same hole through the working directory: one test's `cd`
// relocates every sibling's relative path.
var processGlobalMutators = map[string]bool{
	"Setenv":   true,
	"Unsetenv": true,
	"Chdir":    true,
}

// TestEveryParallelTestKeepsItsFixturesOutOfTheProcess holds the premise of
// this package's t.Parallel() campaign (#2564 P2): a test made parallel owns
// every fixture it touches.
//
// A test suite is configuration of a kind — nothing type-checks "this test
// shares no state" — and the campaign grows by batches, so the premise rots
// one file at a time. The guard reads what the package actually does: it
// parses the test sources, finds the tests that call `t.Parallel()`, taints
// every helper that mutates the environment or the working directory, closes
// the taint over the call graph, and names any parallel test that reaches
// one.
//
// Both sides travel the call graph, because both forms exist: a test is
// parallel when it, or a helper it names, calls `t.Parallel()`, and unsafe
// when it, or a helper it names, mutates the process. What it does NOT judge:
// a fixture shared through a package-level variable, which needs the race
// detector — that is why the campaign's measurement runs `-race -count=1` —
// and either side reached through a function value passed in rather than
// named. What it DOES close is the whole class that costs nothing to
// introduce and everything to debug: a helper three calls away that sets an
// environment variable.
//
// Serial by construction, and the serial tests of this package stay free to
// call Setenv: the testing package releases a parallel test only once its
// parent's sequential pass has returned, so a serial top-level test never
// runs beside a parallel body.
func TestEveryParallelTestKeepsItsFixturesOutOfTheProcess(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		files[e.Name()] = f
	}

	// hazards: a function that mutates process-global state, by the spelling
	// it uses. marksParallel: one that calls t.Parallel(). calls: the
	// package-level functions each one names. tests: where each test lives.
	hazards := map[string]string{}
	marksParallel := map[string]string{}
	calls := map[string][]string{}
	tests := map[string]string{}
	for path, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					spelling := fun.Sel.Name
					if recv, ok := fun.X.(*ast.Ident); ok {
						spelling = recv.Name + "." + spelling
					}
					if processGlobalMutators[fun.Sel.Name] {
						hazards[fn.Name.Name] = spelling
					}
					if fun.Sel.Name == "Parallel" {
						marksParallel[fn.Name.Name] = spelling
					}
				case *ast.Ident:
					calls[fn.Name.Name] = append(calls[fn.Name.Name], fun.Name)
				}
				return true
			})
			if strings.HasPrefix(fn.Name.Name, "Test") {
				tests[fn.Name.Name] = filepath.Base(path)
			}
		}
	}

	// Close both sets over the call graph: a test that calls a helper that
	// calls a helper that sets an environment variable is exactly as unsafe
	// as one that sets it itself, and one that takes its `t.Parallel()` from
	// a fixture helper is exactly as parallel. Both are the form nobody sees
	// in review.
	closeOverCalls := func(reached map[string]string) {
		for grew := true; grew; {
			grew = false
			for fn, callees := range calls {
				if _, ok := reached[fn]; ok {
					continue
				}
				for _, callee := range callees {
					if spelling, ok := reached[callee]; ok {
						reached[fn] = spelling + " (through " + callee + ")"
						grew = true
						break
					}
				}
			}
		}
	}
	closeOverCalls(hazards)
	closeOverCalls(marksParallel)

	var bad []string
	parallelTests := 0
	for test, file := range tests {
		if _, isParallel := marksParallel[test]; !isParallel {
			continue
		}
		parallelTests++
		if spelling, tainted := hazards[test]; tainted {
			bad = append(bad, test+" ("+file+") reaches "+spelling)
		}
	}
	if parallelTests < 20 {
		t.Fatalf("found %d parallel tests across %d test files of this package — the campaign's premise is what this guard holds, and it no longer sees it (batch reverted, or the walk broken?)", parallelTests, len(files))
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("parallel test %s — a parallel test owns every fixture it touches. Give it a per-test fixture (t.TempDir, cmd.Env on the child instead of the parent's environment), or drop its t.Parallel()", b)
	}
}
