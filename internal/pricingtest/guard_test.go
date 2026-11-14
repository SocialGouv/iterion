package pricingtest

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// The two live pricing tiers. A test binary linking either reads the host's
// cache and refreshes it from the network unless its TestMain isolates it.
const (
	costIsolationWitness = "TestTheTestBinaryRunsOnStubbedPricingTiers"
	costPackage          = "github.com/SocialGouv/iterion/pkg/backend/cost"
	clawRegistry         = "github.com/SocialGouv/iterion/pkg/claw/internal/apikit"
	aggregator           = "github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	thisPackage          = "github.com/SocialGouv/iterion/internal/pricingtest"
)

// belowPricingtest are the packages pricingtest itself imports: their tests
// cannot import it, so each isolates in its own way, named here.
var belowPricingtest = map[string]string{
	thisPackage: "its tests drive Isolate itself",
	"github.com/SocialGouv/iterion/pkg/backend/cost":       "its TestMain stubs both tiers' seams (specLookup, livePricing) and switches claw's registry off on a cache directory it owns, and seeds the process registry (checked below: testMainSetsBeforeRunning and the run-time witness costIsolationWitness); a test that exercises a tier pins a fixture on its seam or on the seeded registry",
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs": "it IS the aggregator: its tests build their own registries, the fetch pointed at an httptest server or switched off — 0 connections over 500 runs behind a logging proxy, 2026-11-14",
}

// TestEveryTestBinaryLinkingALivePricingTierIsIsolated reads the dependency
// graph of every test binary in the module and requires each one that links a
// live pricing tier to isolate it from TestMain: pricingtest.Run(m.Run), or
// pricingtest.Isolate() as a statement of TestMain before the first one that
// runs the tests. pkg/claw has its own guard (testutil.IsolateLiveRegistry).
func TestEveryTestBinaryLinkingALivePricingTierIsIsolated(t *testing.T) {
	root := sweepwalk.ModuleRoot(t)
	cmd := exec.Command("go", "list", "-test", "-f", "{{.ImportPath}}|{{join .Deps \" \"}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -test ./...: %v%s", err, stderrOf(err))
	}
	var linkers []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		importPath, deps, ok := strings.Cut(line, "|")
		if !ok {
			t.Fatalf("go list line %q: want import path|deps", line)
		}
		pkg, isTestMain := strings.CutSuffix(importPath, ".test")
		if !isTestMain || strings.HasPrefix(pkg, "github.com/SocialGouv/iterion/pkg/claw/") {
			continue
		}
		fields := strings.Fields(deps)
		if slices.Contains(fields, clawRegistry) || slices.Contains(fields, aggregator) || pkg == aggregator {
			linkers = append(linkers, pkg)
		}
	}
	if len(linkers) == 0 {
		t.Fatal("no test binary links a live pricing tier: the dependency read is wrong, so a green verdict here proves nothing")
	}
	files := packageTestFiles(t, root)
	for _, pkg := range linkers {
		if why, ok := belowPricingtest[pkg]; ok {
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s is exempt with no reason", pkg)
			}
			if pkg == costPackage {
				if !testMainSetsBeforeRunning(t, files[pkg], map[string]string{"CLAW_DISABLE_LIVE_REGISTRY": "1", "XDG_CACHE_HOME": ""}) {
					t.Errorf("%s's TestMain must set CLAW_DISABLE_LIVE_REGISTRY=1 and XDG_CACHE_HOME (an empty cache it owns), unconditionally, before it runs the tests: it cannot import pricingtest, and its test binary links claw's registry", pkg)
				}
				if !declaresTest(t, files[pkg], costIsolationWitness) {
					t.Errorf("%s has no %s: the run-time witness that its binary runs with both live tiers stubbed and the switch on", pkg, costIsolationWitness)
				}
			}
			continue
		}
		if !testMainIsolates(t, files[pkg]) {
			t.Errorf("%s's test binary links a live pricing tier (claw's registry or the models.dev aggregator) and its TestMain does not isolate it: a verdict that reads a price or a context window then depends on the host's cache and the network (#3257).\nAdd: func TestMain(m *testing.M) { os.Exit(pricingtest.Run(m.Run)) } — or call pricingtest.Isolate() at the top of the TestMain it has", pkg)
		}
	}
	for pkg := range belowPricingtest {
		if !slices.Contains(linkers, pkg) {
			t.Errorf("%s is exempt but its test binary no longer links a live tier: drop the exemption", pkg)
		}
	}
}

