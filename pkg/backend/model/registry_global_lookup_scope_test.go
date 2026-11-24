package model

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

// Its own file, and parallel: the walk below touches no process global, while
// registry_global_lookup_test.go (the restore witnesses) installs one and is
// therefore serial. One file, one answer — the partition rule of
// parallel_campaign_test.go applies to its own guards too.

// globalLookupSetters are the installs whose result is the only way back.
var globalLookupSetters = map[string]bool{
	"SetCredentialsLookup": true,
	"SetOAuthDirLookup":    true,
}

// bootInstallers are the production sites that install for the PROCESS's
// lifetime, where there is nothing to give back. Named, not pattern-matched: a
// new boot installer is a decision somebody makes once.
var bootInstallers = map[string]bool{
	"cmd/iterion/runner.go": true,
}

// TestNoTestDiscardsAGlobalLookupRestore is the guard the migration earns: a
// setter call whose result is dropped installs a reader that outlives its
// test. Both forms that keep it are accepted — t.Cleanup(SetX(…)) and
// restore := SetX(…) — because the second is what a test of the restore
// itself needs; only a bare statement throws the way back away.
//
// It walks the module, not this package: the leak this found lived in
// pkg/supervise, and a guard that reads only its own package would have
// reported a clean tree.
func TestNoTestDiscardsAGlobalLookupRestore(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	var offenders []string
	seen := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", ".works", ".claude", ".iterion", "graphify-out":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, "_test.go") && bootInstallers[rel] {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !strings.Contains(string(raw), "SetCredentialsLookup") && !strings.Contains(string(raw), "SetOAuthDirLookup") {
			return nil
		}
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, raw, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(f, func(n ast.Node) bool {
			stmt, ok := n.(*ast.ExprStmt)
			if !ok {
				return true
			}
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			case *ast.Ident:
				name = fun.Name
			}
			if !globalLookupSetters[name] {
				return true
			}
			seen++
			offenders = append(offenders, rel+":"+fset.Position(stmt.Pos()).String()[len(path)+1:]+" "+name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	if root == "" {
		t.Fatal("no module root: the walk judged nothing")
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("%d call(s) throw away the restore a global lookup install returns — the reader stays installed for every test that runs after, in the same binary. Wrap them in t.Cleanup(…) or keep the restore in a variable:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
