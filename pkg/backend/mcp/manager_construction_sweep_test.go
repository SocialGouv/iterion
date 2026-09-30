package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	// Two different reasons a call is exempt, counted separately: this
	// guard cannot READ its options (a spread, a pre-built option), or the
	// call is deliberately left UNARMED because that is the property under
	// test. One number for both could not tell them apart, and demanded a
	// remedy the file could not apply.
	type exemption struct {
		unreadable int
		unarmed    int
		reason     string
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
		"pkg/runview/executor.go": {unreadable: 1,
			reason: "options are spread; covered by runview's behavioural arming test instead"},
		// Exercises the zero value on purpose: that an UNARMED manager is
		// what SetSandbox(nil) has to open is the property under test. Two
		// calls: the unarmed one and the one the engine then opens.
		// ONE unarmed call (the zero-value premise); its sibling in the
		// same file arms itself inline and is judged normally.
		"pkg/backend/model/executor_mcp_start_policy_test.go": {unarmed: 1,
			reason: "asserts that an unarmed manager starts undecided"},
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
		exemptHere, isExempt := exempt[rel]
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		missing, gaveUp := managerBuiltWithoutAStartPolicy(file, mcpImportName(file))
		// An exemption covers the calls this guard cannot READ — never the
		// file. A readable call in an exempt file is judged like any other,
		// and the exemption's count must match the unreadable ones exactly,
		// so a second call cannot hide behind the first's reason. Counting
		// every call instead made an inline-armed second call unclearable:
		// the remedy the message named could not satisfy it.
		if isExempt {
			// An exemption covers a COUNT of calls, never the file. A third
			// call cannot hide behind the first two's reason.
			if missing != exemptHere.unarmed || gaveUp != exemptHere.unreadable {
				t.Errorf("%s is exempt for %d deliberately-unarmed and %d unreadable mcp.NewManager call(s) "+
					"(%s), and now has %d and %d — the exemption covers those calls, not the file; arm the "+
					"new one inline, or raise the count with the reason it deserves",
					rel, exemptHere.unarmed, exemptHere.unreadable, exemptHere.reason, missing, gaveUp)
			}
			return nil
		}
		if missing > 0 {
			offenders = append(offenders, rel)
		}
		if gaveUp > 0 {
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
func managerBuiltWithoutAStartPolicy(file *ast.File, mcpPkg string) (missing, gaveUp int) {
	if mcpPkg == "" {
		return 0, 0 // the file does not import this package at all
	}
	// A manager armed through the EXPORTED path counts as armed:
	// SetStartPolicy is documented as the engine's own arming call and is
	// how the executor does it. Demanding the option instead accused
	// correct code that uses the public API.
	//
	// Bound to the VALUE, over the WHOLE file. Keyed on the file alone, one
	// armed manager exempted every other NewManager beside it. Restricted
	// to function BODIES, a package-level `var m = mcp.NewManager(…)`
	// became invisible — a construction the previous predicate caught.
	// Keyed on a bare identifier, `h.mgr = mcp.NewManager(…)` followed by
	// `h.mgr.SetStartPolicy(…)` was accused of not arming, and a
	// SetStartPolicy method on an unrelated receiver armed everything.
	//
	// The printed expression of the assignment target is the key: it
	// distinguishes `m` in one function from `m` in another only when they
	// are, and it spells `h.mgr` the same way both sites do.
	armed := map[string]bool{}
	// A package-level target is armed from somewhere else BY CONSTRUCTION —
	// `init()`, or a setup function — so its arming can never share its
	// scope, and a scoped key alone accused `var m = mcp.NewManager(…)`
	// armed on the next line of `init()`. Scope-free keys are consulted for
	// package-level targets only, so a function's local `m` keeps needing
	// its own arming.
	armedAnyScope := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetStartPolicy" {
			return true
		}
		armed[scopedExprKey(file, sel.X)] = true
		armedAnyScope[types.ExprString(sel.X)] = true
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isMCPNewManagerCall(call.Fun, mcpPkg) || len(call.Args) == 0 {
			return true
		}
		// Every option arg is read BEFORE giving up: an inline
		// WithStartPolicy after a pre-built option used to be reported as
		// unreadable, so a correctly armed call had to be allowlisted.
		for _, arg := range call.Args[1:] {
			if c, ok := arg.(*ast.CallExpr); ok && calleeName(c.Fun) == "WithStartPolicy" {
				return true
			}
		}
		for _, arg := range call.Args[1:] {
			switch arg.(type) {
			case *ast.Ident, *ast.SelectorExpr:
				// A pre-built option value or a spread (`opts...`): the
				// file assembled its options elsewhere and this walk
				// cannot follow it. Give it up rather than guess — in a
				// gate a false accusation costs as much as a miss. But
				// SAY SO: a site that drifts into this shape is otherwise
				// invisible, no count and no list, and the header's
				// promise that "every site this guard exempts owes a
				// behavioural test" applies to nobody.
				gaveUp++
				return true
			}
		}
		key := managerTargetKey(file, call)
		if armed[key] {
			return true
		}
		if expr, isPkgLevel := strings.CutPrefix(key, packageScope+"\x00"); isPkgLevel && armedAnyScope[expr] {
			return true
		}
		missing++
		return true
	})
	return missing, gaveUp
}