// testMainIsolates reports whether one of files declares a TestMain that
// isolates before running the tests.
func testMainIsolates(t *testing.T, files []string) bool {
	t.Helper()
	for _, name := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		pt := importName(f, thisPackage)
		if pt == "" {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil ||
				len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
				continue
			}
			m := fn.Type.Params.List[0].Names[0].Name
			if isolatesBeforeRunning(fn.Body, pt, m) {
				return true
			}
		}
	}
	return false
}

// isolatesBeforeRunning reads TestMain's statements in the order they run.
// It accepts pt.Run(m.Run) on the path that executes — a top-level
// statement's call, or a function literal passed as an argument to such a
// call and returning it — and pt.Isolate() called by a top-level statement
// before the first one that mentions m. A call under an if, a go or a defer,
// in a function literal that is not an argument, or after m already ran,
// isolates nothing it can be held to.
func isolatesBeforeRunning(body *ast.BlockStmt, pt, m string) bool {
	for _, stmt := range body.List {
		if spineRunsUnder(stmt, pt, m) {
			return true
		}
		if mentions(stmt, m) {
			return false
		}
		if callsIsolate(stmt, pt) {
			return true
		}
	}
	return false
}

// spineRunsUnder reports a pt.Run(m.Run) on stmt's executed path.
func spineRunsUnder(stmt ast.Stmt, pt, m string) bool {
	var exprs []ast.Expr
	switch v := stmt.(type) {
	case *ast.ExprStmt:
		exprs = []ast.Expr{v.X}
	case *ast.AssignStmt:
		exprs = v.Rhs
	case *ast.ReturnStmt:
		exprs = v.Results
	default:
		return false
	}
	for _, e := range exprs {
		if spineCall(e, pt, m) {
			return true
		}
	}
	return false
}

func spineCall(e ast.Expr, pt, m string) bool {
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok {
		return false
	}
	if isCall(call, pt, "Run") && len(call.Args) == 1 {
		if sel, ok := call.Args[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == m {
				return true
			}
		}
	}
	// The function expression evaluates first, then the arguments in order:
	// one that runs the tests before the wrapped Run means they already ran
	// unisolated.
	if mentions(call.Fun, m) {
		return false
	}
	for _, arg := range call.Args {
		if spineCall(arg, pt, m) {
			return true
		}
		if lit, ok := arg.(*ast.FuncLit); ok {
			if isolatesBeforeRunning(lit.Body, pt, m) {
				return true
			}
			if mentions(lit.Body, m) {
				return false
			}
			continue
		}
		if mentions(arg, m) {
			return false
		}
	}
	return false
}

func mentions(n ast.Node, m string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == m {
			found = true
		}
		return !found
	})
	return found
}

// callsIsolate reports a top-level `pt.Isolate()` or `x := pt.Isolate()`.
func callsIsolate(stmt ast.Stmt, pt string) bool {
	var e ast.Expr
	switch v := stmt.(type) {
	case *ast.ExprStmt:
		e = v.X
	case *ast.AssignStmt:
		if len(v.Rhs) == 1 {
			e = v.Rhs[0]
		}
	}
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	return ok && isCall(call, pt, "Isolate")
}

