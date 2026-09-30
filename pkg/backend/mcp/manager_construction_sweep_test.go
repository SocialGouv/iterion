package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Every Manager outside this package has to say which servers its launcher
// may start, because the zero value refuses the ones the operator did not
// install — the safe answer, and a silent behaviour change for a harness that
// meant "start everything".
//
// This guard exists because of WHERE such harnesses live: two of them are
// `//go:build live` e2e tests that cost real LLM money and that no CI job
// runs. A missing start policy there compiles, passes review, and is
// discovered months later by whoever next pays for a live run. The sweep
// parses every Go file in the repository, build tags included, so the live
// tags are covered by the free layer.
//
// What this guard cannot do: prove that a declared policy is the one the
// manager ends up holding. It reads shapes. A call that passes the option
// through a variable, a spread or a helper satisfies it while carrying
// nothing — which is what happened to the production site. Every site this
// guard exempts therefore owes a behavioural test instead.
//
// An entry in the allowlist is a decision on the record, not an exemption.
func TestEveryManagerOutsideThisPackageDeclaresItsStartPolicy(t *testing.T) {
	// An exemption names a file AND how many mcp.NewManager calls its reason
	// covers. A file is not a licence: an unarmed call added beside an
	// exempted one would otherwise be judged by nothing, and the exemption
	// would quietly grow to cover it.
	type exemption struct {
		calls  int
		reason string
	}
	exempt := map[string]exemption{
		// The one production constructor. Its options are assembled into a
		// slice and spread, which this guard cannot read — and Go forbids
		// mixing a positional option with a spread, so the shape cannot be
		// changed to suit it. What covers that site is a BEHAVIOURAL test,
		// runview.TestBuildMCPManagerArmsTheManagerWithItsPolicyArgument:
		// the policy it was given must be the policy the manager holds. Do
		// not replace that with a claim about this file's source — the
		// defect it caught was exactly a correct-looking shape whose value
		// went nowhere.
		"pkg/runview/executor.go": {1, "options are spread; covered by runview's behavioural arming test instead"},
		// Exercises the zero value on purpose: that an UNARMED manager is
		// what SetSandbox(nil) has to open is the property under test. Two
		// calls: the unarmed one and the one the engine then opens.
		"pkg/backend/model/executor_mcp_start_policy_test.go": {2, "asserts the zero value, then that the engine opens it"},
	}

	// Files whose option list this walk cannot read. Not exempt — invisible,
	// which is worse: the list is asserted below so a site drifting into
	// that shape reddens with its name instead of going quiet.
	unreadable := []string{}
	// Empty on purpose. The one file whose options are assembled and spread,
	// pkg/runview/executor.go, is in `exempt` above — with its reason and
	// the behavioural test that covers it — and `exempt` is consulted before
	// the file is parsed, so it never reaches the predicate. Anything that
	// lands here is a site nobody decided about.
	expectedUnreadable := map[string]bool{}

	repoRoot := filepath.Join("..", "..", "..")
	var offenders []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			// testdata is skipped because the toolchain skips it: a .go
			// file there is never compiled, so judging it would accuse
			// source that cannot run. runview's ExecutorSpec sweep walks
			// the same tree by the same rules; the two lists must say the
			// same thing or one of them is wrong and no reader can tell
			// which.
			case "vendor", "node_modules", ".git", ".iterion", "studio", "testdata":
				return filepath.SkipDir
			}
			// A nested checkout (a git worktree, a sibling clone an operator
			// keeps on disk) holds another tree's files: none are tracked
			// here, and its older copies would report as offenders of a rule
			// they predate.
			if path != repoRoot {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
		// This package's own tests exercise the zero value on purpose.
		if strings.HasPrefix(rel, "pkg/backend/mcp/") {
			return nil
		}
		if ex, ok := exempt[rel]; ok {
			if n := countMCPNewManagerCalls(path); n != ex.calls {
				t.Errorf("%s is exempt for %d mcp.NewManager call(s) (%s) and now holds %d — the exemption "+
					"covers those calls, not the file; arm the new one inline, or raise the count with the "+
					"reason it deserves", rel, ex.calls, ex.reason, n)
			}
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		missing, gaveUp := managerBuiltWithoutAStartPolicy(file, mcpImportName(file))
		if missing {
			offenders = append(offenders, rel)
		}
		if gaveUp {
			unreadable = append(unreadable, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these build an mcp.Manager without declaring which servers the launcher may start, so they "+
			"inherit the zero value (operator-installed servers only) — which silently refuses a "+
			"workflow-declared server: %v\nPass mcp.WithStartPolicy(…) (StartAllServers for a harness that "+
			"models an unsandboxed run) or call SetStartPolicy on the result, or add the file to this test's "+
			"exempt map with a reason.", offenders)
	}
	// The silence, made loud. A file whose options this walk cannot read is
	// not judged at all; that is the right call (a false accusation costs as
	// much as a miss) but it must be VISIBLE, or a site drifting into the
	// spread shape leaves the guard covering nothing and saying nothing.
	sort.Strings(unreadable)
	for _, rel := range unreadable {
		if !expectedUnreadable[rel] {
			t.Errorf("%s builds an mcp.Manager with an option list this guard cannot read (a pre-built option "+
				"value or a spread), so it is judged by nothing. Pass mcp.WithStartPolicy(…) inline, or add it "+
				"to expectedUnreadable together with the BEHAVIOURAL test that covers it instead.", rel)
		}
	}
	for rel := range expectedUnreadable {
		if !slices.Contains(unreadable, rel) {
			t.Errorf("%s is listed as unreadable but this guard can now read it — drop the entry so the file "+
				"is judged", rel)
		}
	}
}

// managerBuiltWithoutAStartPolicy reports whether the file calls NewManager
// without a WithStartPolicy argument. It reads the SOURCE rather than
// scanning text: `NewManager(` inside a comment or a string satisfies a grep,
// and a call split across lines defeats a line-oriented one.
func managerBuiltWithoutAStartPolicy(file *ast.File, mcpPkg string) (missing, gaveUp bool) {
	if mcpPkg == "" {
		return false, false // the file does not import this package at all
	}
	// A manager armed through the EXPORTED path counts as armed:
	// SetStartPolicy is documented as the engine's own arming call and is
	// how the executor does it. Demanding the option instead accused
	// correct code that uses the public API.
	//
	// Scoped per FUNCTION, because that is where a variable lives. Keyed on
	// the file, one armed manager exempted every other NewManager beside it
	// — and worse, two functions that both call their manager `m` shared
	// the arming, which is the shape every real file has. Keyed on nothing
	// but the name, a `SetStartPolicy` method on an unrelated receiver
	// armed it too.
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		armed := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SetStartPolicy" {
				return true
			}
			// Only on an identifier this function assigned a manager to —
			// checked below; here we just record what was armed.
			if id, ok := sel.X.(*ast.Ident); ok {
				armed[id.Name] = true
			}
			return true
		})
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || !isMCPNewManagerCall(call.Fun, mcpPkg) || len(call.Args) == 0 {
				return true
			}
			for _, arg := range call.Args[1:] {
				switch a := arg.(type) {
				case *ast.CallExpr:
					// The option may come from a helper, so any callee named
					// WithStartPolicy counts, qualified or not.
					if calleeName(a.Fun) == "WithStartPolicy" {
						return true
					}
				case *ast.Ident, *ast.SelectorExpr:
					// A pre-built option value or a spread (`opts...`): the
					// file assembled its options elsewhere and this walk
					// cannot follow it. Give it up rather than guess — in a
					// gate a false accusation costs as much as a miss. But
					// SAY SO: a site that drifts into this shape is
					// otherwise invisible, no count and no list, and the
					// header's promise that "every site this guard exempts
					// owes a behavioural test" applies to nobody.
					gaveUp = true
					return true
				}
			}
			if !armed[managerIdentOf(fn, call)] {
				missing = true
			}
			return true
		})
		return true
	})
	return missing, gaveUp
}

