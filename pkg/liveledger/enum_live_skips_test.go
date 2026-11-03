package liveledger

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// TestEveryLiveSkipNamesItsPrerequisite closes the class the chokepoint
// opened. `skipLive` records a `skipped` row naming what was missing, so
// `never` stops meaning both "nobody has paid for this target" and "nobody
// could run it here" — but only for the guards that go through it. A raw
// `t.Skip` in a live-tagged test writes NOTHING (the guard refuses before
// the harness reaches Track) or writes the unnamed fallback (when Track is
// already registered), and either way the committed ledger loses the
// prerequisite. Measured: four sibling guards kept the old defect while the
// requireX chokepoint was converted — the fifth would have drifted the same
// way.
//
// A live-tagged test may therefore skip in exactly two shapes:
//
//   - the `testing.Short()` arm, which is the free gate declining to run a
//     live test at all — no target, no row, nothing to record;
//   - a skip whose IMMEDIATELY PRECEDING statement names the prerequisite
//     to the ledger (`liveledger.RecordSkip` — what `skipLive` itself does
//     — or `<tracker>.SetSkipReason`).
//
// `skipLive(t, …)` is neither: it is not a `t.Skip` call at all, so a guard
// routed through the chokepoint never reaches this scan.
func TestEveryLiveSkipNamesItsPrerequisite(t *testing.T) {
	// The live harness: the live-tagged files and the untagged knob file the
	// live build compiles with them (anti-vacuity is liveHarnessSources').
	live := liveHarnessSources(t)
	if !strings.Contains(live[liveKnobsFile], "func skipLive(") {
		t.Fatalf("e2e/%s no longer declares skipLive — the chokepoint every guard's skip goes through", liveKnobsFile)
	}

	var bare []string
	for name, src := range live {
		sites, err := bareSkipSites(name, src)
		if err != nil {
			t.Fatalf("parse e2e/%s: %v", name, err)
		}
		for _, s := range sites {
			bare = append(bare, "e2e/"+name+":"+s)
		}
	}
	if len(bare) > 0 {
		sort.Strings(bare)
		t.Fatalf("%d live-test skip(s) do not name their prerequisite to the ledger — route them through skipLive(t, …), or name the reason with <tracker>.SetSkipReason(reason) on the line before the skip:\n  - %s",
			len(bare), strings.Join(bare, "\n  - "))
	}
}

// bareSkipSites reports the `t.Skip`/`t.Skipf`/`t.SkipNow` calls of one
// source that are neither the testing.Short() arm nor preceded by a
// reason-naming call.
// Returned as "<line>: <enclosing func>" so the message points at the site.
func bareSkipSites(name, src string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		for _, pos := range bareSkipsInBlock(fn.Body, false) {
			out = append(out, fmt.Sprintf("%d: %s", fset.Position(pos).Line, fn.Name.Name))
		}
	}
	return out, nil
}

// bareSkipsInBlock walks one block's statements in order, so the statement
// BEFORE a skip is readable — a sibling's SetSkipReason elsewhere in the
// body must not vouch for a skip that names nothing (the motif
// bodyScopedHooks already pays for). inShort carries the testing.Short()
// arm down into nested blocks.
func bareSkipsInBlock(block *ast.BlockStmt, inShort bool) []token.Pos {
	var out []token.Pos
	var prev ast.Stmt
	for _, stmt := range block.List {
		if call, ok := skipCall(stmt); ok {
			if !inShort && !namesTheReason(prev) {
				out = append(out, call.Pos())
			}
			prev = stmt
			continue
		}
		switch x := stmt.(type) {
		case *ast.IfStmt:
			out = append(out, bareSkipsInBlock(x.Body, inShort || isShortModeCond(x.Cond))...)
			if els, ok := x.Else.(*ast.BlockStmt); ok {
				out = append(out, bareSkipsInBlock(els, inShort)...)
			}
			if els, ok := x.Else.(*ast.IfStmt); ok {
				out = append(out, bareSkipsInBlock(&ast.BlockStmt{List: []ast.Stmt{els}}, inShort)...)
			}
		case *ast.BlockStmt:
			out = append(out, bareSkipsInBlock(x, inShort)...)
		case *ast.ForStmt:
			out = append(out, bareSkipsInBlock(x.Body, inShort)...)
		case *ast.RangeStmt:
			out = append(out, bareSkipsInBlock(x.Body, inShort)...)
		case *ast.SwitchStmt:
			out = append(out, bareSkipsInBody(x.Body, inShort)...)
		case *ast.TypeSwitchStmt:
			out = append(out, bareSkipsInBody(x.Body, inShort)...)
		case *ast.SelectStmt:
			out = append(out, bareSkipsInBody(x.Body, inShort)...)
		case *ast.LabeledStmt:
			out = append(out, bareSkipsInBlock(&ast.BlockStmt{List: []ast.Stmt{x.Stmt}}, inShort)...)
		case *ast.DeferStmt, *ast.GoStmt, *ast.ExprStmt:
			// A skip inside a closure keeps its own block list, reached
			// through the func literal below.
			out = append(out, bareSkipsInLiterals(stmt, inShort)...)
		default:
			out = append(out, bareSkipsInLiterals(stmt, inShort)...)
		}
		prev = stmt
	}
	return out
}

