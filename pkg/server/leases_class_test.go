package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Every net that campaigns on the server's lease store has to have an entry in
// leasedNets: the no-shared-database warning is formatted from that value, so
// a net missing from it takes the memory fallback while the operator is told
// about the others — one named net and a second one silently running on every
// replica is exactly what that warning exists to prevent.
//
// The ground truth is the package's own SOURCE, not a second list: every
// lease.Spec it builds, wherever the lease.Run call sits (the merge-gate
// sweep's spec comes from a method, the issue sync's is inline), because the
// warning's own test can only compare the message to a list — and a list is
// the thing that drifts. The sweep reads the spec's Name field and resolves it
// through the package's string constants; a name it CANNOT resolve is reported
// too, since an unresolvable name is how a net would slip past.
func TestLeasedNets_EveryCampaigningNetIsRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, n := range leasedNets {
		if n.name == "" || strings.TrimSpace(n.what) == "" {
			t.Errorf("leasedNets holds an entry with no name or no consequence: %v, %v", n.name, n.what)
		}
		// The warning takes its TRIGGER from this predicate too: an entry
		// without one would have to be named unconditionally (leases.go
		// treats nil as wired) or disappear, and disappearing is the silence
		// this registry exists to prevent.
		if n.wired == nil {
			t.Errorf("the %q entry carries no start condition — the warning cannot tell whether this configuration starts it", n.name)
		}
		registered[n.name] = true
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		parsed[name] = f
	}
	// Pass 1 — the package's string constants, so a Name field spelled as a
	// constant resolves to the lease name a lease document would carry.
	strConsts := map[string]string{}
	for _, f := range parsed {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
					continue
				}
				if lit, ok := vs.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if v, uerr := strconv.Unquote(lit.Value); uerr == nil {
						strConsts[vs.Names[0].Name] = v
					}
				}
			}
		}
	}
	// Pass 2 — every lease.Spec the package builds.
	var found, offenders []string
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Spec" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "lease" {
				return true
			}
			where := fset.Position(lit.Pos()).String()
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Name" {
					continue
				}
				switch v := kv.Value.(type) {
				case *ast.BasicLit:
					if s, uerr := strconv.Unquote(v.Value); uerr == nil {
						found = append(found, s)
						if !registered[s] {
							offenders = append(offenders, where+": campaigns for "+s)
						}
						return true
					}
				case *ast.Ident:
					if s, ok := strConsts[v.Name]; ok {
						found = append(found, s)
						if !registered[s] {
							offenders = append(offenders, where+": campaigns for "+v.Name+" = "+s)
						}
						return true
					}
				}
				offenders = append(offenders, where+": a lease name this sweep cannot resolve")
			}
			return true
		})
	}
	for _, o := range offenders {
		t.Errorf("%s, which is not in leasedNets — the no-shared-database warning will name every other net and stay silent about this one", o)
	}
	// The sweep must be able to see the nets that DO exist: a refactor that
	// moved spec construction out of this package would otherwise leave it
	// green over nothing.
	if len(found) < 2 {
		t.Fatalf("the sweep found %d lease.Spec in this package (%v), want the nets it guards — a sweep that sees nothing passes over anything", len(found), found)
	}
	for _, want := range []string{leaseMergeGateSweeper, leaseBoardIssueSync} {
		if !slices.Contains(found, want) {
			t.Errorf("the sweep did not see the %q campaign in this package's source: found %v", want, found)
		}
	}
}
