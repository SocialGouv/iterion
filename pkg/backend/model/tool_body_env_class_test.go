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

// The environment reading of a tool body is ToolBodyEnv's, and only its:
// three bodies go to a reader (a `command:`, a `script:`, a
// `postcondition:`) and each prepares its text at one chokepoint, so a
// declaration about the reading is honoured by every site at once instead of
// by N repeated guards (ADR-130, grep-la-classe). These static sweeps keep
// the class closed as the package grows — a fourth site that prepares a body
// its own way fails a test instead of shipping a body the author asked to
// keep verbatim and the engine rewrote anyway.
//
// The sweeps read the package's own non-test sources with go/parser and
// attribute every call to the function that encloses it: a call inside a
// closure belongs to the function that returns the closure, which is where a
// recipe's resolve() lives. A method is keyed by its receiver type, so two
// methods of one name never merge. A mention outside every function body —
// a package-level `var f = g` taking a function as a VALUE — is attributed
// to packageLevel.
//
// Nothing below enumerates the names that render a body. The anchor is the
// package's innermost renderers, and the set of doors to them is DERIVED
// from the parse: a shape the sweep was not written for falls on the side
// that fails, never on the side that passes.

// toolBodyEnvFamily is the raw braced-env expansion: the scanner and the two
// predicates it asks about each segment. A site that names any of them
// prepares a body outside the chokepoint.
var toolBodyEnvFamily = []string{
	"expandBracedEnv",
	"expandBracedEnvSteps",
	"bracedEnvWouldExpand",
	"resolveBracedEnvBody",
}

// rawExpanderAllowed lists the functions that may name the raw expansion,
// with the reason. Nothing else in this package reads a body's environment
// forms for itself.
var rawExpanderAllowed = map[string]string{
	"ToolBodyEnv":          "the chokepoint itself: every tool body the engine hands a reader reads its environment here",
	"expandBracedEnv":      "the engine's reading of a body by the body alone — the door the linearity oracle (#1393) and the C188 reader pin hold the scanner through",
	"expandBracedEnvSteps": "the scanner: it asks the two predicates about each segment it reads",
}

// chokepointCallers lists the functions that prepare an AUTHORED tool body,
// with the body each prepares. The set is pinned in both directions: a body
// preparer that stops calling the chokepoint reddens, and so does a new
// caller — text the author did not write is expanded by nobody (see
// TestSelfRepairCommandIsNotEnvExpanded).
var chokepointCallers = map[string]string{
	"ClawExecutor.shellRecipe":       "the `command:` body, before its templates resolve",
	"ClawExecutor.scriptRecipe":      "the `script:` body, before its templates resolve",
	"ClawExecutor.postconditionBody": "the `postcondition:` body, before its templates resolve",
}

// renderRoots are the package's innermost body renderers: every text a tool
// body becomes on its way to a reader goes through one of them. The sweep is
// anchored HERE and not on the wrappers, because a wrapper set is a list
// that a new door does not have to join — renderCommand and renderScript are
// reached today by four names, two unexported and two exported, and the
// exported pair is what a preview in another package calls.
var renderRoots = []string{"renderCommand", "renderScript"}

// renderPassThrough lists the functions that only FORWARD a body to a
// renderer, with the reason: the body reached them already read, so the
// sweep climbs past them to their own callers, which are the real body
// preparers. A name here that no longer forwards to a renderer is refused,
// and a NEW door to a renderer that is not named here must read the
// environment itself — the derived set is what makes the unenumerated shape
// fail.
var renderPassThrough = map[string]string{
	"resolveCommandTemplate": "the `command:` renderer wrapper: its callers are the body preparers this sweep holds",
	"resolveScriptTemplate":  "the `script:` renderer wrapper: its callers are the body preparers this sweep holds",
	"RenderCommand":          "the exported `command:` renderer a preview calls: its caller reads the environment, and a caller outside this package is held by the module-wide sweep in pkg/dryrun",
	"RenderScript":           "the exported `script:` renderer a preview calls: its caller reads the environment, and a caller outside this package is held by the module-wide sweep in pkg/dryrun",
}

