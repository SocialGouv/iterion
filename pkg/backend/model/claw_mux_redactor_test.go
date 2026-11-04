package model

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The multiplexer's reader quotes a raw IPC line into a decode error and
// then cuts it. The run's pass must cross that line before the cut, which
// is why the claw backend wires the reader's pass from Task.RedactSecrets
// at construction (iterion#2822). This sweep pins the wiring: the file
// that builds a delegate.NewMultiplexer must also call mux.SetRedactor
// with the task's redactor, or the decode error's quoted line cuts before
// the pass.
func TestEveryMultiplexerGetsTheRunsRedactor(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		sites := 0
		wiring := 0
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "NewMultiplexer" {
				sites++
			}
			if sel.Sel.Name == "SetRedactor" && len(call.Args) > 0 {
				if strings.Contains(exprText(t, fset, call.Args[0]), "RedactSecrets") {
					wiring++
				}
			}
			return true
		})
		if sites > 0 && wiring == 0 {
			t.Errorf("%s builds a delegate.NewMultiplexer without mux.SetRedactor(task.RedactSecrets) — the decode error's quoted line would cut before the run's pass (iterion#2822)", name)
		}
		seen += sites
	}
	if seen == 0 {
		t.Fatalf("the sweep found no delegate.NewMultiplexer site — the predicate stopped matching")
	}
}

// exprText prints an expression back to source.
func exprText(t *testing.T, fset *token.FileSet, e ast.Node) string {
	t.Helper()
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, e); err != nil {
		t.Fatalf("print: %v", err)
	}
	return buf.String()
}
