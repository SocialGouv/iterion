package testparallel

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
)

// Campaign is the call graph of one package's own test files: who declares
// what, and who calls what.
//
// Two packages of this tree partition their tests between parallel and serial
// and need the same reading — which test, through which helper, reaches a call
// that cannot run concurrently. The reading lives here so each package keeps
// only its own table and its own rule; the second copy of a graph walk is
// where a recopied guard drifts.
//
// The graph is keyed by NAME, never by type: two methods called Cleanup on
// different fixture types are one node. That over-approximates reachability,
// which is the safe direction for a parallelism verdict — a wrongly-serial
// test costs wall time, a wrongly-parallel one panics the -race binary — and
// callers record what comes out in a table, so an over-approximation shows up
// as a line to read rather than a silent loss.
type Campaign struct {
	files    []string            // base names, sorted
	declares map[string][]string // base name -> the functions it declares
	owner    map[string]string   // function -> base name of the file declaring it
	calls    map[string][]call   // function -> the calls in its body, in source order
	tests    map[string][]*ast.FuncDecl
}

// call is one call site: the bare name the source selects (`Setenv` for both
// t.Setenv and os.Setenv) and the qualified form when the receiver is a plain
// identifier (`exec.Command`). A seed may be written either way; the bare form
// is matched first, so a bare seed keeps covering every receiver.
type call struct{ bare, qualified string }

// ReadCampaign parses every _test.go file of dir. skip, when non-nil, drops a
// file by its source — how a caller excludes a build-tagged layer that its
// deterministic run never compiles (see LiveOnly).
func ReadCampaign(dir string, skip func(src string) bool) (*Campaign, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the package directory %s: %w", dir, err)
	}
	c := &Campaign{
		declares: map[string][]string{}, owner: map[string]string{},
		calls: map[string][]call{}, tests: map[string][]*ast.FuncDecl{},
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, rerr := os.ReadFile(dir + string(os.PathSeparator) + name) // #nosec G304 -- a test reading its own package's sources
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", name, rerr)
		}
		if skip != nil && skip(string(raw)) {
			continue
		}
		f, perr := parser.ParseFile(fset, name, raw, parser.SkipObjectResolution)
		if perr != nil {
			return nil, fmt.Errorf("parse %s: %w", name, perr)
		}
		c.files = append(c.files, name)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := fn.Name.Name
			c.declares[name] = append(c.declares[name], key)
			if _, seen := c.owner[key]; !seen {
				c.owner[key] = name
			}
			if IsTopLevelTest(fn) {
				c.tests[name] = append(c.tests[name], fn)
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				expr, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := expr.Fun.(type) {
				case *ast.SelectorExpr:
					site := call{bare: fun.Sel.Name}
					if recv, ok := fun.X.(*ast.Ident); ok {
						site.qualified = recv.Name + "." + fun.Sel.Name
					}
					c.calls[key] = append(c.calls[key], site)
				case *ast.Ident:
					c.calls[key] = append(c.calls[key], call{bare: fun.Name})
				}
				return true
			})
		}
	}
	sort.Strings(c.files)
	return c, nil
}

// Files returns the base names the walk read, sorted.
func (c *Campaign) Files() []string { return c.files }

// Tests returns the top-level tests a file declares, in source order.
func (c *Campaign) Tests(file string) []*ast.FuncDecl { return c.tests[file] }

// Declares returns the functions a file declares, in source order.
func (c *Campaign) Declares(file string) []string { return c.declares[file] }

// FileOf names the file declaring fn, or "" when the package's test files
// declare no such function (a production function, or one from another
// package).
func (c *Campaign) FileOf(fn string) string { return c.owner[fn] }

// CallsDirectly reports whether fn's own body calls name — bare
// (`Setenv`) or qualified (`exec.Command`).
func (c *Campaign) CallsDirectly(fn, name string) bool {
	for _, site := range c.calls[fn] {
		if site.bare == name || site.qualified == name {
			return true
		}
	}
	return false
}