// testMainSetsBeforeRunning reports whether files declare a function
// TestMain (not a method) that sets each name with os.Setenv — to want[name],
// or to anything when that is "" — in a statement that always runs (a
// top-level call, an assignment from one, or an if's init), before the first
// statement that mentions its *testing.M, with no direct os.Setenv,
// os.Unsetenv of that literal name or os.Clearenv after it in between. An
// undo through a helper, a variable or syscall is not read: the run-time
// witness holds the switch's value.
func testMainSetsBeforeRunning(t *testing.T, files []string, want map[string]string) bool {
	t.Helper()
	for _, name := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		osName := importName(f, "os")
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
				continue
			}
			m := fn.Type.Params.List[0].Names[0].Name
			set := map[string]bool{}
			for _, stmt := range fn.Body.List {
				if mentions(stmt, m) {
					break
				}
				call := alwaysRunCall(stmt)
				// The statement's own set is recorded first; any other write
				// to a watched name in it — its body, an else, a helper's
				// literal — then undoes it.
				if call != nil && isCall(call, osName, "Setenv") && len(call.Args) == 2 {
					if key, ok := call.Args[0].(*ast.BasicLit); ok {
						if k, err := strconv.Unquote(key.Value); err == nil {
							if wantValue, watched := want[k]; watched {
								set[k] = wantValue == "" || literalEquals(call.Args[1], wantValue)
							}
						}
					}
				}
				ast.Inspect(stmt, func(n ast.Node) bool {
					c, ok := n.(*ast.CallExpr)
					if !ok || (c == call && isCall(c, osName, "Setenv")) {
						return true
					}
					if isCall(c, osName, "Clearenv") {
						clear(set)
						return true
					}
					if (!isCall(c, osName, "Setenv") && !isCall(c, osName, "Unsetenv")) || len(c.Args) == 0 {
						return true
					}
					if lit, ok := c.Args[0].(*ast.BasicLit); ok {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							delete(set, v)
						}
					}
					return true
				})
			}
			all := true
			for k := range want {
				all = all && set[k]
			}
			if all {
				return true
			}
		}
	}
	return false
}

// alwaysRunCall is the call a statement always makes: a top-level call, the
// call an assignment takes its value from, or the one in an if's init.
func alwaysRunCall(stmt ast.Stmt) *ast.CallExpr {
	var e ast.Expr
	switch v := stmt.(type) {
	case *ast.ExprStmt:
		e = v.X
	case *ast.AssignStmt:
		if len(v.Rhs) == 1 {
			e = v.Rhs[0]
		}
	case *ast.IfStmt:
		if init, ok := v.Init.(*ast.AssignStmt); ok && len(init.Rhs) == 1 {
			e = init.Rhs[0]
		}
	}
	call, _ := ast.Unparen(e).(*ast.CallExpr)
	return call
}

func literalEquals(e ast.Expr, want string) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && v == want
}

// declaresTest reports whether one of files declares the test function name.
func declaresTest(t *testing.T, files []string, name string) bool {
	t.Helper()
	for _, path := range files {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// stderrOf is the stderr an exec.ExitError carries, for a failure message.
func stderrOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return "\n" + strings.TrimSpace(string(exit.Stderr))
	}
	return ""
}

func isCall(call *ast.CallExpr, pkg, fn string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != fn {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg
}

// packageTestFiles maps each package of the module to the paths of the test
// files the go tool compiles for it, internal and external.
func packageTestFiles(t *testing.T, root string) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{.Dir}}|{{join .TestGoFiles \" \"}} {{join .XTestGoFiles \" \"}}", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./...: %v%s", err, stderrOf(err))
	}
	files := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("go list line %q: want import path|dir|files", line)
		}
		for _, name := range strings.Fields(parts[2]) {
			files[parts[0]] = append(files[parts[0]], filepath.Join(parts[1], name))
		}
	}
	return files
}

// importName is the name f refers to path by, or "" when f does not import it.
func importName(f *ast.File, path string) string {
	for _, spec := range f.Imports {
		if p, err := strconv.Unquote(spec.Path.Value); err != nil || p != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return filepath.Base(path)
	}
	return ""
}

// The guard's own witness: the TestMain shapes it accepts and the ones it
// must refuse.
// The strict check's own witness: the TestMain shapes that set the switch for
// good, and the ones that undo it or are not TestMain at all.
func TestTestMainSetsBeforeRunningReadsTheUndos(t *testing.T) {
	cases := []struct {
		name, decl string
		want       bool
	}{
		{"set", "func TestMain(m *testing.M) {\n\tos.Setenv(\"SW\", \"1\")\n\tos.Exit(m.Run())\n}", true},
		{"set in an if's init", "func TestMain(m *testing.M) {\n\tif err := os.Setenv(\"SW\", \"1\"); err != nil {\n\t\tpanic(err)\n\t}\n\tos.Exit(m.Run())\n}", true},
		{"wrong value", "func TestMain(m *testing.M) {\n\tos.Setenv(\"SW\", \"0\")\n\tos.Exit(m.Run())\n}", false},
		{"undone in the if's body", "func TestMain(m *testing.M) {\n\tif err := os.Setenv(\"SW\", \"1\"); err == nil {\n\t\tos.Unsetenv(\"SW\")\n\t}\n\tos.Exit(m.Run())\n}", false},
		{"reset in the else", "func TestMain(m *testing.M) {\n\tif err := os.Setenv(\"SW\", \"1\"); err != nil {\n\t\tpanic(err)\n\t} else {\n\t\tos.Setenv(\"SW\", \"\")\n\t}\n\tos.Exit(m.Run())\n}", false},
		{"cleared after", "func TestMain(m *testing.M) {\n\tos.Setenv(\"SW\", \"1\")\n\tos.Clearenv()\n\tos.Exit(m.Run())\n}", false},
		{"under a dead if", "func TestMain(m *testing.M) {\n\tif false {\n\t\tos.Setenv(\"SW\", \"1\")\n\t}\n\tos.Exit(m.Run())\n}", false},
		{"a method named TestMain", "type x struct{}\n\nfunc (x) TestMain(m *testing.M) {\n\tos.Setenv(\"SW\", \"1\")\n\tos.Exit(m.Run())\n}", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "package p\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" + c.decl + "\n"
			path := filepath.Join(t.TempDir(), "main_test.go")
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := testMainSetsBeforeRunning(t, []string{path}, map[string]string{"SW": "1"}); got != c.want {
				t.Errorf("testMainSetsBeforeRunning = %v, want %v for:\n%s", got, c.want, c.decl)
			}
		})
	}
}