// renderWithoutEnvAllowed lists the functions that may render through a body
// renderer WITHOUT reading the environment forms first, with the reason.
var renderWithoutEnvAllowed = map[string]string{
	"ClawExecutor.renderActionParams": "an action `params:` value is a connector argument, not a body an interpreter reads: no shell or interpreter environment answers it, and the engine expands nothing there",
}

// renderCall is one call to a body renderer: the renderer's name, the shape
// of the first argument (the body), and whether that argument IS the
// chokepoint's answer — a direct ToolBodyEnv call, or a name bound from one
// EARLIER in the same function. Any other shape is not the chokepoint's
// answer: the reading comes first or it is no defence, so an unrecognised
// provenance is refused rather than assumed sound.
type renderCall struct {
	callee string
	arg    string
	read   bool
}

// reading is one name bound from the chokepoint, with the position of the
// binding: a renderer handed a name bound AFTER it read the body after the
// substitution, which is the order the injection defence forbids.
type reading struct {
	name string
	pos  token.Pos
}

// packageCalls maps each function of this package's non-test files to the
// package-level functions it calls and to every name it mentions, records
// each body-renderer call with the provenance of the body it renders, and
// counts the declarations of each name so a rename cannot empty a sweep in
// silence.
type packageCalls struct {
	calls    map[string]map[string]bool
	mentions map[string]map[string]bool
	renders  map[string][]renderCall
	declared map[string]int
	files    int
}

func readPackageCalls(t *testing.T) *packageCalls {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	renderSet := map[string]bool{}
	for _, n := range renderRoots {
		renderSet[n] = true
	}
	for n := range renderPassThrough {
		renderSet[n] = true
	}
	pc := &packageCalls{
		calls:    map[string]map[string]bool{},
		mentions: map[string]map[string]bool{},
		renders:  map[string][]renderCall{},
		declared: map[string]int{},
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		pc.files++
		for _, d := range f.Decls {
			fn, isFunc := d.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				// A declaration outside every function body: a package-level
				// `var f = g` names g without calling it, which a sweep that
				// reads calls alone never sees.
				pc.noteMentions(packageLevel, d)
				continue
			}
			key := funcKey(fn)
			pc.declared[key]++
			pc.note(key, fn, renderSet)
		}
	}
	// A walk that stopped seeing the package would pass every sweep below.
	if pc.files < 40 {
		t.Fatalf("the walk parsed %d non-test files of this package — the sweeps' subject is not the package", pc.files)
	}
	return pc
}

// note records one function's calls, mentions and body-renderer calls.
func (pc *packageCalls) note(key string, fn *ast.FuncDecl, renderSet map[string]bool) {
	if pc.calls[key] == nil {
		pc.calls[key] = map[string]bool{}
	}
	var reads []reading
	var renders []*ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			// `body, reads := ToolBodyEnv(…)` — the first result is the body.
			if len(v.Rhs) == 1 && isChokepointCall(v.Rhs[0]) && len(v.Lhs) > 0 {
				if id, isIdent := v.Lhs[0].(*ast.Ident); isIdent && id.Name != "_" {
					reads = append(reads, reading{name: id.Name, pos: v.Pos()})
				}
			}
		case *ast.CallExpr:
			if id, isIdent := v.Fun.(*ast.Ident); isIdent {
				pc.calls[key][id.Name] = true
				if renderSet[id.Name] {
					renders = append(renders, v)
				}
			}
		case *ast.Ident:
			pc.mentions[key] = setAdd(pc.mentions[key], v.Name)
		}
		return true
	})
	for _, call := range renders {
		rc := renderCall{callee: call.Fun.(*ast.Ident).Name, arg: "no argument at all"}
		if len(call.Args) > 0 {
			rc.arg = exprLabel(call.Args[0])
			rc.read = isChokepointCall(call.Args[0])
			if id, isIdent := call.Args[0].(*ast.Ident); isIdent {
				for _, r := range reads {
					if r.name == id.Name && r.pos < call.Pos() {
						rc.read = true
						break
					}
				}
			}
		}
		pc.renders[key] = append(pc.renders[key], rc)
	}
}