// Reach returns, for every function that reaches one of the seeds, the reason:
// the seed itself when the function calls it, or "via g() in f.go" when it
// borrows it from another function of these test files. A seed's own entry is
// absent — a function is not its own reason.
//
// Only calls to functions these test files declare are followed, so the walk
// stops at the package boundary instead of guessing about production code.
func (c *Campaign) Reach(seeds map[string]bool) map[string]string {
	reason := map[string]string{}
	for fn, sites := range c.calls {
		for _, site := range sites {
			switch {
			case seeds[site.bare]:
				reason[fn] = site.bare
			case site.qualified != "" && seeds[site.qualified]:
				reason[fn] = site.qualified
			default:
				continue
			}
			break
		}
	}
	// The fixed point walks a SORTED list of functions, and each function's
	// own call sites stay in source order: both keys are stable, so the reason
	// a function ends up with is a function of the sources and not of a map's
	// iteration order. Ranging the map here would make a borrowed reason flip
	// between runs for any function that calls two tainted helpers — and the
	// callers of this reader assert those strings against a committed table,
	// so a flipping reason is a guard that fails at random.
	order := make([]string, 0, len(c.calls))
	for fn := range c.calls {
		order = append(order, fn)
	}
	sort.Strings(order)
	for changed := true; changed; {
		changed = false
		for _, fn := range order {
			if reason[fn] != "" {
				continue
			}
			for _, site := range c.calls[fn] {
				callee := site.bare
				if reason[callee] != "" && c.owner[callee] != "" {
					reason[fn] = "via " + callee + "() in " + c.owner[callee]
					changed = true
					break
				}
			}
		}
	}
	return reason
}

// FilesReaching aggregates Reach per file: the whole set of reasons the file's
// own functions carry, sorted, because a file held by two different calls is a
// fact a one-reason answer loses. Direct reasons win over borrowed ones — a
// borrowed reason is only informative when the file touches nothing itself.
func (c *Campaign) FilesReaching(seeds map[string]bool) map[string]string {
	reason := c.Reach(seeds)
	out := map[string]string{}
	for _, f := range c.files {
		direct, borrowed := map[string]bool{}, map[string]bool{}
		for _, fn := range c.declares[f] {
			why := reason[fn]
			switch {
			case why == "":
			case strings.HasPrefix(why, "via "):
				borrowed[why] = true
			default:
				direct[why] = true
			}
		}
		chosen := direct
		if len(chosen) == 0 {
			chosen = borrowed
		}
		if len(chosen) == 0 {
			continue
		}
		reasons := make([]string, 0, len(chosen))
		for why := range chosen {
			reasons = append(reasons, why)
		}
		sort.Strings(reasons)
		out[f] = strings.Join(reasons, ", ")
	}
	return out
}

// Closure returns the functions reachable FROM roots, roots included: the
// helpers a set of tests actually drives. Reach answers "who gets there";
// Closure answers "what runs under this test", which is the question a rule
// about subprocesses has to ask.
func (c *Campaign) Closure(roots map[string]bool) map[string]bool {
	out := map[string]bool{}
	var walk func(fn string)
	walk = func(fn string) {
		if out[fn] {
			return
		}
		out[fn] = true
		for _, site := range c.calls[fn] {
			if c.owner[site.bare] != "" {
				walk(site.bare)
			}
		}
	}
	for fn := range roots {
		walk(fn)
	}
	return out
}

// IsTopLevelTest reads the signature `func TestX(t *testing.T)` — a method, a
// helper taking more than t, or TestMain is not one.
func IsTopLevelTest(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
		return false
	}
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}

// OpensWithParallel reads the rule the way a reader does: the test declares
// itself parallel before it does anything else. A t.Helper() first is still
// opening with t.Parallel().
func OpensWithParallel(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}
	for _, stmt := range fn.Body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			return false
		}
		expression, ok := expr.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := expression.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		switch sel.Sel.Name {
		case "Parallel":
			return true
		case "Helper":
			continue
		default:
			return false
		}
	}
	return false
}

// LiveOnly marks a file the deterministic layer never compiles: the live layer
// costs real LLM money and no CI job runs it, so its wall is not the race
// leg's and its serialisation is a spend decision, not a speed one.
func LiveOnly(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, "//go:build ") {
			return strings.Contains(line, "live")
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "//") {
			return false
		}
	}
	return false
}