// isMCPNewManagerCall matches `<mcpPkg>.NewManager(…)`, where mcpPkg is the
// name THIS FILE imports pkg/backend/mcp under.
//
// Matching a bare `NewManager` ident was the first version and it was wrong:
// pkg/alert, pkg/dispatcher and pkg/runview each have a NewManager of their
// own, and the guard accused nine innocent files. Reading the file's imports
// also catches an aliased import, which a hardcoded "mcp" would miss.
func isMCPNewManagerCall(fun ast.Expr, mcpPkg string) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewManager" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == mcpPkg
}

// mcpImportName returns the local name under which the file imports
// pkg/backend/mcp, or "" when it does not import it.
func mcpImportName(file *ast.File) string {
	const path = `"github.com/SocialGouv/iterion/pkg/backend/mcp"`
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "mcp"
	}
	return ""
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// countMCPNewManagerCalls counts `<mcp>.NewManager(…)` calls in one file, so
// an exemption written for a single call cannot silently cover a second.
func countMCPNewManagerCalls(path string) int {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return -1
	}
	n := 0
	pkg := mcpImportName(file)
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && isMCPNewManagerCall(call.Fun, pkg) {
			n++
		}
		return true
	})
	return n
}

// managerIdentOf returns the identifier this function assigns the given
// NewManager result to, or "" when the result is returned, discarded or
// passed straight on — in which case nothing in this function can have
// armed it.
func managerIdentOf(fn *ast.FuncDecl, target *ast.CallExpr) string {
	name := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			if rhs != ast.Expr(target) || i >= len(as.Lhs) {
				continue
			}
			if id, ok := as.Lhs[i].(*ast.Ident); ok {
				name = id.Name
			}
		}
		return true
	})
	return name
}