// noteMentions records every name a declaration outside a function body uses.
func (pc *packageCalls) noteMentions(key string, d ast.Decl) {
	ast.Inspect(d, func(n ast.Node) bool {
		if id, isIdent := n.(*ast.Ident); isIdent {
			pc.mentions[key] = setAdd(pc.mentions[key], id.Name)
		}
		return true
	})
}

func setAdd(m map[string]bool, name string) map[string]bool {
	if m == nil {
		m = map[string]bool{}
	}
	m[name] = true
	return m
}

// isChokepointCall reports whether e is a call to ToolBodyEnv.
func isChokepointCall(e ast.Expr) bool {
	call, isCall := e.(*ast.CallExpr)
	if !isCall {
		return false
	}
	id, isIdent := call.Fun.(*ast.Ident)
	return isIdent && id.Name == "ToolBodyEnv"
}

// exprLabel names an expression as a failure message should read it.
func exprLabel(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprLabel(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		return exprLabel(v.Fun) + "(…)"
	case *ast.BasicLit:
		return v.Value
	}
	return "an expression this sweep does not read"
}

// funcKey names a function as the sweeps read it: `Receiver.Method` for a
// method, the bare name for a package-level function.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return recvTypeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
}

func recvTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // a generic receiver, Type[T]
		return recvTypeName(t.X)
	case *ast.IndexListExpr:
		return recvTypeName(t.X)
	}
	return "?"
}

// callersOf lists the functions that call any of names, sorted.
func (pc *packageCalls) callersOf(names ...string) []string {
	return pc.matching(pc.calls, names)
}

// mentionersOf lists the places that NAME any of names — a call, a function
// value, a package-level declaration — sorted.
func (pc *packageCalls) mentionersOf(names ...string) []string {
	return pc.matching(pc.mentions, names)
}