// The predicate above is a BLOCKING gate, and until this table it was only
// ever run over the repository's own tree — where a false accusation shows up
// as a red build on correct code, and a miss shows up as nothing at all.
// Three of its shapes were wrong in exactly that invisible way: the arming
// restricted to function bodies, the arming keyed on a bare identifier, and
// the two below.
//
// Each case is source the guard must read a specific way, so a change to the
// keying reddens here instead of in someone's PR.
func TestTheSweepPredicateReadsTheShapesItClaims(t *testing.T) {
	for _, tc := range []struct {
		name            string
		src             string
		missing, gaveUp int
	}{
		{
			name: "armed inline by the option",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/mcp"
func f() { m := mcp.NewManager(nil, mcp.WithStartPolicy(mcp.StartAllServers)); _ = m }`,
		},
		{
			name: "armed by the engine's own call",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/mcp"
func f() { m := mcp.NewManager(nil); m.SetStartPolicy(mcp.StartAllServers) }`,
		},
		{
			name: "a struct field armed through the same spelling",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/mcp"
type h struct{ mgr *mcp.Manager }
func (x *h) f() { x.mgr = mcp.NewManager(nil); x.mgr.SetStartPolicy(mcp.StartAllServers) }`,
		},
		{
			// F3: the arming cannot share the scope of a package-level var.
			name: "a package-level var armed in init",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/mcp"
var m = mcp.NewManager(nil)
func init() { m.SetStartPolicy(mcp.StartAllServers) }`,
		},
		{
			name: "not armed at all",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/mcp"
func f() { m := mcp.NewManager(nil); _ = m }`,
			missing: 1,
		},
		{
			// F5: two same-named methods on different receivers must not
			// share a scope, or one's arming exempts the other.
			name: "same method name, different receivers",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/mcp"
type a struct{}
type b struct{}
func (a) setup() { m := mcp.NewManager(nil); m.SetStartPolicy(mcp.StartAllServers) }
func (b) setup() { m := mcp.NewManager(nil); _ = m }`,
			missing: 1,
		},
		{
			name: "SetStartPolicy on an unrelated receiver arms nothing",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/mcp"
func f(other thing) { m := mcp.NewManager(nil); _ = m; other.SetStartPolicy(1) }`,
			missing: 1,
		},
		{
			name: "a pre-built option is given up, not accused",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/backend/mcp"
func f(opts []mcp.Option) { m := mcp.NewManager(nil, opts...); _ = m }`,
			gaveUp: 1,
		},
		{
			name: "another package's NewManager is not ours",
			src: `package p
import "github.com/SocialGouv/iterion/pkg/dispatcher"
func f() { m := dispatcher.NewManager(nil); _ = m }`,
		},
		{
			name: "an aliased import is still ours",
			src: `package p
import xmcp "github.com/SocialGouv/iterion/pkg/backend/mcp"
func f() { m := xmcp.NewManager(nil); _ = m }`,
			missing: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			missing, gaveUp := managerBuiltWithoutAStartPolicy(file, mcpImportName(file))
			if missing != tc.missing {
				t.Errorf("missing = %d, want %d", missing, tc.missing)
			}
			if gaveUp != tc.gaveUp {
				t.Errorf("gaveUp = %d, want %d", gaveUp, tc.gaveUp)
			}
		})
	}
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

// managerTargetKey returns the key of whatever the given NewManager result is
// assigned to — a plain identifier, a struct field, whatever the source
// spells — or "" when the result is returned, discarded or passed straight
// on, in which case nothing can have armed it.
//
// Both an assignment (`m := …`, `h.mgr = …`) and a declaration
// (`var m = …`) count: only the first was read, so the `var` form was
// accused of not arming a manager it armed on the next line.
func managerTargetKey(file *ast.File, target *ast.CallExpr) string {
	key := ""
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range v.Rhs {
				if rhs == ast.Expr(target) && i < len(v.Lhs) {
					key = scopedExprKey(file, v.Lhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, val := range v.Values {
				if val == ast.Expr(target) && i < len(v.Names) {
					key = scopedExprKey(file, v.Names[i])
				}
			}
		}
		return true
	})
	return key
}

// packageScope is the scope of a construction with no enclosing function.
const packageScope = "<file>"

// scopedExprKey renders an expression as its source text, prefixed by the
// enclosing function so two functions that both call their manager `m` do
// not share an arming. A package-level construction has no enclosing
// function and gets the file's own scope.
//
// The function's POSITION identifies it, not its name: a file may hold two
// methods both called `setup` on different receivers, and a name let one's
// armed manager exempt the other's unarmed one.
func scopedExprKey(file *ast.File, expr ast.Expr) string {
	scope := packageScope
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		if expr.Pos() >= fn.Body.Lbrace && expr.End() <= fn.Body.Rbrace {
			scope = strconv.Itoa(int(fn.Pos()))
		}
		return true
	})
	return scope + "\x00" + types.ExprString(expr)
}