// bareSkipsInBody walks the clause bodies of a switch/select: each clause
// is its own statement list, so a skip's predecessor is its clause's.
func bareSkipsInBody(body *ast.BlockStmt, inShort bool) []token.Pos {
	var out []token.Pos
	for _, clause := range body.List {
		switch c := clause.(type) {
		case *ast.CaseClause:
			out = append(out, bareSkipsInBlock(&ast.BlockStmt{List: c.Body}, inShort)...)
		case *ast.CommClause:
			out = append(out, bareSkipsInBlock(&ast.BlockStmt{List: c.Body}, inShort)...)
		}
	}
	return out
}

// bareSkipsInLiterals reaches the func literals of an arbitrary statement
// (a `t.Run(name, func(t *testing.T){ … })` body, an assignment's closure),
// each of which is a fresh statement list.
func bareSkipsInLiterals(stmt ast.Stmt, inShort bool) []token.Pos {
	var out []token.Pos
	ast.Inspect(stmt, func(n ast.Node) bool {
		lit, ok := n.(*ast.FuncLit)
		if !ok || lit.Body == nil {
			return true
		}
		out = append(out, bareSkipsInBlock(lit.Body, inShort)...)
		return false
	})
	return out
}

// skipCall reports whether stmt is a `<x>.Skip(…)` / `<x>.Skipf(…)` /
// `<x>.SkipNow()` call — the three ways testing.TB marks a test skipped.
func skipCall(stmt ast.Stmt) (*ast.CallExpr, bool) {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	switch sel.Sel.Name {
	case "Skip", "Skipf", "SkipNow":
		return call, true
	}
	return nil, false
}

// isShortModeCond reports whether cond is `testing.Short()` — the free
// gate's own arm, which records nothing because no target is running.
func isShortModeCond(cond ast.Expr) bool {
	call, ok := cond.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Short" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "testing"
}

// namesTheReason reports whether stmt hands the prerequisite to the ledger:
// liveledger.RecordSkip (what skipLive itself calls) or a tracker's
// SetSkipReason.
func namesTheReason(stmt ast.Stmt) bool {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "SetSkipReason" || sel.Sel.Name == "RecordSkip"
}

// The scanner's own table: each shape the rule accepts or refuses, so a
// mutant that blesses everything (or refuses the short arm) reddens here
// before it reaches the real e2e tree.
func TestBareSkipSites(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"the short-mode arm is the free gate declining", `
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}`, 0},
		{"a raw prerequisite skip is bare", `
	if os.Getenv("K") == "" {
		t.Skip("K is required")
	}`, 1},
		{"a skip whose predecessor names the reason is fine", `
	tracker.SetSkipReason(reason)
	t.Skip(reason)`, 0},
		{"RecordSkip names it too — the chokepoint's own body", `
	liveledger.RecordSkip(t, reason)
	t.Skip(reason)`, 0},
		{"a SetSkipReason elsewhere in the body does not vouch for a later bare skip", `
	tracker.SetSkipReason(other)
	doSomething()
	t.Skipf("docker: %v", err)`, 1},
		{"skipLive is not a t.Skip call", `
	skipLive(t, "no docker image %q", ref)`, 0},
		{"a bare skip inside a subtest closure is seen", `
	t.Run("sub", func(t *testing.T) {
		t.Skip("nope")
	})`, 1},
		{"a bare skip in a switch clause is seen", `
	switch {
	case true:
		t.Skipf("no local image — name one in %s", envBase)
	}`, 1},
		{"SkipNow is a skip too", `
	if os.Getenv("K") == "" {
		t.SkipNow()
	}`, 1},
		{"a bare skip in the else arm of a short-mode if is NOT blessed", `
	if testing.Short() {
		t.Skip("short")
	} else {
		t.Skip("not short")
	}`, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "//go:build live\n\npackage e2e\n\nfunc TestLive_Probe(t *testing.T) {" + c.body + "\n}\n"
			got, err := bareSkipSites("probe_live_test.go", src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) != c.want {
				t.Fatalf("bareSkipSites = %v (%d sites), want %d", got, len(got), c.want)
			}
		})
	}
}