func (pc *packageCalls) matching(index map[string]map[string]bool, names []string) []string {
	var out []string
	for fn, used := range index {
		for _, n := range names {
			if used[n] {
				out = append(out, fn)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// requireDeclared fails when a name the sweeps are written around is no
// longer declared exactly once: a rename would otherwise leave every
// assertion below true of nothing.
func (pc *packageCalls) requireDeclared(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if pc.declared[n] != 1 {
			t.Fatalf("%s is declared %d times in this package, want 1 — a sweep written around it would prove nothing", n, pc.declared[n])
		}
	}
}

// The raw expansion is named by exactly the places the allowlist names: a
// site that prepares a tool body does it at the chokepoint. A MENTION counts,
// not only a call — a package-level `var f = expandBracedEnv` is a second
// reader whose call site names nothing.
func TestToolBodyEnvIsTheOnlyExpander(t *testing.T) {
	pc := readPackageCalls(t)
	pc.requireDeclared(t, append([]string{"ToolBodyEnv"}, toolBodyEnvFamily...)...)
	mentioners := pc.mentionersOf(toolBodyEnvFamily...)
	seen := map[string]bool{}
	for _, fn := range mentioners {
		seen[fn] = true
		if _, ok := rawExpanderAllowed[fn]; !ok {
			t.Errorf("%s names the raw braced-env expansion — prepare the body through ToolBodyEnv (which answers for the node's own declaration), or add an allowlist entry with its reason", fn)
		}
	}
	for fn, why := range rawExpanderAllowed {
		if !seen[fn] {
			t.Errorf("the allowlist still names %s (%q) but nothing there names the raw expansion — drop the entry, so the list keeps saying what is true", fn, why)
		}
	}
}

// Every function that renders a tool body read its environment forms FIRST,
// and the body it renders IS what the chokepoint answered. The order is the
// injection defence: a value substituted before the environment reading
// would have its own `$NAME` read back out of the single quotes
// shellEscapeValue put it in.
//
// The class is reached from the innermost renderers upward, through the
// declared pass-throughs, so a door opened to a renderer under a new name is
// found by the parse. A function the climb reaches that neither reads the
// environment nor carries a reason fails here.
//
// The climb grows by MENTIONS, not by calls: a door that takes a renderer as
// a function VALUE (`var f = resolveCommandTemplate`, `f := renderCommand`)
// calls nothing by name, and a frontier built from call expressions alone
// would never reach the body preparer behind the hop. A mention outside every
// function body is attributed to packageLevel, which has no caller to climb
// to and is refused where it names a renderer.
func TestEveryToolBodyPreparerGoesThroughTheChokepoint(t *testing.T) {
	pc := readPackageCalls(t)
	pc.requireDeclared(t, append([]string{"ToolBodyEnv"}, renderRoots...)...)
	pc.requireDeclared(t, sortedKeys(renderPassThrough)...)

	frontier := pc.mentionersOf(renderRoots...)
	if len(frontier) == 0 {
		t.Fatal("nothing in this package names a body renderer: the sweep's subject is gone and it proves nothing")
	}
	visited := map[string]bool{}
	for len(frontier) > 0 {
		fn := frontier[0]
		frontier = frontier[1:]
		if visited[fn] {
			continue
		}
		visited[fn] = true
		if fn == packageLevel {
			t.Errorf("a package-level declaration names a body renderer as a VALUE — the hop hides every function that reaches the renderer through it from this climb, so nothing asks those functions to read the environment first; call the renderer by name from a function that read the body at the chokepoint")
			continue
		}
		if _, ok := renderWithoutEnvAllowed[fn]; ok {
			continue
		}
		if _, ok := renderPassThrough[fn]; ok {
			frontier = append(frontier, pc.mentionersOf(fn)...)
			continue
		}
		if !pc.calls[fn]["ToolBodyEnv"] {
			t.Errorf("%s renders a tool body without reading its environment forms — call ToolBodyEnv on the authored text first (the expand-then-substitute order is the injection defence), or add an allowlist entry with its reason", fn)
			continue
		}
		for _, rc := range pc.renders[fn] {
			if !rc.read {
				t.Errorf("%s hands %s a body that is not the chokepoint's answer (%s) — render what ToolBodyEnv returned, read BEFORE this call: reading the environment after the substitution reads an upstream value's own `$NAME` back out of its single quotes", fn, rc.callee, rc.arg)
			}
		}
	}
	for fn, why := range renderPassThrough {
		if !visited[fn] {
			t.Errorf("renderPassThrough still names %s (%q) but nothing it forwards to is a body renderer the climb reaches — re-render the set from the sources", fn, why)
		}
	}
	for fn, why := range renderWithoutEnvAllowed {
		if !visited[fn] {
			t.Errorf("renderWithoutEnvAllowed still names %s (%q) but it no longer renders a tool body — drop the entry, so the list keeps saying what is true", fn, why)
		}
	}
	// The three authored bodies must still be among the preparers the climb
	// reaches: a body that stopped rendering here would leave the sweep above
	// true of a smaller class than the one it claims.
	for fn, body := range chokepointCallers {
		if !visited[fn] {
			t.Errorf("%s no longer renders %s through a body renderer — the sweep's class has moved; re-render it from the sources", fn, body)
		}
	}
}

// The authored-bytes boundary: the chokepoint's callers are exactly the
// three bodies the AUTHOR wrote. The recovery ladder's self-repair rung runs
// an LLM-proposed command (rung 3) and is expanded by nobody — the property
// is about the author's text, and a model's answer is not it.
func TestSelfRepairCommandIsNotEnvExpanded(t *testing.T) {
	pc := readPackageCalls(t)
	pc.requireDeclared(t, "ToolBodyEnv", "ClawExecutor.selfRepair")
	callers := pc.callersOf("ToolBodyEnv")
	seen := map[string]bool{}
	for _, fn := range callers {
		seen[fn] = true
		if fn == "ClawExecutor.selfRepair" {
			t.Errorf("the self-repair rung reads the environment forms of its command — it runs an LLM-proposed command, not the author's bytes, and crossing that boundary makes a model's `${NAME}` an engine lookup")
			continue
		}
		if _, ok := chokepointCallers[fn]; !ok {
			t.Errorf("%s prepares a tool body at the chokepoint — if that is an authored body, name it in chokepointCallers; if it is text the author did not write, it must not be expanded at all", fn)
		}
	}
	for fn, body := range chokepointCallers {
		if !seen[fn] {
			t.Errorf("%s no longer reads the environment forms of %s — the body reaches its reader with the engine's answer missing (or with an authored `${NAME}` the shell will answer instead)", fn, body)
		}
	}
}