func TestTestMainIsolatesReadsTheOrder(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"wraps", `os.Exit(pricingtest.Run(m.Run))`, true},
		{"wraps inside another isolation", `os.Exit(hometest.Isolate(func() int { return pricingtest.Run(m.Run) }))`, true},
		{"isolates first", "restore := pricingtest.Isolate()\n\tcode := m.Run()\n\trestore()\n\tos.Exit(code)", true},
		{"isolates after unrelated setup", "capParallelism()\n\trestore := pricingtest.Isolate()\n\tos.Exit(m.Run())\n\trestore()", true},
		{"isolates after running", "code := m.Run()\n\tpricingtest.Isolate()\n\tos.Exit(code)", false},
		{"runs something else", `os.Exit(pricingtest.Run(func() int { return 0 }))`, false},
		{"runs another value's tests", `os.Exit(pricingtest.Run(other.Run))`, false},
		{"never isolates", `os.Exit(m.Run())`, false},
		{"Run under an if that never runs", "if never {\n\t\tos.Exit(pricingtest.Run(m.Run))\n\t}\n\tos.Exit(m.Run())", false},
		{"Run in a closure nobody calls", "_ = func() int { return pricingtest.Run(m.Run) }\n\tos.Exit(m.Run())", false},
		{"Run after the tests ran", "code := m.Run()\n\t_ = pricingtest.Run(m.Run)\n\tos.Exit(code)", false},
		{"deferred Isolate", "defer pricingtest.Isolate()\n\tos.Exit(m.Run())", false},
		{"Isolate in a goroutine", "go pricingtest.Isolate()\n\tos.Exit(m.Run())", false},
		{"Isolate under an if that never runs", "if never {\n\t\tpricingtest.Isolate()\n\t}\n\tos.Exit(m.Run())", false},
		{"Isolate in a closure nobody calls", "f := func() { pricingtest.Isolate() }\n\t_ = f\n\tos.Exit(m.Run())", false},
		{"Run after an earlier argument ran the tests", "os.Exit(pick(m.Run(), pricingtest.Run(m.Run)))", false},
		{"m.Run first inside the wrapper's literal", "os.Exit(a.Wrap(func() int {\n\t\tm.Run()\n\t\treturn pricingtest.Run(m.Run)\n\t}))", false},
		{"tests run by the call's function expression first", "os.Exit(pickFn(m.Run())(pricingtest.Run(m.Run)))", false},
		{"Run nested in wrappers", "os.Exit(a.Wrap(func() int {\n\t\treturn b.Wrap(func() int { return pricingtest.Run(m.Run) })\n\t}))", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "package p\n\nimport (\n\t\"os\"\n\t\"testing\"\n\n\t\"github.com/SocialGouv/iterion/internal/pricingtest\"\n)\n\nfunc TestMain(m *testing.M) {\n\t" + c.body + "\n}\n"
			path := filepath.Join(t.TempDir(), "main_test.go")
			if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := testMainIsolates(t, []string{path}); got != c.want {
				t.Errorf("testMainIsolates = %v, want %v for:\n%s", got, c.want, c.body)
			}
		})
	}
}
